#!/bin/sh
# devcrate setup: runs on the VM inside `up`'s SSH session as
# `sh -s -- <origin> <devcontainer> <setup_script> <in_container> <rebuild>`,
# where <devcontainer> is false for a VM-only Environment, <setup_script> is
# the repo's script (a path in the repo, or empty), <in_container> is true to
# run the user's script in the devcontainer and <rebuild> is true for
# `up --rebuild`. devcrate pushes the user's script
# to /run/devcrate/user-setup.sh first, if there is one. Any failure exits
# non-zero without the Ready marker, which is written last.
set -eu
origin=$1
devcontainer=$2
setup_script=$3
in_container=$4
rebuild=$5
state=/var/lib/devcrate
workspace=$state/workspace
# The devcontainer CLI lives on the root disk, like Docker: a snapshot image
# can carry both, and a fresh VM installs them.
cli_prefix=/opt/devcontainers
export PATH="$cli_prefix/bin:$PATH"

# Prints the Rebuild hash of the devcontainer config in $config (the output of
# `devcontainer read-configuration`): the fields that take effect when the
# image is built or the container is created, the Dockerfile and the CPU
# architecture. Build-context files, the FROM digest and secrets are not in it.
hash_js='
const path = require("path");
const c = JSON.parse(process.env.config).configuration;
const fields = ["image", "build", "dockerFile", "context", "features", "overrideFeatureInstallOrder",
	"dockerComposeFile", "service", "runServices", "workspaceMount", "workspaceFolder", "mounts",
	"runArgs", "appPort", "containerEnv", "containerUser", "remoteUser", "updateRemoteUserUID",
	"init", "privileged", "capAdd", "securityOpt"];
const sorted = v => Array.isArray(v) ? v.map(sorted)
	: v && typeof v === "object" ? Object.fromEntries(Object.keys(v).sort().map(k => [k, sorted(v[k])])) : v;
const picked = Object.fromEntries(fields.filter(f => f in c).map(f => [f, sorted(c[f])]));
const dockerfile = (c.build && c.build.dockerfile) || c.dockerFile;
const text = dockerfile
	? require("fs").readFileSync(path.resolve(path.dirname(c.configFilePath.fsPath), dockerfile), "utf8") : "";
const hash = require("crypto").createHash("sha256");
console.log(hash.update(JSON.stringify([picked, text, process.argv[1]])).digest("hex"));
'

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
	# postCreateCommand run only when it is created. The CLI never notices a
	# config change, so a changed Rebuild hash recreates the container (ADR 0004).
	config=$(devcontainer read-configuration --workspace-folder "$workspace" < /dev/null)
	# The node the CLI runs on (installed above), else one on PATH.
	node=$cli_prefix/node/current/bin/node
	[ -x "$node" ] || node=node
	hash=$(config=$config "$node" -e "$hash_js" "$(uname -m)" < /dev/null)
	set -- --workspace-folder "$workspace"
	old=$(cat "$state/devcontainer.hash" 2>/dev/null || true)
	if [ "$rebuild" = true ] || { [ -n "$old" ] && [ "$hash" != "$old" ]; }; then
		echo "rebuilding the devcontainer…"
		set -- "$@" --remove-existing-container
	fi
	devcontainer up "$@" < /dev/null 9>&-
	# Only after success, so a failed Rebuild is retried by the next up.
	echo "$hash" > "$state/devcontainer.hash"
fi

# The scripts get /dev/null as stdin: setup's own stdin is the rest of this
# script.
user_script=/run/devcrate/user-setup.sh
if [ -f "$user_script" ]; then
	if [ "$in_container" = true ]; then
		echo "running ~/.config/devcrate/setup.sh in the devcontainer…"
		devcontainer exec --workspace-folder "$workspace" sh -s < "$user_script" 9>&-
	else
		echo "running ~/.config/devcrate/setup.sh…"
		sh "$user_script" < /dev/null 9>&-
	fi
fi

mkdir -p /run/devcrate
touch /run/devcrate/ready
