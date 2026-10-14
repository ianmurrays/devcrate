//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestStatusOfUnknownEnvironmentIsMissing(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)

	r := e.Devcrate("status")
	if r.Code != 0 || r.Stdout != "missing\n" {
		t.Fatalf("status = %q exit %d, want \"missing\" exit 0", r.Stdout, r.Code)
	}
}

func TestUpBringsEnvironmentToReady(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)

	r := e.Devcrate("up")
	if r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	wantLines(t, r.Stdout,
		"creating VM…",
		"waiting for SSH…",
		"running setup…",
		"ready: ssh devcrate-"+e.Name+"  (or: devcrate ssh "+e.Name+")",
	)
	// Setup output is streamed unchanged (git writes progress to stderr).
	wantLines(t, r.Stderr, "Cloning into '/var/lib/devcrate/workspace'...")
	if out, code := e.VM("cat /var/lib/devcrate/workspace/README.md"); code != 0 || out != "fixture "+e.Name+"\n" {
		t.Errorf("workspace README = %q (exit %d), want the cloned repo", out, code)
	}
	if _, code := e.VM("test -f /run/devcrate/ready"); code != 0 {
		t.Error("/run/devcrate/ready missing after up")
	}
	if r := e.Devcrate("status"); r.Code != 0 || r.Stdout != "ready\n" {
		t.Errorf("status = %q exit %d, want ready", r.Stdout, r.Code)
	}

	// A second up on a Ready Environment succeeds and changes nothing.
	id := e.ContainerID()
	e.VM("touch /var/lib/devcrate/workspace/local-change")
	r = e.Devcrate("up")
	if r.Code != 0 || !strings.HasSuffix(r.Stdout, "ready: ssh devcrate-"+e.Name+"  (or: devcrate ssh "+e.Name+")\n") {
		t.Fatalf("second up exit %d", r.Code)
	}
	if e.ContainerID() != id {
		t.Error("second up replaced the VM")
	}
	if _, code := e.VM("test -f /var/lib/devcrate/workspace/local-change"); code != 0 {
		t.Error("second up re-cloned the workspace")
	}

	// status reads the VM, so a missing Ready marker or a stopped VM shows.
	e.VM("rm /run/devcrate/ready")
	if r := e.Devcrate("status"); r.Code != 0 || r.Stdout != "running\n" {
		t.Errorf("status without Ready marker = %q exit %d, want running", r.Stdout, r.Code)
	}
	if err := sh("", "docker", "stop", e.Container()); err != nil {
		t.Fatal(err)
	}
	if r := e.Devcrate("status"); r.Code != 0 || r.Stdout != "stopped\n" {
		t.Errorf("status of stopped VM = %q exit %d, want stopped", r.Stdout, r.Code)
	}
}

func TestUpRefusesBareEnvironmentName(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)

	r := e.Devcrate("up", e.Name)
	if r.Code != 1 || !strings.Contains(r.Stderr, "bare Environment name") {
		t.Fatalf("up %s = exit %d stderr %q, want exit 1 refusing a bare name", e.Name, r.Code, r.Stderr)
	}
	if e.ContainerID() != "" {
		t.Error("up with a bare name created a VM")
	}
}

func TestProviderFailureFailsUpWithItsStderr(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	// Pass-through options reach the Provider; the fake fails the named verb.
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nfail = \"up\"\n")

	r := e.Devcrate("up")
	if r.Code != 1 || !strings.Contains(r.Stderr, "fake: failing up as asked") {
		t.Fatalf("up = exit %d stderr %q, want exit 1 with the Provider's stderr", r.Code, r.Stderr)
	}
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	got := strings.Split(out, "\n")
	i := 0
	for _, l := range got {
		if i < len(lines) && l == lines[i] {
			i++
		}
	}
	if i < len(lines) {
		t.Errorf("output missing line %q (in order) in:\n%s", lines[i], out)
	}
}
