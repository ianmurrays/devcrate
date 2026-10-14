package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// portsCmd is `devcrate forward`'s port detection. Usage: ports --watch.
//
// Every 2 s it lists the listening ports in the devcontainer's network
// namespace (the VM's when there is no devcontainer), re-resolving the
// container each time, and prints each change as a JSON line
// {"port": N, "open": true|false}. While the devcontainer is not running
// (a Rebuild, a restart) it has no ports. `ports --snapshot` prints the
// current ports once; --watch runs it under nsenter to read the
// namespace's own ephemeral range.
func portsCmd(args []string) error {
	if len(args) == 1 && args[0] == "--snapshot" {
		ports, err := snapshot()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(ports)
	}
	if len(args) != 1 || args[0] != "--watch" {
		return errors.New("usage: devcrate-agent ports --watch")
	}
	out := json.NewEncoder(os.Stdout)
	open := map[int]bool{}
	hadContainer := false
	for {
		now := map[int]bool{}
		for _, p := range detect(&hadContainer) {
			now[p] = true
		}
		for p := range open {
			if !now[p] {
				if err := out.Encode(portEvent{p, false}); err != nil {
					return err
				}
			}
		}
		for p := range now {
			if !open[p] {
				if err := out.Encode(portEvent{p, true}); err != nil {
					return err
				}
			}
		}
		open = now
		time.Sleep(2 * time.Second)
	}
}

type portEvent struct {
	Port int  `json:"port"`
	Open bool `json:"open"`
}

// detect lists the listening ports where `dial` would connect. Once a
// devcontainer has been seen, its absence means no ports rather than the
// VM's.
func detect(hadContainer *bool) []int {
	pid, err := devcontainerPid()
	if err != nil {
		*hadContainer = true // exists but is not running
		return nil
	}
	if pid == "" {
		if *hadContainer {
			return nil
		}
		ports, _ := snapshot()
		return ports
	}
	*hadContainer = true
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	b, err := exec.Command("nsenter", "-t", pid, "-n", "--", self, "ports", "--snapshot").Output()
	var ports []int
	if err != nil || json.Unmarshal(b, &ports) != nil {
		return nil
	}
	return ports
}

// snapshot is listening() for this process's network namespace.
func snapshot() ([]int, error) {
	rng, _ := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	var tables []io.Reader
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		defer f.Close()
		tables = append(tables, f)
	}
	return listening(ephemeralStart(string(rng)), tables...), nil
}

// ephemeralStart is the first port of an ip_local_port_range, or Linux's
// default when it cannot be read.
func ephemeralStart(rng string) int {
	if f := strings.Fields(rng); len(f) > 0 {
		if n, err := strconv.Atoi(f[0]); err == nil {
			return n
		}
	}
	return 32768
}

// forwardable local addresses in /proc/net/tcp{,6} format: 127.0.0.1,
// 0.0.0.0, ::1 and ::.
var forwardable = []string{"0100007F", "00000000", "00000000000000000000000001000000", "00000000000000000000000000000000"}

// listening lists, sorted and once each, the ports in /proc/net/tcp-format
// tables that listen on a loopback or wildcard address, from 1024 up to
// below ephemeral.
func listening(ephemeral int, tables ...io.Reader) []int {
	var ports []int
	for _, r := range tables {
		s := bufio.NewScanner(r)
		s.Scan() // header
		for s.Scan() {
			f := strings.Fields(s.Text())
			if len(f) < 4 || f[3] != "0A" { // 0A = LISTEN
				continue
			}
			addr, hex, _ := strings.Cut(f[1], ":")
			port, err := strconv.ParseInt(hex, 16, 32)
			if err != nil || !slices.Contains(forwardable, addr) || port < 1024 || int(port) >= ephemeral {
				continue
			}
			if !slices.Contains(ports, int(port)) {
				ports = append(ports, int(port))
			}
		}
	}
	slices.Sort(ports)
	return ports
}
