package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// dialCmd connects stdin and stdout to a TCP target dialed from the
// devcontainer's network namespace, or the VM's when there is no
// devcontainer (ADR 0003). Usage: dial <host:port>.
//
// Joining the namespace re-execs the Agent under nsenter as
// `dial --in-ns <nameserver> <host:port>`, so the whole process is in it:
// Go resolves names on other threads than one that would setns. Names
// resolve through the container's nameserver (Docker's DNS, which knows
// compose services), passed in because the VM's /etc/resolv.conf is still
// the one Go reads.
func dialCmd(args []string) error {
	if len(args) == 3 && args[0] == "--in-ns" {
		if ns := args[1]; ns != "" {
			net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort(ns, "53"))
			}}
		}
		return pipeTo(args[2])
	}
	if len(args) != 1 {
		return errors.New("usage: devcrate-agent dial <host:port>")
	}
	pid, err := devcontainerPid()
	if err != nil {
		return err
	}
	if pid == "" {
		return pipeTo(args[0]) // VM-only
	}
	conf, _ := os.ReadFile("/proc/" + pid + "/root/etc/resolv.conf")
	self, err := os.Executable()
	if err != nil {
		return err
	}
	nsenter, err := exec.LookPath("nsenter")
	if err != nil {
		return err
	}
	return syscall.Exec(nsenter, []string{"nsenter", "-t", pid, "-n", "--", self, "dial", "--in-ns", nameserver(string(conf)), args[0]}, os.Environ())
}

// devcontainerPid is the running devcontainer's PID on the VM, or "" when
// the Environment has no devcontainer.
func devcontainerPid() (string, error) {
	ps, err := exec.Command("docker", "ps", "-a", "--filter", "label=devcontainer.local_folder="+volumeDir+"/workspace",
		"--format", "{{.ID}} {{.State}}").Output()
	if err != nil {
		return "", fmt.Errorf("looking for the devcontainer: %w", err)
	}
	id, err := pickDevcontainer(string(ps))
	if id == "" || err != nil {
		return "", err
	}
	pid, err := exec.Command("docker", "inspect", "-f", "{{.State.Pid}}", id).Output()
	if err != nil {
		return "", fmt.Errorf("inspecting the devcontainer: %w", err)
	}
	return strings.TrimSpace(string(pid)), nil
}

// pickDevcontainer picks the running devcontainer from `docker ps` lines of
// "<id> <state>": "" when there is none (VM-only), an error when none runs.
func pickDevcontainer(ps string) (string, error) {
	f := strings.Fields(ps)
	for i := 0; i+1 < len(f); i += 2 {
		if f[i+1] == "running" {
			return f[i], nil
		}
	}
	if len(f) >= 2 {
		return "", fmt.Errorf("the devcontainer is %s, not running; run devcrate up", f[1])
	}
	return "", nil
}

// nameserver is the first nameserver in a resolv.conf, or "".
func nameserver(conf string) string {
	for _, line := range strings.Split(conf, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" {
			return f[1]
		}
	}
	return ""
}

// pipeTo dials target, copies stdin to it and its replies to stdout until
// it closes.
func pipeTo(target string) error {
	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		io.Copy(conn, os.Stdin)
		conn.(*net.TCPConn).CloseWrite()
	}()
	_, err = io.Copy(os.Stdout, conn)
	return err
}
