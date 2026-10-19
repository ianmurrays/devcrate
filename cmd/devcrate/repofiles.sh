# copy_repo_files copies the forward_repo_files on the tmpfs into the clone as
# real files (ADR 0007): 0600, owned by the clone's owner. Before the clone
# exists, it does nothing.
copy_repo_files() {
	repo_files=/run/devcrate/creds/repo
	repo_clone=/var/lib/devcrate/workspace
	if [ ! -d "$repo_clone/.git" ] || [ ! -d "$repo_files" ]; then
		return 0
	fi
	repo_owner=$(stat -c %u:%g "$repo_clone")
	(cd "$repo_files" && find . -type f) | while IFS= read -r repo_file; do
		repo_file=${repo_file#./}
		(umask 022 && mkdir -p "$repo_clone/$(dirname "$repo_file")")
		# Removed first, so a link at the path is replaced, not followed. A
		# linked parent directory is still followed: the repo is trusted with
		# root here anyway (ADR 0005).
		rm -f "$repo_clone/$repo_file"
		(umask 077 && cp "$repo_files/$repo_file" "$repo_clone/$repo_file")
		chown "$repo_owner" "$repo_clone/$repo_file"
	done
}
