//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const waitingForLock = "waiting for another devcrate command"

// holdLock takes the Environment's laptop-side lock, as another devcrate
// command would, until release is called.
func (e *Env) holdLock() (release func()) {
	e.t.Helper()
	dir := filepath.Join(e.Home, ".local", "state", "devcrate")
	must(os.MkdirAll(dir, 0o700))
	f, err := os.OpenFile(filepath.Join(dir, e.Name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	must(err)
	must(syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	return func() { f.Close() }
}

// start starts a devcrate command in the background; it is killed after
// the test.
func (e *Env) start(args ...string) *exec.Cmd {
	e.t.Helper()
	cmd := e.Command(args...)
	must(cmd.Start())
	e.t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

func TestSSHKeepAliveAndForwardWaitForTheLock(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	local := freePort(t)
	release := e.holdLock()
	ssh := e.start("ssh", "--", "true")
	keepAlive := e.start("keep-alive", "1m")
	forward := e.start("forward", "--no-detect", "-p", fmt.Sprintf("%d:22", local))
	for _, cmd := range []*exec.Cmd{ssh, keepAlive, forward} {
		e.WaitOutput(cmd, waitingForLock)
	}
	if strings.Contains(fmt.Sprint(forward.Stdout), "localhost") {
		t.Errorf("forward forwarded while another command held the lock: %s", forward.Stdout)
	}
	release()

	if r := e.Wait(ssh, []string{"ssh"}); r.Code != 0 {
		t.Errorf("ssh exit %d after the lock was released", r.Code)
	}
	if r := e.Wait(keepAlive, []string{"keep-alive"}); r.Code != 0 {
		t.Errorf("keep-alive exit %d after the lock was released", r.Code)
	}
	e.WaitOutput(forward, fmt.Sprintf("22 -> localhost:%d", local))
}

// A connected ssh session or forward holds the lock only while it sets up,
// so stop still works while connected.
func TestStopWorksWhileConnected(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	local := freePort(t)
	e.startForward(fmt.Sprintf("22 -> localhost:%d", local), "--no-detect", "-p", fmt.Sprintf("%d:22", local))
	ssh := e.start("ssh", "--", "echo connected; sleep 120")
	e.WaitOutput(ssh, "connected")

	if r := e.Devcrate("stop"); r.Code != 0 || strings.Contains(r.Stderr, waitingForLock) {
		t.Errorf("stop while connected = exit %d stderr %q, want it to stop without waiting", r.Code, r.Stderr)
	}
}
