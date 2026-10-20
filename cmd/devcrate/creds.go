package main

import (
	"archive/tar"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/ianmurrays/devcrate/internal/remote"
)

//go:embed creds.sh
var credsSh string

// pushCreds writes the current laptop values of the user config's forward_env,
// forward_env_commands, forward_files and git_signing_key to the VM's tmpfs,
// replacing what was there, and env's forward_repo_files to its clone.
func pushCreds(client *ssh.Client, env *environment) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	archive, err := cfg.credsArchive(env.Name, env.base, env.root)
	if err != nil {
		return err
	}
	if err := remote.Run(client, "sh -c "+shellQuote(repoFilesSh+credsSh), archive, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("forwarding credentials: %w", err)
	}
	return nil
}

// signingGitConfig is the git config fragment that signs with git/signing_key.
// The system git config on the VM and in the devcontainer includes it
// (setup.sh), so a missing fragment means unsigned commits, not failing ones.
const signingGitConfig = "[gpg]\n\tformat = ssh\n[gpg \"ssh\"]\n\tprogram = ssh-keygen\n[user]\n\tsigningkey = /run/devcrate/creds/git/signing_key\n[commit]\n\tgpgsign = true\n[tag]\n\tgpgsign = true\n"

// credsArchive is a tar of env (sourceable exports), names (one forwarded
// var per line), files/<path relative to $HOME>, git/ (the signing key and
// signingGitConfig, or nothing) and repo/<path in the repo>: the Environment
// name's forward_repo_files, else base's, from the laptop clone at root, or
// nothing when root is "".
func (c *config) credsArchive(name, base, root string) (*bytes.Buffer, error) {
	var env, names strings.Builder
	for _, name := range c.ForwardEnv {
		if !envName.MatchString(name) {
			return nil, fmt.Errorf("forward_env: %q is not an environment variable name", name)
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "devcrate: forward_env: %s is not set here; not forwarding it\n", name)
			continue
		}
		fmt.Fprintf(&env, "export %s=%s\n", name, shellQuote(value))
		fmt.Fprintln(&names, name)
	}
	for _, name := range slices.Sorted(maps.Keys(c.ForwardEnvCommands)) {
		if !envName.MatchString(name) {
			return nil, fmt.Errorf("forward_env_commands: %q is not an environment variable name", name)
		}
		cmd := exec.Command("sh", "-c", c.ForwardEnvCommands[name])
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "devcrate: forward_env_commands: the command for %s failed (%v); not forwarding it\n", name, err)
			continue
		}
		fmt.Fprintf(&env, "export %s=%s\n", name, shellQuote(strings.TrimSuffix(string(out), "\n")))
		fmt.Fprintln(&names, name)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	addMode := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	add := func(name string, data []byte) error { return addMode(name, 0o600, data) }
	if err := add("env", []byte(env.String())); err != nil {
		return nil, err
	}
	if err := add("names", []byte(names.String())); err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "files/", Mode: 0o700, Typeflag: tar.TypeDir}); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	for _, p := range c.ForwardFiles {
		rel, err := homeRelative("forward_files", home, p)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(home, rel))
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "devcrate: forward_files: %s does not exist here; not forwarding it\n", p)
			continue
		} else if err != nil {
			return nil, fmt.Errorf("forward_files: %w", err)
		}
		// tar creates the parent directories (0700, under creds.sh's umask).
		if err := add("files/"+filepath.ToSlash(rel), data); err != nil {
			return nil, err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "git/", Mode: 0o711, Typeflag: tar.TypeDir}); err != nil {
		return nil, err
	}
	if c.GitSigningKey != "" {
		rel, err := homeRelative("git_signing_key", home, c.GitSigningKey)
		if err != nil {
			return nil, err
		}
		key, err := os.ReadFile(filepath.Join(home, rel))
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "devcrate: git_signing_key: %s does not exist here; commits will not be signed\n", c.GitSigningKey)
		} else if err != nil {
			return nil, fmt.Errorf("git_signing_key: %w", err)
		} else if err := add("git/signing_key", key); err != nil {
			return nil, err
		} else if err := addMode("git/config", 0o644, []byte(signingGitConfig)); err != nil {
			return nil, err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "repo/", Mode: 0o700, Typeflag: tar.TypeDir}); err != nil {
		return nil, err
	}
	if root != "" {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		paths, ok := c.ForwardRepoFiles[name]
		if !ok { // a linked worktree uses its repo's entry (ADR 0008)
			paths = c.ForwardRepoFiles[base]
		}
		for _, p := range paths {
			rel := filepath.Clean(p) // checked by loadConfig
			// Links resolved, so a repo cannot point a listed path at a
			// laptop file outside it (ADR 0005).
			path, err := filepath.EvalSymlinks(filepath.Join(root, rel))
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "devcrate: forward_repo_files: %s does not exist here; not forwarding it\n", p)
				continue
			} else if err != nil {
				return nil, fmt.Errorf("forward_repo_files: %w", err)
			}
			if !inside(realRoot, path) {
				fmt.Fprintf(os.Stderr, "devcrate: forward_repo_files: %s is a link out of the repo; not forwarding it\n", p)
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("forward_repo_files: %w", err)
			}
			if err := add("repo/"+filepath.ToSlash(rel), data); err != nil {
				return nil, err
			}
		}
	}
	return &buf, tw.Close()
}

// inside reports whether path is dir or under it.
func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// checkRepoPath fails unless p, an entry of forward_repo_files, is a relative
// path inside the repo and outside .git.
func checkRepoPath(p string) error {
	rel := filepath.Clean(p)
	if filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return fmt.Errorf("forward_repo_files: %s is not a path inside the repo", p)
	}
	return nil
}

// homeRelative turns a path from the user config's key (~/x, x relative to
// $HOME, or an absolute path under $HOME) into a path relative to home.
func homeRelative(key, home, p string) (string, error) {
	path := strings.TrimPrefix(p, "~/")
	if !filepath.IsAbs(path) {
		path = filepath.Join(home, path)
	}
	rel, err := filepath.Rel(home, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s: %s is not a file under your home directory", key, p)
	}
	return rel, nil
}
