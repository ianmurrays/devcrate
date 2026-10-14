#!/bin/sh
# devcrate user data: the Provider runs this at boot, after mounting the
# persistent volume at /var/lib/devcrate. Nothing secret goes here.
set -eu
state=/var/lib/devcrate

# sshd host keys live on the volume, so the key devcrate pinned survives a
# resume onto a new VM. The first boot generates fresh ones to keep.
# (Generated aside: sshd may start at any moment and needs keys in /etc/ssh.)
if ! ls "$state"/ssh/ssh_host_*_key >/dev/null 2>&1; then
	rm -rf "$state/ssh.new"
	mkdir -p "$state/ssh.new/etc/ssh"
	ssh-keygen -A -f "$state/ssh.new"
	mv "$state/ssh.new/etc/ssh" "$state/ssh"
	rm -rf "$state/ssh.new"
fi
for key in "$state"/ssh/ssh_host_*; do
	name=$(basename "$key")
	cp -p "$key" "/etc/ssh/.$name" && mv "/etc/ssh/.$name" "/etc/ssh/$name"
done

# /root lives on the volume. The cloud has just put devcrate's key in the
# VM's own /root, so carry it over (another laptop has another key).
if ! mountpoint -q /root; then
	if [ ! -d "$state/home" ]; then
		cp -a /root "$state/home"
	fi
	mkdir -p "$state/home/.ssh"
	cat /root/.ssh/authorized_keys "$state/home/.ssh/authorized_keys" 2>/dev/null | sort -u > "$state/home/.ssh/authorized_keys.new" || true
	mv "$state/home/.ssh/authorized_keys.new" "$state/home/.ssh/authorized_keys"
	chmod 700 "$state/home" "$state/home/.ssh"
	chmod 600 "$state/home/.ssh/authorized_keys"
	mount --bind "$state/home" /root
fi

systemctl restart ssh 2>/dev/null || systemctl restart sshd

# The Agent `up` last pushed to the volume (none before the first `up`).
if [ -x "$state/agent/devcrate-agent" ]; then
	"$state/agent/devcrate-agent" install
fi
