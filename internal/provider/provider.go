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
)

// Request is the JSON object every verb receives on stdin.
type Request struct {
	EnvironmentID string         `json:"environment_id"`
	Options       map[string]any `json:"options"`
	SSHPublicKey  string         `json:"ssh_public_key"`
	UserData      string         `json:"user_data"`
}

// Result is what `up` and `status` print on stdout.
type Result struct {
	State string `json:"state"` // running | busy | stopped | not_found
	Host  string `json:"host"`
	User  string `json:"user"`
	Port  int    `json:"port"`
}

// Provider is one discovered devcrate-provider-<name> executable.
type Provider struct {
	Name   string
	Path   string
	Stderr io.Writer // where the Provider's stderr goes; os.Stderr by default
}

// Find looks for devcrate-provider-<name> on PATH, then in <configDir>/providers.
func Find(name, configDir string) (*Provider, error) {
	exe := "devcrate-provider-" + name
	if path, err := exec.LookPath(exe); err == nil {
		return &Provider{Name: name, Path: path, Stderr: os.Stderr}, nil
	}
	path := filepath.Join(configDir, "providers", exe)
	if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return &Provider{Name: name, Path: path, Stderr: os.Stderr}, nil
	}
	return nil, fmt.Errorf("provider %q not found: no %s on PATH or in %s", name, exe, filepath.Join(configDir, "providers"))
}

func (p *Provider) Up(req Request) (Result, error)     { return p.result("up", req) }
func (p *Provider) Status(req Request) (Result, error) { return p.result("status", req) }

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
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("provider %s %s: invalid JSON on stdout: %w", p.Name, verb, err)
	}
	return nil
}
