//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The harness runs every Agent with a 1 s poll (DEVCRATE_TEST_AGENT_POLL), so
// a few seconds of idle_timeout is enough.

// waitStopped waits up to limit for the Agent to Self-stop the Environment:
// its VM gone. It watches Docker, not `devcrate status`, whose SSH probe
// would count as a connection.
func (e *Env) waitStopped(limit time.Duration) bool {
	e.t.Helper()
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if e.ContainerID() == "" {
			return true
		}
	}
	return false
}

// stillRunningFor fails the test if the Environment's VM disappears within d.
func (e *Env) stillRunningFor(d time.Duration, why string) {
	e.t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if e.ContainerID() == "" {
			e.t.Fatalf("Environment stopped itself %s", why)
		}
	}
}

func (e *Env) agentLog() string {
	out, _ := e.VM("journalctl -u devcrate-agent --no-pager -n 50")
	return out
}

func TestIdleEnvironmentStopsItself(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = \"3s\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if !e.waitStopped(60 * time.Second) {
		t.Fatalf("Environment did not stop itself after idle_timeout; agent log:\n%s", e.agentLog())
	}
	if !e.VolumeExists() {
		t.Error("Self-stop removed the volume")
	}
	if r := e.Devcrate("status"); r.Stdout != "stopped\n" {
		t.Errorf("status after Self-stop = %q, want stopped", r.Stdout)
	}
}

func TestOpenSSHSessionKeepsEnvironmentRunning(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = \"3s\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	// A session with no command running, like a terminal left open.
	session := exec.Command("ssh", "-F", filepath.Join(e.Home, ".ssh", "config"), "-o", "BatchMode=yes", "-N", "devcrate-"+e.Name)
	session.Env = append(os.Environ(), "HOME="+e.Home)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Process.Kill()

	e.stillRunningFor(10*time.Second, "with an SSH session open")
	session.Process.Kill()
	session.Wait()
	if !e.waitStopped(60 * time.Second) {
		t.Fatalf("Environment did not stop itself after the session closed; agent log:\n%s", e.agentLog())
	}
}

func TestRunningSetupKeepsEnvironmentRunning(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = \"3s\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	// A setup still running with nobody connected holds the lock setup.sh takes.
	if out, err := exec.Command("docker", "exec", "-d", e.Container(), "flock", "/run/devcrate/setup.lock", "sleep", "12").CombinedOutput(); err != nil {
		t.Fatalf("holding the setup lock: %v %s", err, out)
	}
	e.stillRunningFor(10*time.Second, "while setup was running")
	if !e.waitStopped(60 * time.Second) {
		t.Fatalf("Environment did not stop itself after setup exited; agent log:\n%s", e.agentLog())
	}
}

func TestFailedSetupStillStopsWhenIdle(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nname = \""+e.Name+"\"\nidle_timeout = \"3s\"\n")
	// A clone that fails: nothing is served at this origin.
	if err := sh(e.Dir, "git", "remote", "set-url", "origin", gitBaseURL+"/"+e.Name+"-missing.git"); err != nil {
		t.Fatal(err)
	}
	if r := e.Devcrate("up"); r.Code != 1 || !strings.Contains(r.Stderr, "setup failed") {
		t.Fatalf("up = exit %d stderr %q, want setup to fail", r.Code, r.Stderr)
	}
	if !e.waitStopped(60 * time.Second) {
		t.Fatalf("Environment did not stop itself after a failed setup; agent log:\n%s", e.agentLog())
	}
	if !e.VolumeExists() {
		t.Error("Self-stop removed the volume")
	}
}

func TestIdleTimeoutZeroNeverStops(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	// With idle stop off, no Self-stop credential is needed either.
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = 0\nno_self_stop_env = true\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	e.stillRunningFor(8*time.Second, "with idle_timeout = 0")
}

func TestUpFailsWithoutSelfStopCredential(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nno_self_stop_env = true\n")
	r := e.Devcrate("up")
	if r.Code != 1 || !strings.Contains(r.Stderr, "devcrate: provider fake returned no self_stop_env, so "+e.Name+" could not stop itself when Idle") ||
		!strings.Contains(r.Stderr, "idle_timeout = 0") {
		t.Errorf("up without self_stop_env = exit %d stderr %q, want exit 1 explaining idle stop cannot work", r.Code, r.Stderr)
	}
}

func TestSelfStopCredentialIsRootOnlyOnTmpfs(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	token := "tok-" + e.Name
	e.UserConfig("idle_timeout = \"1s\"\n[providers.fake]\ntoken = \"" + token + "\"\n")
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = \"45m\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	// The repo's idle_timeout wins over the user's default.
	if out, _ := e.VM("cat /usr/local/lib/devcrate/config.json"); !strings.Contains(out, `"idle_timeout":"45m0s"`) {
		t.Errorf("Agent config = %q, want the repo's idle_timeout", out)
	}
	const file = "/run/devcrate/self-stop.env"
	if out, _ := e.VM("stat -c '%a %U' " + file + "; stat -f -c %T " + file); out != "600 root\ntmpfs\n" {
		t.Errorf("self_stop_env file mode, owner and filesystem = %q, want 600 root on tmpfs", out)
	}
	if out, _ := e.VM("cat " + file); !strings.Contains(out, token) {
		t.Errorf("self_stop_env file = %q, want the token", out)
	}
	if out, code := e.VM("grep -rl " + token + " /var/lib/devcrate /var/lib/cloud/user-data"); code != 1 {
		t.Errorf("token found on the volume or in user data: %q (grep exit %d)", out, code)
	}
	// A sleeping laptop's session drops after about 3 minutes.
	if out, _ := e.VM("sshd -T | grep -E '^clientalive(interval|countmax) '"); out != "clientaliveinterval 60\nclientalivecountmax 3\n" {
		t.Errorf("sshd keep-alive settings = %q", out)
	}
}

func TestUpAfterSelfStopRestoresWorkspaceAndContainer(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	e.WriteFile(".devcontainer/devcontainer.json", devcontainerJSON)
	e.Push()
	// VM-only first, to load the image into the VM's Docker.
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = 0\ndevcontainer = false\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	e.LoadImage("alpine:3.20")
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = \"3s\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	id, _, _ := strings.Cut(e.devcontainer(), " ")
	if id == "" {
		t.Fatal("no devcontainer after up")
	}
	e.VM("echo kept > /var/lib/devcrate/workspace/notes.txt")
	if !e.waitStopped(90 * time.Second) {
		t.Fatalf("Environment did not stop itself; agent log:\n%s", e.agentLog())
	}

	// Takes effect from this up: the checks below must not race a Self-stop.
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = 0\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up after Self-stop exit %d", r.Code)
	}
	if got := e.devcontainer(); got != id+" running" {
		t.Errorf("devcontainer after Self-stop and up = %q, want the same container %s running", got, id)
	}
	if r := e.Devcrate("ssh", "--", "cat /var/lib/devcrate/workspace/notes.txt"); r.Stdout != "kept\n" {
		t.Errorf("workspace file after Self-stop and up = %q exit %d, want kept", r.Stdout, r.Code)
	}
	if r := e.Devcrate("ssh", "--container", "--", "cat /created"); r.Stdout != "created\n" {
		t.Errorf("postCreateCommand marker after Self-stop and up = %q, want one run", r.Stdout)
	}
}

func TestFailedSelfStopLeavesAgentErrorThatUpAndSSHPrint(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nidle_timeout = \"3s\"\nself_stop_fails = true\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	// The fake mounts the volume a second time at /mnt/devcrate-volume, so
	// this sees the volume even mid-attempt.
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if _, code := e.VM("test -f /mnt/devcrate-volume/agent-error"); code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no agent-error on the volume; agent log:\n%s", e.agentLog())
		}
	}
	// Stopping the Agent lets an attempt in progress finish and recover, so
	// the checks below never race the next retry.
	if out, code := e.VM("systemctl stop devcrate-agent"); code != 0 {
		t.Fatalf("stopping the Agent: %s", out)
	}
	if out, code := e.VM("mountpoint -q /var/lib/devcrate && mountpoint -q /root && systemctl is-active -q docker"); code != 0 {
		t.Errorf("after a failed Self-stop, the volume, /root and Docker are not all back: %s\nagent log:\n%s", out, e.agentLog())
	}

	const want = "fake: self-stop failing as asked"
	if r := e.Devcrate("ssh", "--", "true"); r.Code != 0 || !strings.Contains(r.Stderr, want) {
		t.Errorf("ssh after a failed Self-stop = exit %d stderr %q, want the agent-error printed", r.Code, r.Stderr)
	}
	if r := e.Devcrate("up"); r.Code != 0 || !strings.Contains(r.Stderr, want) {
		t.Errorf("up after a failed Self-stop = exit %d stderr %q, want the agent-error printed", r.Code, r.Stderr)
	}
}
