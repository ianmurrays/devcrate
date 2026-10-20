//go:build e2e

package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// Named returns e as an Env whose Name is name, the Environment `up --name
// name` in e's directory should create. Its VM and volume are removed after
// the test.
func (e *Env) Named(name string) *Env {
	e.t.Helper()
	n := *e
	n.Name = name
	e.t.Cleanup(n.destroy)
	return &n
}

func TestUpNameIsRememberedPerWorktree(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", e.Name+"-feat")
	x := w.Named(e.Name + "-x")

	r := w.Devcrate("up", "--name", x.Name)
	if r.Code != 0 || !strings.Contains(r.Stdout, "ready: ssh devcrate-"+x.Name) {
		t.Fatalf("up --name %s = exit %d stdout %q, want Ready as %s", x.Name, r.Code, r.Stdout, x.Name)
	}
	if w.ContainerID() != "" {
		t.Error("up --name also created the derived name's VM")
	}
	if r := w.Devcrate("up"); r.Code != 0 || !strings.Contains(r.Stdout, "ready: ssh devcrate-"+x.Name) {
		t.Errorf("bare up after up --name = exit %d stdout %q, want Ready as %s", r.Code, r.Stdout, x.Name)
	}
	x.VM("echo named > /root/which")
	if r := w.Devcrate("ssh", "--", "cat /root/which"); r.Stdout != "named\n" {
		t.Errorf("bare ssh after up --name read %q exit %d, want %s's %q", r.Stdout, r.Code, x.Name, "named\n")
	}
	if w.ContainerID() != "" {
		t.Error("a bare command created the derived name's VM")
	}
	out, err := exec.Command("git", "-C", w.Dir, "status", "--porcelain", "--ignored").Output()
	if err != nil || len(out) != 0 {
		t.Errorf("git status after up --name = %q %v, want a clean working tree", out, err)
	}
}
