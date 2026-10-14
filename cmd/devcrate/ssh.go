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
	"golang.org/x/term"

	"github.com/ianmurrays/devcrate/internal/provider"
	"github.com/ianmurrays/devcrate/internal/remote"
)

// sshCmd replaces devcrate with the system ssh, logged in to the VM as root,
// running `-- <cmd>` if given. It never starts the Environment.
func sshCmd(flags map[string]string, args []string) error {
	var command []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, command = args[:i], args[i+1:]
	}
	env, err := resolve(flags["--provider"], args)
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
	err = printAgentError(client, false)
	tty := len(command) == 0 && term.IsTerminal(int(os.Stdin.Fd()))
	if err == nil && flags["--container"] != "" {
		command, err = env.containerCommand(client, command, tty)
	}
	client.Close()
	if err != nil {
		return err
	}

	bin, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	// -F /dev/null: everything comes from here, never the user's ssh config.
	argv := []string{"ssh", "-F", "/dev/null", "-A"}
	if tty {
		argv = append(argv, "-t") // ssh would not allocate one for a command
	}
	argv = append(argv,
		"-i", filepath.Join(dir, "id_ed25519"), "-o", "IdentitiesOnly=yes",
		"-o", "HostKeyAlias="+env.alias(), "-o", "UserKnownHostsFile="+filepath.Join(dir, "known_hosts"),
		"-p", strconv.Itoa(res.Port), "-l", res.User, "--", res.Host)
	return syscall.Exec(bin, append(argv, command...), os.Environ())
}

// containerCommand wraps command (a login shell when empty) in a docker exec
// into the Environment's running devcontainer. With no running devcontainer
// it fails rather than fall back to the VM.
func (e *environment) containerCommand(client *ssh.Client, command []string, tty bool) ([]string, error) {
	var out strings.Builder
	find := "docker ps -a --filter label=devcontainer.local_folder=/var/lib/devcrate/workspace --format '{{.ID}} {{.State}}'"
	if err := remote.Run(client, find, nil, &out, os.Stderr); err != nil {
		return nil, fmt.Errorf("looking for %s's devcontainer: %w", e.Name, err)
	}
	id, state, _ := strings.Cut(strings.TrimSpace(out.String()), " ")
	switch {
	case id == "":
		return nil, fmt.Errorf("%s has no devcontainer; use devcrate ssh without --container for the VM", e.Name)
	case state != "running":
		return nil, fmt.Errorf("%s's devcontainer is %s, not running; run devcrate up in its repo", e.Name, state)
	}
	script := "if command -v bash >/dev/null; then exec bash -l; fi; exec sh -l"
	if len(command) > 0 {
		script = strings.Join(command, " ") // as ssh would hand it to the VM's shell
	}
	flags := "-i"
	if tty {
		flags = "-it"
	}
	return []string{"docker", "exec", flags, id, "sh", "-c", shellQuote(script)}, nil
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
