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

// gitSSH reaches the private git server without host key prompts.
const gitSSH = "GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR'"

// privateGitSetup is a repo setup_script that turns the VM into a private
// git server: user git, authorizing only the laptop's key (laptop.pub in the
// fixture), serving /srv/private.git. It writes the address the VM, its
// devcontainer and image builds all reach it at to .devcontainer/githost,
// then runs `git ls-remote` against it with whatever agent setup has.
const privateGitSetup = `set -eu
id git >/dev/null 2>&1 || { useradd -m -s /bin/sh git; usermod -p '*' git; }
mkdir -p /home/git/.ssh && cp laptop.pub /home/git/.ssh/authorized_keys
chown -R git /home/git/.ssh && chmod 700 /home/git/.ssh
[ -d /srv/private.git ] || { git clone -q --bare . /srv/private.git; chown -R git /srv/private.git; }
mkdir -p .devcontainer
docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}' > .devcontainer/githost
echo "setup agent: ${SSH_AUTH_SOCK:-none}"
` + gitSSH + ` git ls-remote "git@$(cat .devcontainer/githost):/srv/private.git" || echo "setup ls-remote failed"
`

// lsRemote lists the private repo from wherever /githost (or the VM's
// workspace copy) names the server.
const lsRemote = gitSSH + ` git ls-remote "git@$(cat /githost 2>/dev/null || cat /var/lib/devcrate/workspace/.devcontainer/githost):/srv/private.git"`

// agentDockerfile reaches the private repo during the image build, through
// the agent the build opts in to with --ssh default.
const agentDockerfile = `FROM alpine:3.20
RUN apk add --no-cache git openssh-client
COPY githost /githost
RUN --mount=type=ssh ` + lsRemote + ` > /built-ls-remote
`

// laptopAgent starts an ssh-agent holding a fresh key, as the laptop's own,
// passes it to every devcrate command, and returns its public key line.
func (e *Env) laptopAgent() string {
	e.t.Helper()
	dir, err := os.MkdirTemp("/tmp", "e2e-agent") // unix socket paths are short
	must(err)
	e.t.Cleanup(func() { os.RemoveAll(dir) })
	key := filepath.Join(dir, "id")
	if err := sh("", "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e-laptop-key", "-f", key); err != nil {
		e.t.Fatal(err)
	}
	sock := filepath.Join(dir, "agent.sock")
	agent := exec.Command("ssh-agent", "-D", "-a", sock)
	must(agent.Start())
	e.t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })
	for i := 0; ; i++ {
		add := exec.Command("ssh-add", key)
		add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
		out, err := add.CombinedOutput()
		if err == nil {
			break
		}
		if i == 50 {
			e.t.Fatalf("ssh-add: %v\n%s", err, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.LaptopEnv = append(e.LaptopEnv, "SSH_AUTH_SOCK="+sock)
	pub, err := os.ReadFile(key + ".pub")
	must(err)
	return string(pub)
}

func TestAgentForwardingReachesPrivateGitEverywhere(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile("laptop.pub", e.laptopAgent())
	e.WriteFile("private-git.sh", privateGitSetup)
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	e.WriteFile(".devcontainer/Dockerfile", agentDockerfile)
	e.WriteFile(".devcontainer/devcontainer.json", `{"build": {"dockerfile": "Dockerfile", "options": ["--ssh", "default"]}}`)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nsetup_script = \"private-git.sh\"\n")
	e.Push()

	r := e.Devcrate("up")
	if r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if !strings.Contains(r.Stdout, "refs/heads/main") || strings.Contains(r.Stdout, "setup ls-remote failed") {
		t.Errorf("git ls-remote during up's setup did not list the private repo:\n%s", r.Stdout)
	}
	if !strings.Contains(e.ReadHome(".ssh/devcrate_config"), "  ForwardAgent yes\n") {
		t.Errorf("SSH config entry lacks ForwardAgent yes:\n%s", e.ReadHome(".ssh/devcrate_config"))
	}

	if r := e.Devcrate("ssh", "--", lsRemote); r.Code != 0 || !strings.Contains(r.Stdout, "refs/heads/main") {
		t.Errorf("ls-remote over devcrate ssh = exit %d %q, want the private repo's refs", r.Code, r.Stdout)
	}

	id := strings.Fields(e.devcontainer())
	if len(id) == 0 {
		t.Fatal("no devcontainer")
	}
	if out, code := e.VM("docker exec " + id[0] + " cat /built-ls-remote"); code != 0 || !strings.Contains(out, "refs/heads/main") {
		t.Errorf("the image build's ls-remote = %q (exit %d), want the private repo's refs", out, code)
	}

	// Each ssh --container is a new connection, whose agent replaces the last.
	for i := range 2 {
		if r := e.Devcrate("ssh", "--container", "--", lsRemote); r.Code != 0 || !strings.Contains(r.Stdout, "refs/heads/main") {
			t.Errorf("ls-remote in the devcontainer, connection %d = exit %d %q, want the private repo's refs", i+1, r.Code, r.Stdout)
		}
	}

	// With nobody connected (docker exec is no SSH connection), the
	// container's agent socket gives nothing. sshd may take a moment to drop
	// the last session's socket.
	var out string
	code := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if out, code = e.VM("docker exec -e SSH_AUTH_SOCK=/run/devcrate/ssh-agent/agent.sock " + id[0] + " ssh-add -l"); code != 0 {
			break
		}
	}
	if code == 0 || strings.Contains(out, "e2e-laptop-key") {
		t.Errorf("with nobody connected, ssh-add -l in the devcontainer = %q (exit %d), want a refusal", out, code)
	}
}

func TestForwardAgentFalseForwardsNothing(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile("laptop.pub", e.laptopAgent())
	e.WriteFile("private-git.sh", privateGitSetup)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nsetup_script = \"private-git.sh\"\ndevcontainer = false\n")
	e.Push()
	e.UserConfig("forward_agent = false\n")

	r := e.Devcrate("up")
	if r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if !strings.Contains(r.Stdout, "setup agent: none") || strings.Contains(r.Stdout, "refs/heads/main") {
		t.Errorf("setup had an agent with forward_agent = false:\n%s", r.Stdout)
	}
	if strings.Contains(e.ReadHome(".ssh/devcrate_config"), "  ForwardAgent") {
		t.Errorf("SSH config entry forwards the agent with forward_agent = false:\n%s", e.ReadHome(".ssh/devcrate_config"))
	}
	if r := e.Devcrate("ssh", "--", "ssh-add -l"); r.Code == 0 || strings.Contains(r.Stdout, "e2e-laptop-key") {
		t.Errorf("devcrate ssh forwarded the agent with forward_agent = false: exit %d %q", r.Code, r.Stdout)
	}
}

func TestForwardAgentInRepoConfigIsRejected(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nforward_agent = true\n")

	r := e.Devcrate("up")
	if r.Code != 1 || !strings.Contains(r.Stderr, ".devcrate.toml: forward_agent can only be set in your user config") {
		t.Errorf("up with forward_agent in .devcrate.toml = exit %d stderr %q, want exit 1 naming the key", r.Code, r.Stderr)
	}
	if e.ContainerID() != "" {
		t.Error("up created the VM anyway")
	}
}
