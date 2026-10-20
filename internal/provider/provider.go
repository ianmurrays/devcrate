// Package provider runs Provider executables over the JSON verb contract
// (ADR 0001): one process per verb, one JSON object on stdin, JSON on stdout,
// stderr passed through, non-zero exit is failure.
package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// validName keeps a Provider name a plain word: with a slash, exec.LookPath
// would take devcrate-provider-<name> as a path, so a repo's .devcrate.toml
// could run a file from the repo.
var validName = regexp.MustCompile(`^[a-z0-9-]+$`)

// Request is the JSON object every verb receives on stdin.
type Request struct {
	EnvironmentID string         `json:"environment_id"`
	Options       map[string]any `json:"options"`
	SSHPublicKey  string         `json:"ssh_public_key"`
	UserData      string         `json:"user_data"`
	// BaseName is the repo's base Environment name, shared by its worktree
	// Environments: what its Base image is labelled with.
	BaseName string `json:"base_name"`
}

// State is an Environment's state as a Provider reports it.
type State string

const (
	Running  State = "running"
	Busy     State = "busy" // mid-transition, such as a Self-stop
	Stopped  State = "stopped"
	NotFound State = "not_found"
)

// Result is what `up` and `status` print on stdout.
type Result struct {
	State State  `json:"state"`
	Host  string `json:"host"`
	User  string `json:"user"`
	Port  int    `json:"port"`
	// SelfStopEnv (from `up` only) is the environment the self-stop script
	// runs with on the VM: the Self-stop credential.
	SelfStopEnv map[string]string `json:"self_stop_env,omitempty"`
}

// Provider is one discovered devcrate-provider-<name> executable.
type Provider struct {
	Name   string
	Path   string
	Stderr io.Writer // where the Provider's stderr goes; os.Stderr by default
}

// executableDir is the directory of the devcrate executable (symlinks
// resolved), where the Providers that ship with devcrate live.
var executableDir = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// Find looks for devcrate-provider-<name> on PATH, then in
// <configDir>/providers, then next to the devcrate executable (the Providers
// devcrate ships with).
func Find(name, configDir string) (*Provider, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("invalid provider name %q: use only lowercase letters, digits and -", name)
	}
	exe := "devcrate-provider-" + name
	if path, err := exec.LookPath(exe); err == nil {
		return &Provider{Name: name, Path: path, Stderr: os.Stderr}, nil
	}
	dirs := []string{filepath.Join(configDir, "providers")}
	if dir, err := executableDir(); err == nil {
		dirs = append(dirs, dir)
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, exe)
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return &Provider{Name: name, Path: path, Stderr: os.Stderr}, nil
		}
	}
	return nil, fmt.Errorf("provider %q not found: no %s on PATH, in %s or next to devcrate", name, exe, filepath.Join(configDir, "providers"))
}

func (p *Provider) Up(req Request) (Result, error)     { return p.result("up", req) }
func (p *Provider) Status(req Request) (Result, error) { return p.result("status", req) }

// Stop ends compute billing and keeps the disk. Its stdout is ignored.
func (p *Provider) Stop(req Request) error { return p.run("stop", req, nil) }

// Delete removes everything the Environment owns. Its stdout is ignored.
func (p *Provider) Delete(req Request) error { return p.run("delete", req, nil) }

// SelfStop returns the Provider's self-stop script, which devcrate copies to
// the VM for the Agent to run with self_stop_env sourced. The verb prints
// {"script": "..."}.
func (p *Provider) SelfStop(req Request) (string, error) {
	var out struct {
		Script string `json:"script"`
	}
	err := p.run("self-stop", req, &out)
	return out.Script, err
}

// Item is one entry of `list`.
type Item struct {
	EnvironmentID string `json:"environment_id"`
	State         State  `json:"state"`
}

// List returns every Environment this Provider knows about.
func (p *Provider) List() ([]Item, error) {
	var items []Item
	err := p.run("list", Request{}, &items)
	return items, err
}

func (p *Provider) result(verb string, req Request) (Result, error) {
	var res Result
	err := p.run(verb, req, &res)
	return res, err
}

func (p *Provider) run(verb string, req Request, out any) error {
	in, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var stdout bytes.Buffer
	cmd := exec.Command(p.Path, verb)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stdout = &stdout
	cmd.Stderr = p.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provider %s %s failed: %w", p.Name, verb, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("provider %s %s: invalid JSON on stdout: %w", p.Name, verb, err)
	}
	return nil
}
