#!/bin/sh
# devcrate setup: runs on the VM inside `up`'s SSH session as
# `sh -s -- <origin> <devcontainer> <setup_script> <in_container>`, where
# <devcontainer> is false for a VM-only Environment, <setup_script> is the
# repo's script (a path in the repo, or empty), and <in_container> is true to
# run the user's script in the devcontainer. devcrate pushes the user's script
# to /run/devcrate/user-setup.sh first, if there is one. Any failure exits
# non-zero without the Ready marker, which is written last.
set -eu
origin=$1
devcontainer=$2
setup_script=$3
in_container=$4
state=/var/lib/devcrate
workspace=$state/workspace
# The devcontainer CLI lives on the root disk, like Docker: a snapshot image
# can carry both, and a fresh VM installs them.
cli_prefix=/opt/devcontainers
export PATH="$cli_prefix/bin:$PATH"

rm -f /run/devcrate/ready

# Everything below also goes to setup.log on the volume (stdout and stderr
# stay separate on the way to devcrate). The log holds the latest setup only.
mkdir -p /run/devcrate
rm -f /run/devcrate/stdout /run/devcrate/stderr
mkfifo /run/devcrate/stdout /run/devcrate/stderr
: > "$state/setup.log"
tee -a "$state/setup.log" < /run/devcrate/stdout &
tee -a "$state/setup.log" < /run/devcrate/stderr >&2 &
exec > /run/devcrate/stdout 2> /run/devcrate/stderr
rm /run/devcrate/stdout /run/devcrate/stderr

# The Agent never counts the Environment Idle while this lock is held, and
# starts its idle clock when setup exits (the lock's mtime), however it exits.
# Scripts setup runs get 9>&-, so what they leave running cannot hold it.
exec 9>>/run/devcrate/setup.lock
flock 9
# Also let the tees drain before the session ends; the exit status is kept.
trap 'touch /run/devcrate/setup.lock; exec >&- 2>&-; wait' EXIT

# A sleeping laptop's session drops after about 3 minutes, so it stops
# holding the Environment awake.
sshd_conf=$(printf 'ClientAliveInterval 60\nClientAliveCountMax 3')
if [ "$(cat /etc/ssh/sshd_config.d/devcrate.conf 2>/dev/null)" != "$sshd_conf" ]; then
	mkdir -p /etc/ssh/sshd_config.d
	echo "$sshd_conf" > /etc/ssh/sshd_config.d/devcrate.conf
	systemctl reload ssh 2>/dev/null || systemctl reload sshd
fi

# at_least <version> <minimum>: dotted-version comparison.
at_least() {
	[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n 1)" = "$2" ]
}

# run_installer <url> [args]: downloads a shell installer, then runs it, so a
# failed download fails setup.
run_installer() {
	url=$1
	shift
	curl -fsSL -o /tmp/devcrate-installer.sh "$url"
	sh /tmp/devcrate-installer.sh "$@"
	rm -f /tmp/devcrate-installer.sh
}

version=$(dockerd --version 2>/dev/null | sed -n 's/^Docker version \([0-9.]*\).*/\1/p')
if [ -n "$version" ] && at_least "$version" 29; then
	echo "Docker $version is installed"
else
	echo "installing Docker…"
	run_installer https://get.docker.com
fi

# Docker's and containerd's storage live on the volume (ADR 0004), so images
# and the devcontainer survive a Stop. Restart them only when that changes.
restart=
# Only data-root is set in daemon.json; its other settings are kept. sed, as
# there may be no JSON tool on the VM.
daemon_json=/etc/docker/daemon.json
data_root="\"data-root\": \"$state/docker\""
mkdir -p /etc/docker
if grep -q '"data-root"' "$daemon_json" 2>/dev/null; then
	sed "s|\"data-root\"[[:space:]]*:[[:space:]]*\"[^\"]*\"|$data_root|" "$daemon_json"
elif grep -q '[^{}[:space:]]' "$daemon_json" 2>/dev/null; then
	sed "0,/{/s|{|{$data_root, |" "$daemon_json"
else
	echo "{$data_root}"
fi > "$daemon_json.new"
if cmp -s "$daemon_json.new" "$daemon_json"; then
	rm "$daemon_json.new"
else
	mv "$daemon_json.new" "$daemon_json"
	restart=1
fi
containerd_root="root = \"$state/containerd\""
if ! grep -qx "$containerd_root" /etc/containerd/config.toml 2>/dev/null; then
	mkdir -p /etc/containerd
	# A top-level key must come before any [table].
	{ echo "$containerd_root"; sed '/^root *=/d' /etc/containerd/config.toml 2>/dev/null || true; } > /etc/containerd/config.toml.new
	mv /etc/containerd/config.toml.new /etc/containerd/config.toml
	restart=1
fi
if [ -n "$restart" ]; then
	# docker.service is socket-activated; restart the socket with it.
	systemctl stop docker docker.socket containerd
	systemctl start containerd docker.socket docker
fi
# The containerd move is untested on real clouds; this is the guard.
docker_root=$(readlink -f "$(docker info -f '{{.DockerRootDir}}')")
containerd_root=$(readlink -f "$(containerd config dump 2>/dev/null | sed -n "s/^root = ['\"]\(.*\)['\"]$/\1/p")")
for root in "Docker's data-root:$docker_root" "containerd's root:$containerd_root"; do
	case ${root#*:} in
	"$state"/*) ;;
	*)
		echo "devcrate: ${root%%:*} is ${root#*:}, not under $state: images and the devcontainer would be lost at the next Stop" >&2
		exit 1
		;;
	esac
done

if [ ! -d "$workspace/.git" ]; then
	git clone "$origin" "$workspace"
elif [ "$(git -C "$workspace" remote get-url origin)" != "$origin" ]; then
	echo "devcrate: this Environment holds a clone of $(git -C "$workspace" remote get-url origin), not $origin: set name in .devcrate.toml to give this repo its own Environment" >&2
	exit 1
fi

if [ -n "$setup_script" ]; then
	echo "running $setup_script…"
	(cd "$workspace" && if [ -x "./$setup_script" ]; then "./$setup_script"; else sh "./$setup_script"; fi) < /dev/null 9>&-
fi

if [ "$devcontainer" = false ]; then
	in_container=false
	echo "devcontainer = false: VM-only"
elif [ ! -f "$workspace/.devcontainer/devcontainer.json" ] && [ ! -f "$workspace/.devcontainer.json" ]; then
	in_container=false
	echo "no devcontainer.json: VM-only"
else
	version=$(devcontainer --version 2>/dev/null || true)
	if [ -n "$version" ] && at_least "$version" 0.80.0; then
		echo "devcontainer CLI $version is installed"
	else
		echo "installing the devcontainer CLI…"
		rm -rf "$cli_prefix"
		run_installer https://raw.githubusercontent.com/devcontainers/cli/main/scripts/install.sh --prefix "$cli_prefix"
	fi
	# Starts the existing container if there is one; lifecycle hooks such as
	# postCreateCommand run only when it is created.
	# The Agent's SSH-agent relay: the container gets its directory and
	# lifecycle hooks get SSH_AUTH_SOCK (ssh --container sets it too).
	agent_dir=/run/devcrate/ssh-agent
	mkdir -p "$agent_dir"
	set -- --remote-env "SSH_AUTH_SOCK=$agent_dir/agent.sock"
	devcontainer up --workspace-folder "$workspace" --mount "type=bind,source=$agent_dir,target=$agent_dir" "$@" 9>&-
fi

# The scripts get /dev/null as stdin: setup's own stdin is the rest of this
# script.
user_script=/run/devcrate/user-setup.sh
if [ -f "$user_script" ]; then
	if [ "$in_container" = true ]; then
		echo "running ~/.config/devcrate/setup.sh in the devcontainer…"
		# "$@" is the --remote-env list from devcontainer up (in_container is
		# true only when that ran).
		devcontainer exec --workspace-folder "$workspace" "$@" sh -s < "$user_script" 9>&-
	else
		echo "running ~/.config/devcrate/setup.sh…"
		sh "$user_script" < /dev/null 9>&-
	fi
fi

mkdir -p /run/devcrate
touch /run/devcrate/ready
