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
	// The name is the linked worktree's only.
	if got := e.statusName(); got != e.Name {
		t.Errorf("main worktree's Environment = %q, want its derived %q", got, e.Name)
	}
}

func TestUpNameWhileStoredEnvironmentExistsFails(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", e.Name+"-feat")
	y := w.Named(e.Name + "-y")
	x := w.Named(e.Name + "-x")
	if r := w.Devcrate("up", "--name", y.Name); r.Code != 0 {
		t.Fatalf("up --name %s exit %d", y.Name, r.Code)
	}

	r := w.Devcrate("up", "--name", x.Name)
	if want := "devcrate delete " + y.Name; r.Code != 1 || !strings.Contains(r.Stderr, want) {
		t.Errorf("up --name %s while %s exists = exit %d stderr %q, want exit 1 naming %q", x.Name, y.Name, r.Code, r.Stderr, want)
	}
	if strings.Contains(r.Stdout, "creating VM") || x.ContainerID() != "" || x.VolumeExists() {
		t.Errorf("up --name %s created a VM while %s exists", x.Name, y.Name)
	}
	if got := w.statusName(); got != y.Name {
		t.Errorf("after the refused up --name, the worktree's Environment = %q, want %q", got, y.Name)
	}

	if r := w.Devcrate("delete", "--yes", y.Name); r.Code != 0 {
		t.Fatalf("delete %s exit %d", y.Name, r.Code)
	}
	if r := w.Devcrate("up", "--name", x.Name); r.Code != 0 || !strings.Contains(r.Stdout, "ready: ssh devcrate-"+x.Name) {
		t.Fatalf("up --name %s after deleting %s = exit %d stdout %q, want Ready as %s", x.Name, y.Name, r.Code, r.Stdout, x.Name)
	}
	if r := w.Devcrate("up"); r.Code != 0 || !strings.Contains(r.Stdout, "ready: ssh devcrate-"+x.Name) {
		t.Errorf("bare up after replacing the name = exit %d stdout %q, want Ready as %s", r.Code, r.Stdout, x.Name)
	}
	if y.ContainerID() != "" || y.VolumeExists() {
		t.Errorf("bare up after replacing the name recreated %s", y.Name)
	}
	x.VM("echo x > /root/which")
	if r := w.Devcrate("ssh", "--", "cat /root/which"); r.Stdout != "x\n" {
		t.Errorf("bare ssh after replacing the name read %q exit %d, want %s's %q", r.Stdout, r.Code, x.Name, "x\n")
	}
}

func TestUpNameInMainWorktreeWinsOverRepoConfig(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	toml := "provider = \"fake\"\nname = \"" + e.Name + "-toml\"\n"
	e.WriteFile(".devcrate.toml", toml)
	x := e.Named(e.Name + "-x")
	w := e.Worktree("feat", e.Name+"-toml-feat")
	w.WriteFile(".devcrate.toml", toml)
	if r := e.Devcrate("up", "--name", x.Name); r.Code != 0 || !strings.Contains(r.Stdout, "ready: ssh devcrate-"+x.Name) {
		t.Fatalf("up --name %s in the main worktree = exit %d stdout %q, want Ready as %s", x.Name, r.Code, r.Stdout, x.Name)
	}
	if got := e.statusName(); got != x.Name {
		t.Errorf("main worktree's Environment after up --name = %q, want %q", got, x.Name)
	}
	// The name is the main worktree's only; a linked one keeps its derived name.
	if got := w.statusName(); got != w.Name {
		t.Errorf("linked worktree's Environment = %q, want its derived %q", got, w.Name)
	}
}

func TestUpInvalidNameFailsBeforeCreating(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	bad := e.Named("Bad_Name")
	r := e.Devcrate("up", "--name", bad.Name)
	if r.Code != 1 || !strings.Contains(r.Stderr, "invalid Environment name") {
		t.Errorf("up --name %s = exit %d stderr %q, want exit 1 for an invalid name", bad.Name, r.Code, r.Stderr)
	}
	if strings.Contains(r.Stdout, "creating VM") || e.ContainerID() != "" || e.VolumeExists() {
		t.Error("up created a VM for an invalid --name")
	}
	if got := e.statusName(); got != e.Name {
		t.Errorf("after an invalid --name, the Environment = %q, want %q", got, e.Name)
	}
}

func TestUpNameFallsBackToTheRepoNamesRepoFiles(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", e.Name+"-feat")
	x := w.Named(e.Name + "-x")
	e.UserConfig("[forward_repo_files]\n" + e.Name + " = [\"secret.key\"]\n")
	w.WriteFile("secret.key", "linked")
	if r := w.Devcrate("up", "--name", x.Name); r.Code != 0 {
		t.Fatalf("up --name %s exit %d", x.Name, r.Code)
	}
	if out, _ := x.VM("cat /var/lib/devcrate/workspace/secret.key"); out != "linked" {
		t.Errorf("%s's clone has secret.key = %q, want %q from the base name's entry", x.Name, out, "linked")
	}
}

func TestUpNameWithGitURLIsRejected(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	r := e.DevcrateIn(e.Home, "up", "--name", e.Name+"-x", gitBaseURL+"/"+e.Name+".git")
	if r.Code != 1 || !strings.Contains(r.Stderr, "--name") || !strings.Contains(r.Stderr, "git URL") {
		t.Errorf("up --name with a git URL = exit %d stderr %q, want exit 1 rejecting --name", r.Code, r.Stderr)
	}
}
