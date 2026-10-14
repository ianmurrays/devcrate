//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStopThenUpKeepsWorkspace(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	e.VM("echo kept > /var/lib/devcrate/workspace/scratch")

	// stdin is not a TTY here, so a prompt would fail or hang; stop never asks.
	r := e.Devcrate("stop")
	if r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	if e.ContainerID() != "" {
		t.Error("stop left the fake VM running")
	}
	if r := e.Devcrate("status"); r.Stdout != "stopped\n" {
		t.Errorf("status after stop = %q, want stopped", r.Stdout)
	}

	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up after stop exit %d", r.Code)
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status after resume = %q, want ready", r.Stdout)
	}
	if out, _ := e.VM("cat /var/lib/devcrate/workspace/scratch"); out != "kept\n" {
		t.Errorf("workspace file after resume = %q, want kept", out)
	}
}

func TestDeleteRemovesVMAndVolume(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}

	// Without a TTY, delete refuses unless --yes is given.
	r := e.Devcrate("delete")
	if r.Code != 1 || !strings.Contains(r.Stderr, "--yes") {
		t.Fatalf("delete without TTY = exit %d stderr %q, want exit 1 mentioning --yes", r.Code, r.Stderr)
	}
	if e.ContainerID() == "" {
		t.Fatal("delete without TTY removed the VM")
	}

	r = e.Devcrate("delete", "--yes")
	if r.Code != 0 {
		t.Fatalf("delete --yes exit %d", r.Code)
	}
	if e.ContainerID() != "" {
		t.Error("delete left the VM")
	}
	if e.VolumeExists() {
		t.Error("delete left the volume")
	}
	if r := e.Devcrate("status"); r.Code != 0 || r.Stdout != "missing\n" {
		t.Errorf("status after delete = %q exit %d, want missing", r.Stdout, r.Code)
	}
}

func TestDeletePromptsOnTTY(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	prompt := "Delete Environment " + e.Name + " and its volume? [y/N]"

	r := e.DevcrateTTY(prompt, "n\n", "delete")
	if r.Code != 1 || !strings.Contains(r.Stdout, prompt) {
		t.Fatalf("delete answered n = exit %d output %q, want exit 1 after %q", r.Code, r.Stdout, prompt)
	}
	if e.ContainerID() == "" || !e.VolumeExists() {
		t.Fatal("delete answered n removed the Environment")
	}

	r = e.DevcrateTTY(prompt, "y\n", "delete")
	if r.Code != 0 || !strings.Contains(r.Stdout, prompt) {
		t.Fatalf("delete answered y = exit %d output %q", r.Code, r.Stdout)
	}
	if e.ContainerID() != "" || e.VolumeExists() {
		t.Error("delete answered y left the VM or volume")
	}
}

func TestListAndJSON(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}

	r := e.Devcrate("list")
	if r.Code != 0 {
		t.Fatalf("list exit %d", r.Code)
	}
	header, _, _ := strings.Cut(r.Stdout, "\n")
	if !regexp.MustCompile(`^NAME +PROVIDER +STATE$`).MatchString(header) {
		t.Errorf("list header = %q, want NAME  PROVIDER  STATE", header)
	}
	// Other tests' Environments share the fake Provider, so look for ours.
	if !regexp.MustCompile(`(?m)^` + e.Name + ` +fake +running$`).MatchString(r.Stdout) {
		t.Errorf("list has no row %q fake running:\n%s", e.Name, r.Stdout)
	}

	r = e.Devcrate("list", "--json")
	var rows []map[string]string
	if err := json.Unmarshal([]byte(r.Stdout), &rows); err != nil {
		t.Fatalf("list --json = %q: %v", r.Stdout, err)
	}
	found := false
	for _, row := range rows {
		found = found || row["name"] == e.Name && row["provider"] == "fake" && row["state"] == "running"
	}
	if !found {
		t.Errorf("list --json has no {name: %s, provider: fake, state: running}: %s", e.Name, r.Stdout)
	}

	r = e.Devcrate("status", "--json")
	var st map[string]string
	if err := json.Unmarshal([]byte(r.Stdout), &st); err != nil || st["name"] != e.Name || st["state"] != "ready" {
		t.Errorf("status --json = %q (%v), want {name: %s, state: ready}", r.Stdout, err, e.Name)
	}
}

func TestCommandsByNameFromAnyDirectory(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	elsewhere := t.TempDir()

	// Outside a repo the name is required.
	if r := e.DevcrateIn(elsewhere, "status"); r.Code != 1 {
		t.Errorf("status outside a repo without a name = exit %d, want 1", r.Code)
	}

	// Outside a repo, the Providers to ask come from the user config.
	cfg := filepath.Join(e.Home, ".config", "devcrate")
	must(os.MkdirAll(cfg, 0o755))
	must(os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("[providers.fake]\n"), 0o644))

	if r := e.DevcrateIn(elsewhere, "status", e.Name); r.Code != 0 || r.Stdout != "ready\n" {
		t.Errorf("status %s elsewhere = %q exit %d, want ready", e.Name, r.Stdout, r.Code)
	}
	if r := e.DevcrateIn(elsewhere, "list"); !regexp.MustCompile(`(?m)^` + e.Name + ` +fake +running$`).MatchString(r.Stdout) {
		t.Errorf("list elsewhere has no row for %s:\n%s", e.Name, r.Stdout)
	}
	if r := e.DevcrateIn(elsewhere, "stop", e.Name); r.Code != 0 {
		t.Fatalf("stop %s elsewhere exit %d", e.Name, r.Code)
	}
	if r := e.DevcrateIn(elsewhere, "status", e.Name); r.Stdout != "stopped\n" {
		t.Errorf("status after stop elsewhere = %q, want stopped", r.Stdout)
	}
	if r := e.DevcrateIn(elsewhere, "delete", "--yes", e.Name); r.Code != 0 {
		t.Fatalf("delete %s elsewhere exit %d", e.Name, r.Code)
	}
	if r := e.DevcrateIn(elsewhere, "status", e.Name); r.Code != 0 || r.Stdout != "missing\n" {
		t.Errorf("status after delete elsewhere = %q exit %d, want missing", r.Stdout, r.Code)
	}
	if e.VolumeExists() {
		t.Error("delete elsewhere left the volume")
	}
}

func TestMutatingCommandsWaitForTheEnvironmentLock(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}

	// Hold the lock as another devcrate command would.
	dir := filepath.Join(e.Home, ".local", "state", "devcrate")
	must(os.MkdirAll(dir, 0o700))
	f, err := os.OpenFile(filepath.Join(dir, e.Name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	must(err)
	must(syscall.Flock(int(f.Fd()), syscall.LOCK_EX))

	cmd := e.Command("stop")
	must(cmd.Start())
	e.WaitOutput(cmd, "waiting for another devcrate command on "+e.Name)
	time.Sleep(time.Second)
	if e.ContainerID() == "" {
		t.Fatal("stop ran while another command held the lock")
	}

	f.Close() // releases the lock
	if r := e.Wait(cmd, []string{"stop"}); r.Code != 0 {
		t.Fatalf("stop after the lock was released exit %d", r.Code)
	}
	if e.ContainerID() != "" {
		t.Error("stop did not stop the Environment after taking the lock")
	}
}

func TestConcurrentUpAndStopAreSerialised(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)

	up := e.Command("up")
	must(up.Start())
	e.WaitOutput(up, "creating VM…")
	stop := e.Command("stop")
	must(stop.Start())
	e.WaitOutput(stop, "waiting for another devcrate command on "+e.Name)

	// up finishes untouched, then stop runs.
	if r := e.Wait(up, []string{"up"}); r.Code != 0 || !strings.Contains(r.Stdout, "ready: ssh devcrate-"+e.Name) {
		t.Fatalf("up exit %d, want Ready", r.Code)
	}
	if r := e.Wait(stop, []string{"stop"}); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	if r := e.Devcrate("status"); r.Stdout != "stopped\n" {
		t.Errorf("status = %q, want stopped", r.Stdout)
	}
}

func TestUpWaitsWhileBusy(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	// The fake reports busy while this file exists.
	busy := filepath.Join(t.TempDir(), "busy")
	must(os.WriteFile(busy, nil, 0o644))
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nbusy_while = \""+busy+"\"\n")

	if r := e.Devcrate("status"); r.Stdout != "busy\n" {
		t.Fatalf("status = %q, want busy", r.Stdout)
	}
	cmd := e.Command("up")
	must(cmd.Start())
	e.WaitOutput(cmd, e.Name+" is busy; waiting…")
	time.Sleep(time.Second)
	if e.ContainerID() != "" {
		t.Fatal("up created the VM while the Environment was busy")
	}

	must(os.Remove(busy))
	if r := e.Wait(cmd, []string{"up"}); r.Code != 0 {
		t.Fatalf("up after busy cleared exit %d", r.Code)
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status after up = %q, want ready", r.Stdout)
	}
}
