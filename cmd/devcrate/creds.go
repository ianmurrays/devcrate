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
var credsScript string

// pushCreds writes the current laptop values of the user config's forward_env,
// forward_env_commands, forward_files and git_signing_key to the VM's tmpfs,
// replacing what was there.
func pushCreds(client *ssh.Client) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	archive, err := cfg.credsArchive()
	if err != nil {
		return err
	}
	if err := remote.Run(client, "sh -c "+shellQuote(credsScript), archive, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("forwarding credentials: %w", err)
	}
	return nil
}

// signingGitConfig is the git config fragment that signs with git/signing_key.
// The system git config on the VM and in the devcontainer includes it
// (setup.sh), so a missing fragment means unsigned commits, not failing ones.
const signingGitConfig = "[gpg]\n\tformat = ssh\n[gpg \"ssh\"]\n\tprogram = ssh-keygen\n[user]\n\tsigningkey = /run/devcrate/creds/git/signing_key\n[commit]\n\tgpgsign = true\n[tag]\n\tgpgsign = true\n"

// credsArchive is a tar of env (sourceable exports), names (one forwarded
// var per line), files/<path relative to $HOME> and git/ (the signing key and
// signingGitConfig, or nothing).
func (c *config) credsArchive() (*bytes.Buffer, error) {
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
	return &buf, tw.Close()
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
