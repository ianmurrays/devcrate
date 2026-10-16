//go:build e2e

// Package e2e drives the real devcrate binary against the fake Provider
// (fakeprovider/), whose "VM" is a privileged systemd container built from vm/.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	binDir     string // devcrate and devcrate-provider-fake
	gitRoot    string // bare fixture repos, served over dumb HTTP
	gitBaseURL string // as seen from inside the VM
	gitPort    int
	vmImage    string
	mirrorURL  string // Docker Hub pull-through cache, as seen from inside the VM
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "devcrate-e2e-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)
	binDir = filepath.Join(tmp, "bin")
	gitRoot = filepath.Join(tmp, "git")
	must(os.MkdirAll(gitRoot, 0o755))

	must(sh("", "go", "build", "-o", filepath.Join(binDir, "devcrate"), "../cmd/devcrate"))
	must(sh("", "go", "build", "-o", filepath.Join(binDir, "devcrate-provider-fake"), "./fakeprovider"))
	// The Agent runs on the fake VM, so it is built for the Docker daemon's
	// CPU, not this machine's. devcrate finds it next to itself.
	arch, err := exec.Command("docker", "version", "-f", "{{.Server.Arch}}").Output()
	must(err)
	goarch := strings.TrimSpace(string(arch))
	agent := exec.Command("go", "build", "-o", filepath.Join(binDir, "devcrate-agent-linux-"+goarch), "../cmd/devcrate-agent")
	agent.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goarch)
	if out, err := agent.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("building the Agent: %v\n%s", err, out))
	}

	// Tag the VM image by the Dockerfile's hash so worktrees with different
	// Dockerfiles don't clobber each other's image.
	dockerfile, err := os.ReadFile("vm/Dockerfile")
	must(err)
	sum := sha256.Sum256(dockerfile)
	vmImage = "devcrate-fake-vm:" + hex.EncodeToString(sum[:])[:12]
	if sh("", "docker", "image", "inspect", vmImage) != nil {
		must(sh("", "docker", "build", "-q", "-t", vmImage, "vm"))
	}

	// Docker Desktop and OrbStack both route host.docker.internal to the
	// laptop's loopback, so the VM can clone from this server (on Linux, CI
	// routes it there, see .github/workflows/ci.yml). It speaks smart
	// HTTP, which shallow and filtered clones (`up <git-url>`) need.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	gitPath, err := exec.LookPath("git")
	must(err)
	go http.Serve(ln, &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + gitRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=uploadpack.allowFilter", "GIT_CONFIG_VALUE_0=true",
		},
	})
	gitPort = ln.Addr().(*net.TCPAddr).Port
	gitBaseURL = fmt.Sprintf("http://host.docker.internal:%d", gitPort)

	mirrorURL = ensureMirror()

	return m.Run()
}

// mirrorName is the Docker Hub pull-through cache that every fake VM's
// dockerd uses as a registry mirror, so parallel runs fetch each image from
// Hub once instead of using up its anonymous rate limit. It is shared by
// every test process on this Docker daemon and never removed.
const mirrorName = "devcrate-e2e-registry-mirror"

// ensureMirror starts the mirror if no test process has, and returns its URL
// as seen from inside a fake VM.
func ensureMirror() string {
	// Fails with a name conflict when another process created it first. The
	// image is ECR's copy of Docker's library one, so starting the mirror
	// never needs Hub quota.
	exec.Command("docker", "run", "-d", "--name", mirrorName,
		"--label", "devcrate.e2e.mirror=1", "--restart", "unless-stopped",
		"-p", "127.0.0.1::5000",
		"-e", "REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io",
		"public.ecr.aws/docker/library/registry:2").Run()
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		exec.Command("docker", "start", mirrorName).Run()
		out, err := exec.Command("docker", "port", mirrorName, "5000/tcp").Output()
		if err == nil {
			hostPort := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
			port := hostPort[strings.LastIndex(hostPort, ":")+1:]
			if resp, err := http.Get("http://127.0.0.1:" + port + "/v2/"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return "http://host.docker.internal:" + port
				}
			}
		}
		if time.Now().After(deadline) {
			panic(fmt.Sprintf("registry mirror %s did not come up: docker port: %v %s", mirrorName, err, out))
		}
	}
}

// Env is one test's Environment: a unique name, an isolated HOME, and a local
// clone whose origin is a bare repo the VM can clone.
type Env struct {
	t    *testing.T
	Name string
	Home string
	Dir  string // the local clone; devcrate runs here
	// LaptopEnv is extra NAME=value environment for every devcrate command.
	LaptopEnv []string
}

// NewEnv makes a fixture repo named e2e-<random> with `provider = "fake"` in
// its .devcrate.toml, and removes the Environment's VM and volume after the test.
func NewEnv(t *testing.T) *Env {
	t.Helper()
	b := make([]byte, 4)
	rand.Read(b)
	e := &Env{t: t, Name: "e2e-" + hex.EncodeToString(b), Home: t.TempDir()}
	e.Dir = filepath.Join(t.TempDir(), e.Name)
	must(os.MkdirAll(e.Dir, 0o755))
	t.Cleanup(e.destroy)

	// The laptop can't resolve host.docker.internal; its git (as devcrate
	// runs it, with this HOME) reaches the same server on loopback.
	gitconfig := fmt.Sprintf("[url \"http://127.0.0.1:%d/\"]\n\tinsteadOf = %s/\n", gitPort, gitBaseURL)
	must(os.WriteFile(filepath.Join(e.Home, ".gitconfig"), []byte(gitconfig), 0o644))

	e.WriteFile(".devcrate.toml", "provider = \"fake\"\n")
	e.WriteFile("README.md", "fixture "+e.Name+"\n")
	bare := filepath.Join(gitRoot, e.Name+".git")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"-c", "user.email=e2e@devcrate", "-c", "user.name=e2e", "commit", "-q", "-m", "fixture"},
		{"clone", "-q", "--bare", e.Dir, bare},
		{"-C", bare, "update-server-info"},
		{"remote", "add", "origin", gitBaseURL + "/" + e.Name + ".git"},
	} {
		if err := sh(e.Dir, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// WriteFile writes a file in the local clone's working tree (not committed).
func (e *Env) WriteFile(rel, content string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.Dir, rel), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// Push commits the local clone's working tree and pushes it to the bare repo
// the VM clones.
func (e *Env) Push() {
	e.t.Helper()
	bare := filepath.Join(gitRoot, e.Name+".git")
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.email=e2e@devcrate", "-c", "user.name=e2e", "commit", "-q", "-m", "change"},
		{"push", "-q", bare, "HEAD:main"},
		{"-C", bare, "update-server-info"},
	} {
		if err := sh(e.Dir, "git", args...); err != nil {
			e.t.Fatal(err)
		}
	}
}

// UserConfig writes the isolated HOME's ~/.config/devcrate/config.toml.
func (e *Env) UserConfig(content string) {
	e.t.Helper()
	dir := filepath.Join(e.Home, ".config", "devcrate")
	must(os.MkdirAll(dir, 0o755))
	must(os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o644))
}

// InstallProvider copies the fake Provider into the isolated HOME's
// ~/.config/devcrate/providers/ as devcrate-provider-<name>. It sees the same
// fake VMs as `fake`.
func (e *Env) InstallProvider(name string) {
	e.t.Helper()
	bin, err := os.ReadFile(filepath.Join(binDir, "devcrate-provider-fake"))
	must(err)
	dir := filepath.Join(e.Home, ".config", "devcrate", "providers")
	must(os.MkdirAll(dir, 0o755))
	must(os.WriteFile(filepath.Join(dir, "devcrate-provider-"+name), bin, 0o755))
}

// Result is a finished devcrate command.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Devcrate runs the devcrate binary in the local clone with HOME isolated.
func (e *Env) Devcrate(args ...string) Result {
	e.t.Helper()
	return e.DevcrateIn(e.Dir, args...)
}

// DevcrateIn runs devcrate in dir instead of the local clone.
func (e *Env) DevcrateIn(dir string, args ...string) Result {
	e.t.Helper()
	cmd := e.Command(args...)
	cmd.Dir = dir
	return e.Wait(cmd, args)
}

// DevcrateTTY runs devcrate on a pseudo-terminal via script(1) (BSD on macOS,
// util-linux on Linux), typing input once the terminal shows prompt. The
// terminal's output, prompt included, is in Stdout.
func (e *Env) DevcrateTTY(prompt, input string, args ...string) Result {
	e.t.Helper()
	cmd := e.Command(args...)
	script, err := exec.LookPath("script")
	if err != nil {
		e.t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		// util-linux takes the command as one shell string.
		quoted := make([]string, len(cmd.Args))
		for i, a := range cmd.Args {
			quoted[i] = shellQuote(a)
		}
		cmd.Args = []string{"script", "-qec", strings.Join(quoted, " "), "/dev/null"}
	} else {
		cmd.Args = append([]string{"script", "-q", "/dev/null"}, cmd.Args...)
	}
	cmd.Path = script
	stdin, err := cmd.StdinPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.WaitOutput(cmd, prompt)
	// Leave stdin open: on EOF script(1) types ^D, which can overtake input.
	// Wait closes it after devcrate exits.
	io.WriteString(stdin, input)
	return e.Wait(cmd, args)
}

// syncBuffer is a bytes.Buffer safe to read while a command writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Command is an unstarted devcrate command in the local clone with HOME
// isolated, its output captured for Wait. Start it to run it in the background.
func (e *Env) Command(args ...string) *exec.Cmd {
	cmd := exec.Command(filepath.Join(binDir, "devcrate"), args...)
	cmd.Dir = e.Dir
	cmd.Env = append(os.Environ(),
		"HOME="+e.Home,
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"DEVCRATE_FAKE_IMAGE="+vmImage,
		"DEVCRATE_FAKE_REGISTRY_MIRROR="+mirrorURL,
		// The hidden test-only override of the Agent's 60 s poll.
		"DEVCRATE_TEST_AGENT_POLL=1s",
		// Never forward the developer's own agent to test VMs; tests that
		// want one set it in LaptopEnv.
		"SSH_AUTH_SOCK=",
	)
	cmd.Env = append(cmd.Env, e.LaptopEnv...)
	cmd.Stdout, cmd.Stderr = &syncBuffer{}, &syncBuffer{}
	return cmd
}

// WaitOutput waits up to 30 s for a started Command's stdout or stderr to
// contain s, failing the test if it never does.
func (e *Env) WaitOutput(cmd *exec.Cmd, s string) {
	e.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(fmt.Sprint(cmd.Stdout), s) || strings.Contains(fmt.Sprint(cmd.Stderr), s) {
			return
		}
	}
	e.t.Fatalf("devcrate never printed %q; stdout %q stderr %q", s, cmd.Stdout, cmd.Stderr)
}

// Wait runs cmd from Command (or waits for it, if started) and logs its output.
func (e *Env) Wait(cmd *exec.Cmd, args []string) Result {
	e.t.Helper()
	var err error
	if cmd.Process == nil {
		err = cmd.Run()
	} else {
		err = cmd.Wait()
	}
	r := Result{Stdout: fmt.Sprint(cmd.Stdout), Stderr: fmt.Sprint(cmd.Stderr)}
	if exit, ok := err.(*exec.ExitError); ok {
		r.Code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatalf("devcrate %v: %v", args, err)
	}
	e.t.Logf("devcrate %s -> exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	return r
}

// VolumeExists reports whether the fake Environment's volume exists.
func (e *Env) VolumeExists() bool {
	return exec.Command("docker", "volume", "inspect", e.Container()).Run() == nil
}

// Container is the fake VM's Docker container name.
func (e *Env) Container() string { return "devcrate-fake-" + e.Name }

// VM runs a shell command as root on the fake VM via docker exec, bypassing
// devcrate. It fails the test if docker exec itself cannot run.
func (e *Env) VM(script string) (out string, code int) {
	e.t.Helper()
	b, err := exec.Command("docker", "exec", e.Container(), "sh", "-c", script).CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(b), exit.ExitCode()
	} else if err != nil {
		e.t.Fatalf("docker exec: %v", err)
	}
	return string(b), 0
}

// ContainerID is the fake VM's container ID, or "" when there is none.
func (e *Env) ContainerID() string {
	b, _ := exec.Command("docker", "inspect", "-f", "{{.Id}}", e.Container()).Output()
	return strings.TrimSpace(string(b))
}

func (e *Env) destroy() {
	label := "label=devcrate.environment=" + e.Name
	ids, _ := exec.Command("docker", "ps", "-aq", "--filter", label).Output()
	for _, id := range strings.Fields(string(ids)) {
		exec.Command("docker", "rm", "-f", "-v", id).Run() // -v: the VM's anonymous docker/containerd volumes
	}
	vols, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", label).Output()
	for _, v := range strings.Fields(string(vols)) {
		exec.Command("docker", "volume", "rm", "-f", v).Run()
	}
}

func sh(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %v\n%s", name, args, err, out)
	}
	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
