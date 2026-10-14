//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// keepAliveEnv is a VM-only Environment with a 3 s idle_timeout, up.
func keepAliveEnv(t *testing.T) *Env {
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = \"3s\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	return e
}

// heldThenStops checks that an Environment kept alive for 10 s from held
// stays up for the hold plus idle_timeout (3 s), then stops on its own.
func (e *Env) heldThenStops(held time.Time) {
	e.t.Helper()
	e.stillRunningFor(time.Until(held.Add(12*time.Second)), "during its Keep-alive plus idle_timeout")
	if !e.waitStopped(60 * time.Second) {
		e.t.Fatalf("Environment did not stop itself after its Keep-alive expired; agent log:\n%s", e.agentLog())
	}
}

func TestKeepAliveFromLaptopHoldsEnvironment(t *testing.T) {
	t.Parallel()
	e := keepAliveEnv(t)
	held := time.Now()
	// By name; the stopped test below covers the repo's own (no name).
	if r := e.Devcrate("keep-alive", e.Name, "10s"); r.Code != 0 || !strings.Contains(r.Stdout, "kept alive until") {
		t.Fatalf("keep-alive = exit %d stdout %q stderr %q", r.Code, r.Stdout, r.Stderr)
	}
	e.heldThenStops(held)
}

func TestKeepAliveFromVMHoldsEnvironment(t *testing.T) {
	t.Parallel()
	e := keepAliveEnv(t)
	held := time.Now()
	// docker exec, not SSH: no connection of its own keeps the VM up.
	if out, code := e.VM("devcrate-agent keep-alive 10s"); code != 0 || !strings.Contains(out, "kept alive until") {
		t.Fatalf("devcrate-agent keep-alive = exit %d output %q", code, out)
	}
	e.heldThenStops(held)
}

func TestKeepAliveOnStoppedEnvironmentFailsLikeSSH(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("stop"); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	ssh := e.Devcrate("ssh")
	r := e.Devcrate("keep-alive", "1h")
	if r.Code != 1 || r.Stderr != ssh.Stderr || !strings.Contains(r.Stderr, "is stopped") {
		t.Errorf("keep-alive on a stopped Environment = exit %d stderr %q, want exit 1 and ssh's %q", r.Code, r.Stderr, ssh.Stderr)
	}
}
