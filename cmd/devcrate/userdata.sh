# devcrate user data: the Provider runs this at boot, after mounting the
# persistent volume at /var/lib/devcrate. Nothing secret goes here. devcrate
# sends it after volume.sh.
set -eu
volume=/var/lib/devcrate

# The first boot generates fresh host keys to keep.
restore_from_volume fresh
systemctl restart ssh 2>/dev/null || systemctl restart sshd

# The Agent `up` last pushed to the volume (none before the first `up`).
if [ -x "$volume/agent/devcrate-agent" ]; then
	"$volume/agent/devcrate-agent" install
fi
