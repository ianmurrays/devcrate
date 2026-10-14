package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ianmurrays/devcrate/internal/remote"
)

// forwardPort is one port devcrate forward listens on.
type forwardPort struct {
	target string // "3000" or "host:3000", dialed from the devcontainer
	local  int    // laptop port, on 127.0.0.1
	label  string // portsAttributes.label
}

func (p forwardPort) String() string {
	label := ""
	if p.label != "" {
		label = " (" + p.label + ")"
	}
	return fmt.Sprintf("%s%s -> localhost:%d", p.target, label, p.local)
}

// forwardPorts is the devcontainer's forwardPorts plus -p specs
// (<remote> or <local>:<remote>), labelled from portsAttributes. config is
// `devcontainer read-configuration` output, nil when there is none.
func forwardPorts(config []byte, specs []string) ([]forwardPort, error) {
	var cfg struct {
		Configuration struct {
			ForwardPorts    []any `json:"forwardPorts"`
			PortsAttributes map[string]struct {
				Label string `json:"label"`
			} `json:"portsAttributes"`
		} `json:"configuration"`
	}
	if config != nil {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("reading devcontainer.json: %w", err)
		}
	}
	var ports []forwardPort
	add := func(target string, local int) error {
		host, p, found := strings.Cut(target, ":")
		if !found {
			p = host
		}
		remotePort, err := strconv.Atoi(p)
		if err != nil || remotePort < 1 || remotePort > 65535 || (found && host == "") {
			return fmt.Errorf("%q is not a port or host:port", target)
		}
		if local == 0 {
			local = remotePort
		}
		label := cfg.Configuration.PortsAttributes[target].Label
		if label == "" {
			label = cfg.Configuration.PortsAttributes[p].Label
		}
		for i := range ports { // -p remaps a forwardPorts entry
			if ports[i].target == target {
				ports[i].local = local
				return nil
			}
		}
		ports = append(ports, forwardPort{target: target, local: local, label: label})
		return nil
	}
	for _, fp := range cfg.Configuration.ForwardPorts {
		target := fmt.Sprint(fp)
		if f, ok := fp.(float64); ok {
			target = strconv.Itoa(int(f))
		}
		if err := add(target, 0); err != nil {
			return nil, fmt.Errorf("forwardPorts: %w", err)
		}
	}
	for _, spec := range specs {
		local, target := 0, spec
		if l, rest, ok := strings.Cut(spec, ":"); ok {
			if n, err := strconv.Atoi(l); err == nil {
				if n < 1 || n > 65535 {
					return nil, fmt.Errorf("-p %s: %d is not a port", spec, n)
				}
				local, target = n, rest
			}
		}
		if err := add(target, local); err != nil {
			return nil, fmt.Errorf("-p %s: %w", spec, err)
		}
	}
	return ports, nil
}

// forward forwards ports from the running Environment to 127.0.0.1 until
// interrupted or the SSH connection drops. Each local connection is one SSH
// exec channel to the Agent's dial subcommand (ADR 0003). The open
// connection keeps the Environment from being Idle.
func forward(flags map[string]string, args []string) error {
	var specs []string
	if flags["-p"] != "" {
		specs = strings.Split(flags["-p"], ",")
	}
	if _, err := forwardPorts(nil, specs); err != nil { // fail before connecting
		return err
	}
	env, err := resolve(flags["--provider"], args)
	if err != nil {
		return err
	}
	res, err := env.running()
	if err != nil {
		return err
	}
	client, err := env.dial(res, 0)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", env.Name, err)
	}
	defer client.Close()
	if remote.Run(client, "test -f /run/devcrate/ready", nil, nil, nil) != nil {
		return fmt.Errorf("%s is running but not ready; run devcrate up in its repo", env.Name)
	}

	// The VM's copy of the repo is what runs, so its devcontainer.json counts.
	var config bytes.Buffer
	readConfig := `cd /var/lib/devcrate/workspace && { [ -f .devcontainer/devcontainer.json ] || [ -f .devcontainer.json ]; } &&
PATH=/opt/devcontainers/bin:$PATH && command -v devcontainer >/dev/null &&
devcontainer read-configuration --workspace-folder . < /dev/null`
	if err := remote.Run(client, readConfig, nil, &config, io.Discard); err != nil {
		config.Reset() // VM-only: no devcontainer.json or no CLI
	}
	var cfg []byte
	if config.Len() > 0 {
		cfg = config.Bytes()
	}
	ports, err := forwardPorts(cfg, specs)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return errors.New("nothing to forward: add forwardPorts to devcontainer.json or pass -p <port>")
	}

	for _, p := range ports {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p.local))
		if errors.Is(err, syscall.EADDRINUSE) {
			free := p.local + 1
			fmt.Fprintf(os.Stderr, "devcrate: localhost:%d is in use; not forwarding %s (pick another local port with -p %d:%s)\n", p.local, p.target, free, p.target)
			continue
		} else if err != nil {
			return err
		}
		fmt.Println(p)
		go serve(client, ln, p)
	}

	go keepalive(client)
	err = client.Wait()
	if err == nil {
		err = errors.New("closed by the VM")
	}
	return fmt.Errorf("connection to %s lost: %w", env.Name, err)
}

// serve pipes each connection on ln to p.target through the Agent.
func serve(client *ssh.Client, ln net.Listener, p forwardPort) {
	target := p.target
	if !strings.Contains(target, ":") {
		target = "localhost:" + target
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			if err := pipe(client, conn, target); err != nil {
				fmt.Fprintf(os.Stderr, "devcrate: %s: %v\n", p.target, err)
			}
		}()
	}
}

// pipe connects conn to `devcrate-agent dial target` on the VM.
func pipe(client *ssh.Client, conn net.Conn, target string) error {
	s, err := client.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	stdin, err := s.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	s.Stdout, s.Stderr = conn, &stderr
	if err := s.Start("/usr/local/lib/devcrate/devcrate-agent dial " + shellQuote(target)); err != nil {
		return err
	}
	go func() {
		io.Copy(stdin, conn)
		stdin.Close() // half-close: the Agent closes its write side too
	}()
	// Only the Agent's own failures (e.g. nothing listening) are worth
	// printing; a local client resetting the connection is routine.
	if err := s.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(msg)
		}
	}
	return nil
}

// keepalive closes the connection when the VM stops answering, so a dropped
// network ends forward instead of hanging it.
func keepalive(client *ssh.Client) {
	for range time.Tick(15 * time.Second) {
		done := make(chan error, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				continue
			}
		case <-time.After(30 * time.Second):
		}
		client.Close()
		return
	}
}
