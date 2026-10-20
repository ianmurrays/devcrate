# devcrate save-image runs this on a Ready VM before the Provider snapshots
# its root disk as the Base image (ADR 0009). It removes devcrate's own state,
# so a VM booted from the image behaves like a fresh instance and mounts its
# own volume. The volume is left alone: user data reinstalls the Agent from it.
# Idempotent, so a save that failed after it can be retried.
set -eu

# The Agent's whole root-disk footprint (devcrate-agent install).
systemctl disable -q --now devcrate-agent 2>/dev/null || true
rm -rf /usr/local/lib/devcrate /usr/local/bin/devcrate-agent /etc/systemd/system/devcrate-agent.service
systemctl daemon-reload

# Started before the volume is mounted, they would write under its mount
# point on the root disk. Setup starts them once it is.
systemctl disable -q docker docker.socket containerd

# cloud-init's per-instance state, so the next VM runs its own user data.
if command -v cloud-init >/dev/null 2>&1; then
	cloud-init clean --logs
else
	rm -rf /var/lib/cloud
fi

# The Provider's user data adds the next VM's own volume.
sed -i -E '\|^[^#[:space:]]+[[:space:]]+/var/lib/devcrate[[:space:]]|d' /etc/fstab

# The root disk's own /root/.ssh, under the volume's bind mount at /root.
root=$(mktemp -d)
mount --bind / "$root"
rm -rf "$root/root/.ssh"
umount "$root"
rmdir "$root"
