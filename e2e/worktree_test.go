//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Worktree adds a linked git worktree of e's local clone in a sibling
// directory named basename, on a new branch of the same name, and returns
// it as an Env sharing e's HOME whose Name is name, the Environment the
// worktree should get. Its VM and volume are removed after the test.
func (e *Env) Worktree(basename, name string) *Env {
	e.t.Helper()
	w := &Env{t: e.t, Name: name, Home: e.Home, Dir: filepath.Join(filepath.Dir(e.Dir), basename), LaptopEnv: e.LaptopEnv}
	if err := sh(e.Dir, "git", "worktree", "add", "-q", "-b", basename, w.Dir); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(w.destroy)
	return w
}

// statusName is the Environment name `status --json` reports for e's
// directory.
func (e *Env) statusName() string {
	e.t.Helper()
	r := e.Devcrate("status", "--json")
	var st map[string]string
	if err := json.Unmarshal([]byte(r.Stdout), &st); err != nil {
		e.t.Fatalf("status --json = %q exit %d: %v", r.Stdout, r.Code, err)
	}
	return st["name"]
}

func TestLinkedWorktreeGetsItsOwnEnvironment(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", e.Name+"-feat")

	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up in the main worktree exit %d", r.Code)
	}
	if got := e.statusName(); got != e.Name {
		t.Errorf("main worktree's Environment = %q, want %q as before worktrees", got, e.Name)
	}
	r := w.Devcrate("up")
	if r.Code != 0 {
		t.Fatalf("up in the linked worktree exit %d", r.Code)
	}
	if !strings.Contains(r.Stdout, "ready: ssh devcrate-"+w.Name) {
		t.Errorf("up in the linked worktree stdout %q, want Ready as %s", r.Stdout, w.Name)
	}
	if w.ContainerID() == "" || e.ContainerID() == "" {
		t.Fatal("want a VM for each worktree")
	}

	r = e.Devcrate("list")
	for _, name := range []string{e.Name, w.Name} {
		if !regexp.MustCompile(`(?m)^` + name + ` +fake +running$`).MatchString(r.Stdout) {
			t.Errorf("list has no row %q fake running:\n%s", name, r.Stdout)
		}
	}

	// Bare commands in the linked worktree reach its Environment.
	w.VM("echo linked > /root/which")
	e.VM("echo main > /root/which")
	if r := w.Devcrate("ssh", "--", "cat /root/which"); r.Stdout != "linked\n" {
		t.Errorf("ssh in the linked worktree read %q exit %d, want its own VM's %q", r.Stdout, r.Code, "linked\n")
	}
	if r := w.Devcrate("keep-alive", "1m"); r.Code != 0 || !strings.Contains(r.Stdout, "kept alive until") {
		t.Errorf("keep-alive in the linked worktree = exit %d %q", r.Code, r.Stdout+r.Stderr)
	}
	if r := w.Devcrate("stop"); r.Code != 0 || !strings.Contains(r.Stdout, "stopped "+w.Name) {
		t.Errorf("stop in the linked worktree = exit %d %q, want it to stop %s", r.Code, r.Stdout, w.Name)
	}
	if w.ContainerID() != "" {
		t.Error("stop in the linked worktree left its VM running")
	}
	if e.ContainerID() == "" {
		t.Error("stop in the linked worktree stopped the main worktree's VM")
	}
	if r := w.Devcrate("status"); r.Stdout != "stopped\n" {
		t.Errorf("status in the linked worktree = %q, want stopped", r.Stdout)
	}
	// The main worktree's Environment is still running, so these reach the
	// linked worktree's only if they fail as stopped.
	stopped := w.Name + " is stopped"
	if r := w.Devcrate("keep-alive", "1m"); r.Code != 1 || !strings.Contains(r.Stderr, stopped) {
		t.Errorf("keep-alive in the stopped linked worktree = exit %d stderr %q, want %q", r.Code, r.Stderr, stopped)
	}
	if r := w.Devcrate("forward", "--no-detect", "-p", fmt.Sprintf("%d:22", freePort(t))); r.Code != 1 || !strings.Contains(r.Stderr, stopped) {
		t.Errorf("forward in the stopped linked worktree = exit %d stderr %q, want %q", r.Code, r.Stderr, stopped)
	}
	if r := w.Devcrate("delete", "--yes"); r.Code != 0 || !strings.Contains(r.Stdout, "deleted "+w.Name) {
		t.Errorf("delete in the linked worktree = exit %d %q, want it to delete %s", r.Code, r.Stdout, w.Name)
	}
	if w.VolumeExists() {
		t.Error("delete in the linked worktree left its volume")
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("main worktree's status after deleting the linked one = %q, want ready", r.Stdout)
	}
}

func TestLinkedWorktreeForwardsItsOwnRepoFiles(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", e.Name+"-feat")
	// forward_repo_files is keyed by Environment name.
	e.UserConfig("[forward_repo_files]\n" + w.Name + " = [\"secret.key\"]\n")
	e.WriteFile("secret.key", "main")
	w.WriteFile("secret.key", "linked")
	if r := w.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up in the linked worktree exit %d", r.Code)
	}
	if out, _ := w.VM("cat /var/lib/devcrate/workspace/secret.key"); out != "linked" {
		t.Errorf("the linked worktree's clone has secret.key = %q, want the linked worktree's %q", out, "linked")
	}
}

func TestUpInTwoWorktreesIsNotSerialised(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", e.Name+"-feat")
	release := e.holdLock() // as an up in the main worktree would
	defer release()
	if r := w.Devcrate("up"); r.Code != 0 || strings.Contains(r.Stderr, waitingForLock) {
		t.Errorf("up in the linked worktree while the main one's lock is held = exit %d stderr %q, want it to run without waiting", r.Code, r.Stderr)
	}
}

func TestLinkedWorktreeNamePrefixIsRepoConfigName(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", "my-app-feat")
	w.WriteFile(".devcrate.toml", "provider = \"fake\"\nname = \"my-app\"\n")
	if got := w.statusName(); got != "my-app-feat" {
		t.Errorf("linked worktree's Environment = %q, want %q", got, "my-app-feat")
	}
}

func TestLinkedWorktreeDirectoryNameIsSanitized(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("Fix_Login.2", e.Name+"-fix-login-2")
	if r := w.Devcrate("up"); r.Code != 0 || !strings.Contains(r.Stdout, "ready: ssh devcrate-"+w.Name) {
		t.Fatalf("up in worktree Fix_Login.2 = exit %d stdout %q, want Ready as %s", r.Code, r.Stdout, w.Name)
	}
}

func TestLinkedWorktreeNameTooLongFailsBeforeCreating(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	// e2e-xxxxxxxx (12) + - + 20 = 33 characters.
	w := e.Worktree("a-very-long-worktree", e.Name+"-a-very-long-worktree")
	r := w.Devcrate("up")
	if r.Code != 1 || !strings.Contains(r.Stderr, "--name") || !strings.Contains(r.Stderr, w.Name) {
		t.Errorf("up with a 33-character name = exit %d stderr %q, want exit 1 naming %s and pointing at --name", r.Code, r.Stderr, w.Name)
	}
	if strings.Contains(r.Stdout, "creating VM") || w.ContainerID() != "" || w.VolumeExists() {
		t.Error("up created a VM for an invalid name")
	}
}
