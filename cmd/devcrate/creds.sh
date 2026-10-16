# devcrate credential push: runs on the VM as root on every `up` and `ssh`,
# with a tar of env, names, files/ and git/ on stdin. Everything stays on tmpfs.
set -eu
umask 077
creds=/run/devcrate/creds
# Written here; $creds is a read-only view of it, which the devcontainer
# mounts, so the container cannot change what the VM sources.
data=/run/devcrate/.creds

mkdir -p "$data" "$creds"
# Traversable by everyone, so other users' git can read the include of
# git/config (git treats an unreadable include as fatal; only a missing one is
# skipped). Everything secret in it is 0600 or under files/ (0700).
chmod 711 /run/devcrate "$data"
if ! mountpoint -q "$creds"; then
	mount --bind "$data" "$creds"
	mount -o remount,bind,ro "$creds"
fi
# Unique, so two concurrent pushes (devcrate ssh takes no lock) don't collide.
new=$(mktemp -d /run/devcrate/.creds-new.XXXXXX)
tar -x --no-same-owner -C "$new"
# Swap the contents, not the directory: bind mounts keep pointing at it.
rm -rf "$data/files" "$data/git"
mv "$new/files" "$new/git" "$new/names" "$new/env" "$data/"
rmdir "$new"
# Owned by the devcontainer's remoteUser (setup records its uid:gid), so it
# can read them; still unreadable to anyone else. Root reads them regardless.
if [ -f /run/devcrate/creds-owner ]; then
	chown -R "$(cat /run/devcrate/creds-owner)" "$data"
fi

echo "if [ -r $creds/env ]; then . $creds/env; fi" > /etc/profile.d/devcrate-creds.sh
chmod 644 /etc/profile.d/devcrate-creds.sh
cd "$creds/files"
find . -type f | while IFS= read -r path; do
	path=${path#./}
	mkdir -p "$HOME/$(dirname "$path")"
	ln -sfn "$creds/files/$path" "$HOME/$path"
done
