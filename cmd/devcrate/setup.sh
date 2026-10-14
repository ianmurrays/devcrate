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
# Let the tees drain before the session ends; the exit status is kept.
trap 'exec >&- 2>&-; wait' EXIT

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
daemon_json="{\"data-root\": \"$state/docker\"}"
if [ "$(cat /etc/docker/daemon.json 2>/dev/null)" != "$daemon_json" ]; then
	mkdir -p /etc/docker
	echo "$daemon_json" > /etc/docker/daemon.json
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

# Forwarded env vars, pushed to tmpfs by devcrate just before setup. Both
# setup scripts see them, and a Dockerfile's `--secret id=NAME` reads them
# from this environment.
creds=/run/devcrate/creds
. "$creds/env"

if [ -n "$setup_script" ]; then
	echo "running $setup_script…"
	(cd "$workspace" && if [ -x "./$setup_script" ]; then "./$setup_script"; else sh "./$setup_script"; fi) < /dev/null
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
	# postCreateCommand run only when it is created. They get each forwarded
	# var; the container gets the (read-only) creds directory.
	set --
	while IFS= read -r name; do
		eval "value=\$$name"
		set -- "$@" --remote-env "$name=$value"
	done < "$creds/names"
	devcontainer up --workspace-folder "$workspace" --mount "type=bind,source=$creds,target=$creds" "$@" < /dev/null

	# The same sourcing and $HOME symlinks as on the VM, for the container user.
	id=$(docker ps -q --filter "label=devcontainer.local_folder=$workspace")
	docker exec -u 0 "$id" sh -c "mkdir -p /etc/profile.d && echo 'if [ -r $creds/env ]; then . $creds/env; fi' > /etc/profile.d/devcrate-creds.sh"
	devcontainer exec --workspace-folder "$workspace" sh -c "cd $creds/files && find . -type f | while IFS= read -r p; do p=\${p#./}; mkdir -p \"\$HOME/\$(dirname \"\$p\")\" && ln -sfn \"$creds/files/\$p\" \"\$HOME/\$p\"; done" < /dev/null
fi

# The scripts get /dev/null as stdin: setup's own stdin is the rest of this
# script.
user_script=/run/devcrate/user-setup.sh
if [ -f "$user_script" ]; then
	if [ "$in_container" = true ]; then
		echo "running ~/.config/devcrate/setup.sh in the devcontainer…"
		# Its login-shell env probe sources the forwarded env vars.
		devcontainer exec --workspace-folder "$workspace" sh -s < "$user_script"
	else
		echo "running ~/.config/devcrate/setup.sh…"
		sh "$user_script" < /dev/null
	fi
fi

mkdir -p /run/devcrate
touch /run/devcrate/ready
