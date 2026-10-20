//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// git runs git in the local clone, failing the test on error.
func (e *Env) git(args ...string) {
	e.t.Helper()
	args = append([]string{"-c", "user.email=e2e@devcrate", "-c", "user.name=e2e"}, args...)
	if err := sh(e.Dir, "git", args...); err != nil {
		e.t.Fatal(err)
	}
}

// pushBranch commits the working tree on a new branch and pushes it to the
// bare repo the VM clones.
func (e *Env) pushBranch(branch string) {
	e.t.Helper()
	bare := filepath.Join(gitRoot, e.Name+".git")
	e.git("checkout", "-q", "-b", branch)
	e.WriteFile(strings.ReplaceAll(branch, "/", "-")+".txt", branch+"\n")
	e.git("add", "-A")
	e.git("commit", "-q", "-m", branch)
	e.git("push", "-q", bare, "HEAD:"+branch)
}

// vmBranch is the branch checked out in the Environment's clone.
func (e *Env) vmBranch() string {
	e.t.Helper()
	out, code := e.VM("git -C /var/lib/devcrate/workspace rev-parse --abbrev-ref HEAD")
	if code != 0 {
		e.t.Fatalf("reading the VM's branch: %s", out)
	}
	return strings.TrimSpace(out)
}

func TestNewEnvironmentChecksOutPushedBranch(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.pushBranch("feature/login")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	} else if strings.Contains(r.Stderr, "warning") {
		t.Errorf("up on a pushed branch warned: %q", r.Stderr)
	}
	if b := e.vmBranch(); b != "feature/login" {
		t.Errorf("VM branch = %q, want feature/login", b)
	}
	if out, _ := e.VM("cat /var/lib/devcrate/workspace/feature-login.txt"); out != "feature/login\n" {
		t.Errorf("branch file on the VM = %q, want the branch's commit checked out", out)
	}

	// An existing clone is never switched, so work on the VM is never disturbed.
	e.pushBranch("other")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("second up exit %d", r.Code)
	}
	if b := e.vmBranch(); b != "feature/login" {
		t.Errorf("VM branch after up from another branch = %q, want feature/login unchanged", b)
	}
}

func TestNewEnvironmentStaysOnDefaultBranch(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup   func(e *Env)
		warning string
	}{
		"unpushed branch": {
			setup:   func(e *Env) { e.git("checkout", "-q", "-b", "local-only") },
			warning: "local-only",
		},
		"unborn branch": {
			setup:   func(e *Env) { e.git("checkout", "-q", "--orphan", "unborn") },
			warning: "unborn",
		},
		"detached HEAD": {
			setup:   func(e *Env) { e.git("checkout", "-q", "--detach") },
			warning: "detached HEAD",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			tc.setup(e)
			r := e.Devcrate("up")
			if r.Code != 0 {
				t.Fatalf("up exit %d", r.Code)
			}
			if !strings.Contains(r.Stderr, "warning") || !strings.Contains(r.Stderr, tc.warning) {
				t.Errorf("up stderr = %q, want a warning naming %q", r.Stderr, tc.warning)
			}
			if b := e.vmBranch(); b != "main" {
				t.Errorf("VM branch = %q, want the default branch main", b)
			}
		})
	}
}
