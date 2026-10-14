//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort is a laptop port nothing listens on right now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startForward starts `devcrate forward args...` and waits until it prints
// want (the last port line it should print). It is killed after the test.
func (e *Env) startForward(want string, args ...string) *exec.Cmd {
	e.t.Helper()
	cmd := e.Command(append([]string{"forward"}, args...)...)
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	e.WaitOutput(cmd, want)
	return cmd
}

// readLocal connects to localhost:port and returns the first n bytes the
// other end sends.
func readLocal(t *testing.T, port, n int) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatalf("connecting to localhost:%d: %v", port, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading from localhost:%d: %v (got %q)", port, err, buf)
	}
	return string(buf)
}

// listenInContainer runs a server in the devcontainer that answers every
// connection to addr:port with reply.
func (e *Env) listenInContainer(addr string, port int, reply string) {
	e.t.Helper()
	id, _, _ := strings.Cut(e.devcontainer(), " ")
	if out, code := e.VM(fmt.Sprintf("docker exec -d %s nc -lk -s %s -p %d -e echo %s", id, addr, port, reply)); code != 0 {
		e.t.Fatalf("starting a server in the devcontainer: %s", out)
	}
}

func TestForwardOnStoppedEnvironmentFails(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("stop"); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	r := e.Devcrate("forward", "-p", "3000")
	if r.Code != 1 || !strings.Contains(r.Stderr, e.Name+" is stopped") || !strings.Contains(r.Stderr, "devcrate up") {
		t.Errorf("forward on a stopped Environment = exit %d stderr %q, want exit 1 naming the state and devcrate up", r.Code, r.Stderr)
	}
	if e.ContainerID() != "" {
		t.Error("forward started the Environment")
	}
}

func TestForwardReportsABrokenDevcontainerConfig(t *testing.T) {
	t.Parallel()
	e := newDetectEnv(t, `{"image": "alpine:3.20", "forwardPorts": [3000]}`)
	e.editWorkspace(".devcontainer/devcontainer.json", `["not", "an", "object"]`)
	r := e.Devcrate("forward", "--no-detect")
	if r.Code != 1 || !strings.Contains(r.Stderr, "devcontainer.json") || strings.Contains(r.Stderr, "nothing to forward") {
		t.Errorf("forward with a broken devcontainer.json = exit %d stderr %q, want exit 1 with the config error", r.Code, r.Stderr)
	}
}

func TestForwardInVMOnlyModeDialsOnTheVM(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	// sshd listens on the VM's port 22: a remap to a free laptop port.
	local := freePort(t)
	e.startForward(fmt.Sprintf("22 -> localhost:%d", local), "-p", fmt.Sprintf("%d:22", local))
	if got := readLocal(t, local, 8); got != "SSH-2.0-" {
		t.Errorf("localhost:%d answered %q, want the VM's sshd banner", local, got)
	}
	// Each connection is an SSH session: allow more than sshd's default 10.
	// (Not tested by opening them: here the target is sshd, whose own
	// MaxStartups would drop unauthenticated connections past 10.)
	if out, _ := e.VM("sshd -T | grep -i '^maxsessions '"); out != "maxsessions 200\n" {
		t.Errorf("sshd -T: %q, want maxsessions 200", out)
	}
}

func TestForwardExitsWhenTheConnectionDrops(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	local := freePort(t)
	cmd := e.startForward(fmt.Sprintf("22 -> localhost:%d", local), "-p", fmt.Sprintf("%d:22", local))
	// forward's session process, not the listening sshd.
	if out, code := e.VM("pkill -f '[s]shd: root@'"); code != 0 {
		t.Fatalf("killing the SSH session: %s", out)
	}
	done := make(chan Result, 1)
	go func() { done <- e.Wait(cmd, []string{"forward"}) }()
	select {
	case r := <-done:
		if r.Code == 0 || !strings.Contains(r.Stderr, "connection to "+e.Name+" lost") {
			t.Errorf("forward after the connection dropped = exit %d stderr %q, want non-zero with the reason", r.Code, r.Stderr)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("forward kept running after its SSH connection was killed")
	}
}

func TestOpenForwardKeepsEnvironmentRunning(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = \"3s\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	local := freePort(t)
	cmd := e.startForward(fmt.Sprintf("22 -> localhost:%d", local), "-p", fmt.Sprintf("%d:22", local))
	e.stillRunningFor(10*time.Second, "with forward open")
	cmd.Process.Kill()
	if !e.waitStopped(60 * time.Second) {
		t.Fatalf("Environment did not stop itself after forward exited; agent log:\n%s", e.agentLog())
	}
}

func TestForwardReachesAComposeService(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	port := freePort(t)
	e.withDevcontainer()
	e.WriteFile(".devcontainer/devcontainer.json", fmt.Sprintf(`{
		"dockerComposeFile": "compose.yml",
		"service": "app",
		"workspaceFolder": "/workspaces/workspace",
		"forwardPorts": ["db:%d"],
		"portsAttributes": {"db:%d": {"label": "database"}}
	}`, port, port))
	e.WriteFile(".devcontainer/compose.yml", fmt.Sprintf(`services:
  app:
    image: alpine:3.20
    command: sleep infinity
    volumes: ["..:/workspaces/workspace"]
  db:
    image: alpine:3.20
    command: nc -lk -p %d -e echo db
`, port))
	e.Push()
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	e.startForward(fmt.Sprintf("db:%d (database) -> localhost:%d", port, port))
	if got := readLocal(t, port, 3); got != "db\n" {
		t.Errorf("localhost:%d answered %q, want the compose service db", port, got)
	}
}

func TestForwardReachesLoopbackServersInTheDevcontainer(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	web, busy, extra := freePort(t), freePort(t), freePort(t)
	e.withDevcontainer()
	e.WriteFile(".devcontainer/devcontainer.json", fmt.Sprintf(`{
		// JSONC, as devcontainer.json usually is
		"image": "alpine:3.20",
		"forwardPorts": [%d, %d],
		"portsAttributes": {"%d": {"label": "web"}},
	}`, web, busy, web))
	e.Push()
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	e.listenInContainer("127.0.0.1", web, "web")
	e.listenInContainer("127.0.0.1", extra, "extra")
	taken, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", busy))
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	cmd := e.startForward(fmt.Sprintf("%d -> localhost:%d", extra, extra), "-p", strconv.Itoa(extra))
	wantLines(t, fmt.Sprint(cmd.Stdout), fmt.Sprintf("%d (web) -> localhost:%d", web, web), fmt.Sprintf("%d -> localhost:%d", extra, extra))
	if stderr := fmt.Sprint(cmd.Stderr); !strings.Contains(stderr, fmt.Sprintf("localhost:%d is in use", busy)) || !strings.Contains(stderr, fmt.Sprintf("-p %d:%d", busy+1, busy)) {
		t.Errorf("stderr %q, want a warning that localhost:%d is busy naming -p %d:%d", stderr, busy, busy+1, busy)
	}
	if got := readLocal(t, web, 4); got != "web\n" {
		t.Errorf("localhost:%d answered %q, want the devcontainer's 127.0.0.1 server", web, got)
	}
	if got := readLocal(t, extra, 6); got != "extra\n" {
		t.Errorf("localhost:%d answered %q, want the devcontainer's -p server", extra, got)
	}
}
