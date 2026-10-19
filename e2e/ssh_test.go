//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHRunsCommandAsRoot(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}

	if r := e.Devcrate("ssh", e.Name, "--", "whoami"); r.Code != 0 || r.Stdout != "root\n" {
		t.Errorf("ssh -- whoami = %q exit %d, want root exit 0", r.Stdout, r.Code)
	}
	// The remote command's exit code is devcrate's.
	if r := e.Devcrate("ssh", "--", "exit", "7"); r.Code != 7 {
		t.Errorf("ssh -- exit 7 exit %d, want 7", r.Code)
	}
	// It runs in the clone, like the interactive shell.
	if r := e.Devcrate("ssh", "--", "pwd"); r.Stdout != "/var/lib/devcrate/workspace\n" {
		t.Errorf("ssh -- pwd = %q exit %d, want the clone", r.Stdout, r.Code)
	}
}

func TestSSHOnStoppedEnvironmentFails(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("stop"); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}

	r := e.Devcrate("ssh", e.Name, "--", "true")
	if r.Code != 1 || !strings.Contains(r.Stderr, e.Name+" is stopped; run devcrate up in its repo") {
		t.Errorf("ssh on stopped = exit %d stderr %q, want exit 1 with the stopped message", r.Code, r.Stderr)
	}
	if e.ContainerID() != "" {
		t.Error("ssh started the stopped Environment")
	}
}

func TestPlainSSHWorksAcrossResume(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	// The user's own SSH config, with no trailing newline.
	userConfig := "Host example\n  User me"
	must(os.MkdirAll(filepath.Join(e.Home, ".ssh"), 0o700))
	must(os.WriteFile(filepath.Join(e.Home, ".ssh", "config"), []byte(userConfig), 0o600))

	r := e.Devcrate("up")
	if r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if !strings.Contains(r.Stdout, "~/.ssh/config") {
		t.Errorf("first up did not say it edited ~/.ssh/config:\n%s", r.Stdout)
	}
	config := e.ReadHome(".ssh/config")
	// Absolute, not ~: ssh takes ~ from the passwd entry, not the test's $HOME.
	include := "Include " + filepath.Join(e.Home, ".ssh", "devcrate_config") + "\n"
	if config != include+userConfig {
		t.Errorf("~/.ssh/config after up = %q, want the Include line then the user's config unchanged", config)
	}
	e.PlainSSH("true")
	e.PlainSSH("echo kept > /root/scratch")

	if r := e.Devcrate("stop"); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	if !strings.Contains(e.ReadHome(".ssh/devcrate_config"), "Host devcrate-"+e.Name+"\n") {
		t.Error("stop removed the SSH config block")
	}

	// The resumed VM is a new container on a new port with fresh host keys
	// until user data restores the ones on the volume.
	r = e.Devcrate("up")
	if r.Code != 0 {
		t.Fatalf("up after stop exit %d", r.Code)
	}
	if strings.Contains(r.Stdout, "~/.ssh/config") {
		t.Errorf("second up mentioned ~/.ssh/config again:\n%s", r.Stdout)
	}
	if got := e.ReadHome(".ssh/config"); got != config {
		t.Errorf("second up changed ~/.ssh/config to %q", got)
	}
	if out := e.PlainSSH("cat /root/scratch"); out != "kept\n" {
		t.Errorf("/root/scratch after resume = %q, want kept", out)
	}
}

func TestDeleteRemovesSSHConfigBlock(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("delete", "--yes"); r.Code != 0 {
		t.Fatalf("delete exit %d", r.Code)
	}
	if strings.Contains(e.ReadHome(".ssh/devcrate_config"), "devcrate-"+e.Name) {
		t.Error("delete left the SSH config block")
	}

	// A new Environment under the same name has new host keys, and must not
	// trip over the deleted one's pinned key.
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up after delete exit %d", r.Code)
	}
	e.PlainSSH("true")
}

// ReadHome reads a file under the test's HOME.
func (e *Env) ReadHome(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.Home, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// PlainSSH runs cmd with the system ssh as a user would (`ssh devcrate-<name>`),
// reading the test HOME's ~/.ssh/config, since ssh takes ~ from the passwd
// entry rather than $HOME. BatchMode turns any host key prompt into a failure,
// and any host key warning fails the test.
func (e *Env) PlainSSH(cmd string) string {
	e.t.Helper()
	c := exec.Command("ssh", "-F", filepath.Join(e.Home, ".ssh", "config"), "-o", "BatchMode=yes", "devcrate-"+e.Name, cmd)
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil || strings.Contains(stderr.String(), "WARNING") {
		e.t.Fatalf("ssh devcrate-%s %q: %v\n%s", e.Name, cmd, err, stderr.String())
	}
	return string(out)
}
