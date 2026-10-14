package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ianmurrays/devcrate/internal/provider"
	"github.com/ianmurrays/devcrate/internal/remote"
)

// sshCmd replaces devcrate with the system ssh, logged in to the VM as root,
// running `-- <cmd>` if given. It never starts the Environment.
func sshCmd(_ map[string]bool, args []string) error {
	var command []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, command = args[:i], args[i+1:]
	}
	env, err := resolve(args)
	if err != nil {
		return err
	}
	if env.provider == nil {
		return env.missing()
	}
	res, err := env.provider.Status(env.request())
	if err != nil {
		return err
	}
	switch res.State {
	case "running":
	case "stopped":
		return fmt.Errorf("%s is stopped; run devcrate up in its repo", env.Name)
	case "not_found":
		return fmt.Errorf("no Environment named %s", env.Name)
	default:
		return fmt.Errorf("%s is %s; try again shortly", env.Name, res.State)
	}
	// Connecting first pins the host key (on a laptop that has never run up
	// for it) and fails clearly when the VM is unreachable.
	client, err := env.dial(res, 0)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", env.Name, err)
	}
	client.Close()

	bin, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	// -F /dev/null: everything comes from here, never the user's ssh config.
	argv := []string{"ssh", "-F", "/dev/null", "-A",
		"-i", filepath.Join(dir, "id_ed25519"), "-o", "IdentitiesOnly=yes",
		"-o", "HostKeyAlias=" + env.alias(), "-o", "UserKnownHostsFile=" + filepath.Join(dir, "known_hosts"),
		"-p", strconv.Itoa(res.Port), "-l", res.User, "--", res.Host}
	return syscall.Exec(bin, append(argv, command...), os.Environ())
}

// alias is the Environment's SSH host alias and HostKeyAlias.
func (e *environment) alias() string { return "devcrate-" + e.Name }

// dial connects to the running VM res, checking its pinned host key.
func (e *environment) dial(res provider.Result, wait time.Duration) (*ssh.Client, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	return remote.Dial(res.Host, res.Port, res.User, e.key, filepath.Join(dir, "known_hosts"), e.alias(), wait)
}

// writeSSHConfig points the Environment's block in ~/.ssh/devcrate_config at
// the running VM res, and on first use includes that file from ~/.ssh/config.
func (e *environment) writeSSHConfig(res provider.Result) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	block := fmt.Sprintf("Host %s\n  HostName %s\n", e.alias(), res.Host)
	if res.Port != 22 {
		block += fmt.Sprintf("  Port %d\n", res.Port)
	}
	block += fmt.Sprintf("  User %s\n  HostKeyAlias %s\n  UserKnownHostsFile %s\n  IdentityFile %s\n  IdentitiesOnly yes\n  ForwardAgent yes\n",
		res.User, e.alias(), sshPath(filepath.Join(dir, "known_hosts")), sshPath(filepath.Join(dir, "id_ed25519")))
	if err := e.editSSHConfig(block); err != nil {
		return err
	}

	// ~/.ssh/config is the user's: one Include at the top, then never again.
	config := filepath.Join(home, ".ssh", "config")
	include := "Include " + sshPath(filepath.Join(home, ".ssh", "devcrate_config"))
	data, err := os.ReadFile(config)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == include {
			return nil
		}
	}
	if err := os.WriteFile(config, append([]byte(include+"\n"), data...), 0o600); err != nil {
		return err
	}
	fmt.Printf("added %q to the top of ~/.ssh/config\n", include)
	return nil
}

// removeSSHConfig drops the Environment's block and its pinned host key.
func (e *environment) removeSSHConfig() error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := e.editSSHConfig(""); err != nil {
		return err
	}
	return remote.Unpin(filepath.Join(dir, "known_hosts"), e.alias())
}

// editSSHConfig replaces the Environment's block in ~/.ssh/devcrate_config
// with block ("" removes it).
func (e *environment) editSSHConfig(block string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".ssh", "devcrate_config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Blocks run from their Host line to the next one.
	var out strings.Builder
	skip := false
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.HasPrefix(line, "Host ") {
			skip = strings.TrimSpace(line) == "Host "+e.alias()
		}
		if !skip {
			out.WriteString(line)
		}
	}
	out.WriteString(block)
	return os.WriteFile(path, []byte(out.String()), 0o600)
}

// sshPath spells path as ~/… when that means the same to ssh, which expands
// ~ from the passwd entry rather than $HOME.
func sshPath(path string) string {
	home, err := os.UserHomeDir()
	u, uerr := user.Current()
	if err != nil || uerr != nil || u.HomeDir != home {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return path
}

// configDir is ~/.config/devcrate.
func configDir() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".config", "devcrate"), err
}
