#!/bin/sh
# restore_from_volume <fresh|keep>: put the VM state that lives on the volume
# in place: sshd host keys and /root. User data runs it at boot and setup
# again on every up, as a VM that was powered on rather than created may not
# rerun user data. Idempotent. When the volume has no host keys yet, it
# generates fresh ones (fresh) or keeps those sshd serves now (keep, so the
# key devcrate has just pinned stays valid). Sets ssh_changed when /etc/ssh
# changed.
restore_from_volume() {
	vol=/var/lib/devcrate
	ssh_changed=
	# sshd host keys live on the volume, so the key devcrate pinned survives
	# a resume onto a new VM. (Prepared aside: sshd may start at any moment
	# and needs keys in /etc/ssh.)
	if ! ls "$vol"/ssh/ssh_host_*_key >/dev/null 2>&1; then
		rm -rf "$vol/ssh.new"
		mkdir -p "$vol/ssh.new/etc/ssh"
		if [ "$1" = keep ] && ls /etc/ssh/ssh_host_*_key >/dev/null 2>&1; then
			cp -p /etc/ssh/ssh_host_* "$vol/ssh.new/etc/ssh/"
		else
			ssh-keygen -A -f "$vol/ssh.new"
		fi
		rm -rf "$vol/ssh"
		mv "$vol/ssh.new/etc/ssh" "$vol/ssh"
		rm -rf "$vol/ssh.new"
	fi
	for key in "$vol"/ssh/ssh_host_*; do
		name=$(basename "$key")
		if ! cmp -s "$key" "/etc/ssh/$name"; then
			cp -p "$key" "/etc/ssh/.$name" && mv "/etc/ssh/.$name" "/etc/ssh/$name"
			ssh_changed=1
		fi
	done

	# /root lives on the volume. The cloud has put devcrate's key in the VM's
	# own /root, so carry it over (another laptop has another key), with the
	# Agent's ~/.ssh/rc if it is there.
	if ! mountpoint -q /root; then
		if [ ! -d "$vol/home" ]; then
			cp -a /root "$vol/home"
		fi
		mkdir -p "$vol/home/.ssh"
		cat /root/.ssh/authorized_keys "$vol/home/.ssh/authorized_keys" 2>/dev/null | sort -u > "$vol/home/.ssh/authorized_keys.new" || true
		mv "$vol/home/.ssh/authorized_keys.new" "$vol/home/.ssh/authorized_keys"
		if [ -f /root/.ssh/rc ]; then
			cp -p /root/.ssh/rc "$vol/home/.ssh/rc"
		fi
		chmod 700 "$vol/home" "$vol/home/.ssh"
		chmod 600 "$vol/home/.ssh/authorized_keys"
		mount --bind "$vol/home" /root
	fi
}

