//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// baseName is the base name `up` recorded on the Environment's volume.
func (e *Env) baseName() string {
	e.t.Helper()
	out, code := e.VM("cat /var/lib/devcrate/base-name")
	if code != 0 {
		e.t.Fatalf("reading %s's base-name: exit %d: %s", e.Name, code, out)
	}
	return out
}

func TestUpRecordsTheBaseEnvironmentName(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", e.Name+"-feat")
	n := e.Worktree("named", e.Name+"-named")
	x := n.Named(e.Name + "-x")

	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up in the main worktree exit %d", r.Code)
	}
	if got := e.baseName(); got != e.Name {
		t.Errorf("main worktree's base-name = %q, want %q", got, e.Name)
	}
	if r := w.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up in the linked worktree exit %d", r.Code)
	}
	if got := w.baseName(); got != e.Name {
		t.Errorf("linked worktree's base-name = %q, want the base Environment name %q", got, e.Name)
	}
	if r := n.Devcrate("up", "--name", x.Name); r.Code != 0 {
		t.Fatalf("up --name %s exit %d", x.Name, r.Code)
	}
	if got := x.baseName(); got != e.Name {
		t.Errorf("up --name %s's base-name = %q, want the base Environment name %q", x.Name, got, e.Name)
	}
}

func TestUpGitURLRecordsTheEnvironmentNameAsBaseName(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.DevcrateIn(e.Home, "up", gitBaseURL+"/"+e.Name+".git"); r.Code != 0 || !strings.Contains(r.Stdout, "ready: ssh devcrate-"+e.Name) {
		t.Fatalf("up <git-url> = exit %d stdout %q, want Ready as %s", r.Code, r.Stdout, e.Name)
	}
	if got := e.baseName(); got != e.Name {
		t.Errorf("up <git-url>'s base-name = %q, want %q", got, e.Name)
	}
}
