//go:build e2e

package e2e

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// freeLowPort is a laptop port nothing listens on right now, below Linux's
// ephemeral range, so the Agent detects it when the container listens on it.
func freeLowPort(t *testing.T) int {
	t.Helper()
	for range 100 {
		if port := 1024 + rand.IntN(32768-1024); localFree(port) {
			return port
		}
	}
	t.Fatal("no free port below 32768")
	return 0
}

// localFree reports whether nothing listens on localhost:port.
func localFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// newDetectEnv is an Environment, already up, with devcontainerJSON.
func newDetectEnv(t *testing.T, devcontainerJSON string) *Env {
	e := NewEnv(t)
	e.withDevcontainer()
	e.WriteFile(".devcontainer/devcontainer.json", devcontainerJSON)
	e.Push()
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	return e
}

// line is forward's output for a port forwarded to the same local port.
func line(port int) string { return fmt.Sprintf("%d -> localhost:%d", port, port) }

// notForwarded fails the test if forward printed or bound any of ports.
// Call it once a port started after them was detected: it waits one more
// poll first.
func notForwarded(t *testing.T, cmd fmt.Stringer, ports ...int) {
	t.Helper()
	time.Sleep(3 * time.Second)
	out := "\n" + cmd.String()
	for _, p := range ports {
		// A laptop port below 1024 may be in use for other reasons.
		if strings.Contains(out, fmt.Sprintf("\n%d ", p)) || (p >= 1024 && !localFree(p)) {
			t.Errorf("port %d was forwarded; stdout %q", p, out)
		}
	}
}

// listensInContainer fails the test unless something in the devcontainer
// listens on port, so a port that is not forwarded proves something.
func (e *Env) listensInContainer(ports ...int) {
	e.t.Helper()
	for _, p := range ports {
		if out, code := e.VM(fmt.Sprintf("docker exec %s grep -q ':%04X 00000000:0000 0A' /proc/net/tcp", e.running(), p)); code != 0 {
			e.t.Fatalf("nothing listens on %d in the devcontainer: %s", p, out)
		}
	}
}

// stopInContainer stops listenInContainer's server on port.
func (e *Env) stopInContainer(port int) {
	e.t.Helper()
	if out, code := e.VM(fmt.Sprintf("docker exec %s pkill -f '[n]c -lk -s .* -p %d '", e.running(), port)); code != 0 {
		e.t.Fatalf("stopping the server on %d: %s", port, out)
	}
}

func TestForwardDetectsListeningPorts(t *testing.T) {
	t.Parallel()
	before, later, busy, specific := freeLowPort(t), freeLowPort(t), freeLowPort(t), freeLowPort(t)
	ephemeral := 40000 + rand.IntN(20000)
	e := newDetectEnv(t, fmt.Sprintf(`{"image": "alpine:3.20", "portsAttributes": {"%d": {"label": "web"}}}`, before))
	e.listenInContainer("127.0.0.1", before, "before")
	e.listenInContainer("0.0.0.0", busy, "busy")
	e.listenInContainer("0.0.0.0", 999, "low")
	e.listenInContainer("0.0.0.0", ephemeral, "ephemeral")
	if out, code := e.VM(fmt.Sprintf(`docker exec -d %s sh -c 'nc -lk -s $(hostname -i) -p %d -e echo specific'`, e.running(), specific)); code != 0 {
		t.Fatalf("starting a server on the container's IP: %s", out)
	}
	taken, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", busy))
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	// No forwardPorts and no -p: detection alone is enough.
	cmd := e.startForward(fmt.Sprintf("%d (web) -> localhost:%d", before, before))
	if got := readLocal(t, before, 7); got != "before\n" {
		t.Errorf("localhost:%d answered %q, want the server already listening", before, got)
	}
	e.WaitOutput(cmd, fmt.Sprintf("localhost:%d is in use; not forwarding %d (pick another local port with -p %d:%d)", busy, busy, busy+1, busy))

	e.listenInContainer("0.0.0.0", later, "later")
	e.WaitOutput(cmd, line(later))
	if got := readLocal(t, later, 6); got != "later\n" {
		t.Errorf("localhost:%d answered %q, want the server started while forward runs", later, got)
	}
	taken.Close()
	e.listensInContainer(busy, 999, ephemeral, specific)
	notForwarded(t, cmd.Stdout.(fmt.Stringer), busy, 999, ephemeral, specific)
	if n := strings.Count(fmt.Sprint(cmd.Stderr), fmt.Sprintf("localhost:%d is in use", busy)); n != 1 {
		t.Errorf("warned %d times about busy localhost:%d, want once (not retried)", n, busy)
	}
}

// waitLines waits up to 60 s for forward's stdout to contain s n times.
func (e *Env) waitLines(cmd *exec.Cmd, s string, n int) {
	e.t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if strings.Count(fmt.Sprint(cmd.Stdout), s) >= n {
			return
		}
	}
	e.t.Fatalf("devcrate never printed %q %d times; stdout %q stderr %q", s, n, cmd.Stdout, cmd.Stderr)
}

func TestForwardIgnoresPortsByOnAutoForward(t *testing.T) {
	t.Parallel()
	single, ranged, other := freeLowPort(t), freeLowPort(t), freeLowPort(t)
	e := newDetectEnv(t, fmt.Sprintf(`{"image": "alpine:3.20", "portsAttributes": {
		"%d": {"onAutoForward": "ignore"},
		"%d-%d": {"onAutoForward": "ignore"}
	}}`, single, ranged-1, ranged+1))
	for _, p := range []int{single, ranged, other} {
		e.listenInContainer("0.0.0.0", p, "x")
	}
	cmd := e.startForward(line(other))
	e.listensInContainer(single, ranged)
	notForwarded(t, cmd.Stdout.(fmt.Stringer), single, ranged)
	cmd.Process.Kill()
	cmd.Wait()

	// Allowlist-only: only ports with their own portsAttributes key.
	e.editWorkspace(".devcontainer/devcontainer.json", fmt.Sprintf(`{"image": "alpine:3.20",
		"portsAttributes": {"%d": {"label": "listed"}},
		"otherPortsAttributes": {"onAutoForward": "ignore"}
	}`, single))
	cmd = e.startForward(fmt.Sprintf("%d (listed) -> localhost:%d", single, single))
	notForwarded(t, cmd.Stdout.(fmt.Stringer), ranged, other)
}

func TestForwardClosesDetectedPortsButNotExplicitOnes(t *testing.T) {
	t.Parallel()
	detected, explicit := freeLowPort(t), freeLowPort(t)
	e := newDetectEnv(t, `{"image": "alpine:3.20"}`)
	e.listenInContainer("127.0.0.1", detected, "detected")
	e.listenInContainer("127.0.0.1", explicit, "explicit")
	cmd := e.startForward(line(detected), "-p", fmt.Sprint(explicit))
	wantLines(t, fmt.Sprint(cmd.Stdout), line(explicit), line(detected))

	e.stopInContainer(detected)
	e.stopInContainer(explicit)
	e.WaitOutput(cmd, fmt.Sprintf("%d closed", detected))
	if !localFree(detected) {
		t.Errorf("localhost:%d is still bound after its server stopped", detected)
	}
	time.Sleep(3 * time.Second)
	if strings.Contains(fmt.Sprint(cmd.Stdout), fmt.Sprintf("%d closed", explicit)) || localFree(explicit) {
		t.Errorf("the explicit port %d was un-forwarded; stdout %q", explicit, cmd.Stdout)
	}
}

func TestForwardNoDetectForwardsOnlyExplicitPorts(t *testing.T) {
	t.Parallel()
	detectable, explicit := freeLowPort(t), freeLowPort(t)
	e := newDetectEnv(t, fmt.Sprintf(`{"image": "alpine:3.20", "forwardPorts": [%d]}`, explicit))
	e.listenInContainer("127.0.0.1", explicit, "explicit")
	e.listenInContainer("127.0.0.1", detectable, "detectable")
	cmd := e.startForward(line(explicit), "--no-detect")
	e.listensInContainer(detectable)
	notForwarded(t, cmd.Stdout.(fmt.Stringer), detectable)
	if got := readLocal(t, explicit, 9); got != "explicit\n" {
		t.Errorf("localhost:%d answered %q, want the forwardPorts server", explicit, got)
	}
}

func TestForwardDetectsPortsAgainAfterARebuild(t *testing.T) {
	t.Parallel()
	port := freeLowPort(t)
	e := newDetectEnv(t, `{"image": "alpine:3.20"}`)
	e.listenInContainer("127.0.0.1", port, "first")
	cmd := e.startForward(line(port))

	e.up("--rebuild")
	e.WaitOutput(cmd, fmt.Sprintf("%d closed", port))
	e.listenInContainer("127.0.0.1", port, "second")
	e.waitLines(cmd, line(port), 2)
	if got := readLocal(t, port, 7); got != "second\n" {
		t.Errorf("localhost:%d answered %q, want the rebuilt container's server", port, got)
	}
}
