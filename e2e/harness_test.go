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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var (
	binDir     string // devcrate and devcrate-provider-fake
	gitRoot    string // bare fixture repos, served over dumb HTTP
	gitBaseURL string // as seen from inside the VM
	vmImage    string
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
	// laptop's loopback, so the VM can clone from this server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go http.Serve(ln, http.FileServer(http.Dir(gitRoot)))
	gitBaseURL = fmt.Sprintf("http://host.docker.internal:%d", ln.Addr().(*net.TCPAddr).Port)

	return m.Run()
}

// Env is one test's Environment: a unique name, an isolated HOME, and a local
// clone whose origin is a bare repo the VM can clone.
type Env struct {
	t    *testing.T
	Name string
	Home string
	Dir  string // the local clone; devcrate runs here
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

// Result is a finished devcrate command.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Devcrate runs the devcrate binary in the local clone with HOME isolated.
func (e *Env) Devcrate(args ...string) Result {
	e.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "devcrate"), args...)
	cmd.Dir = e.Dir
	cmd.Env = append(os.Environ(),
		"HOME="+e.Home,
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"DEVCRATE_FAKE_IMAGE="+vmImage,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if exit, ok := err.(*exec.ExitError); ok {
		r.Code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatalf("devcrate %v: %v", args, err)
	}
	e.t.Logf("devcrate %s -> exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	return r
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
