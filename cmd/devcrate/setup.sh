#!/bin/sh
# devcrate setup: runs on the VM inside `up`'s SSH session as
# `sh -s -- <origin> <devcontainer>`, where <devcontainer> is false for a
# VM-only Environment. Any failure exits non-zero without the Ready marker,
# which is written last.
set -eu
origin=$1
devcontainer=$2
state=/var/lib/devcrate
workspace=$state/workspace
# The devcontainer CLI lives on the root disk, like Docker: a snapshot image
# can carry both, and a fresh VM installs them.
cli_prefix=/opt/devcontainers
export PATH="$cli_prefix/bin:$PATH"

rm -f /run/devcrate/ready

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

# Forwarded env vars, pushed to tmpfs by devcrate just before setup. A
# Dockerfile's `--secret id=NAME` reads them from this environment.
creds=/run/devcrate/creds
. "$creds/env"

if [ "$devcontainer" = false ]; then
	echo "devcontainer = false: VM-only"
elif [ ! -f "$workspace/.devcontainer/devcontainer.json" ] && [ ! -f "$workspace/.devcontainer.json" ]; then
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
	# Lifecycle hooks get each forwarded var; the container gets the
	# (read-only) creds directory.
	set --
	while IFS= read -r name; do
		eval "value=\$$name"
		set -- "$@" --remote-env "$name=$value"
	done < "$creds/names"
	devcontainer up --workspace-folder "$workspace" --mount "type=bind,source=$creds,target=$creds" "$@"

	# The same sourcing and $HOME symlinks as on the VM, for the container user.
	id=$(docker ps -q --filter "label=devcontainer.local_folder=$workspace")
	docker exec -u 0 "$id" sh -c "mkdir -p /etc/profile.d && echo 'if [ -r $creds/env ]; then . $creds/env; fi' > /etc/profile.d/devcrate-creds.sh"
	devcontainer exec --workspace-folder "$workspace" sh -c "cd $creds/files && find . -type f | while IFS= read -r p; do p=\${p#./}; mkdir -p \"\$HOME/\$(dirname \"\$p\")\" && ln -sfn \"$creds/files/\$p\" \"\$HOME/\$p\"; done"
fi

mkdir -p /run/devcrate
touch /run/devcrate/ready
