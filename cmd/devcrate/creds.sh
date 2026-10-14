# devcrate credential push: runs on the VM as root on every `up` and `ssh`,
# with a tar of env, names and files/ on stdin. Everything stays on tmpfs.
set -eu
umask 077
creds=/run/devcrate/creds
# Written here; $creds is a read-only view of it, which the devcontainer
# mounts, so the container cannot change what the VM sources.
data=/run/devcrate/.creds

mkdir -p "$data" "$creds"
if ! mountpoint -q "$creds"; then
	mount --bind "$data" "$creds"
	mount -o remount,bind,ro "$creds"
fi
# Unique, so two concurrent pushes (devcrate ssh takes no lock) don't collide.
new=$(mktemp -d /run/devcrate/.creds-new.XXXXXX)
tar -x --no-same-owner -C "$new"
# Swap the contents, not the directory: bind mounts keep pointing at it.
rm -rf "$data/files"
mv "$new/files" "$new/names" "$new/env" "$data/"
rmdir "$new"

echo "if [ -r $creds/env ]; then . $creds/env; fi" > /etc/profile.d/devcrate-creds.sh
chmod 644 /etc/profile.d/devcrate-creds.sh
cd "$creds/files"
find . -type f | while IFS= read -r path; do
	path=${path#./}
	mkdir -p "$HOME/$(dirname "$path")"
	ln -sfn "$creds/files/$path" "$HOME/$path"
done
