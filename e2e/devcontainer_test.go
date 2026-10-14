//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storageRoots is what the VM's running Docker and containerd use as storage,
// with symlinks resolved.
const storageRoots = `readlink -f "$(docker info -f '{{.DockerRootDir}}')"; ` +
	`readlink -f "$(containerd config dump 2>/dev/null | sed -n "s/^root = '\(.*\)'$/\1/p")"`

func TestVMOnlyEnvironmentGetsDockerOnTheVolume(t *testing.T) {
	t.Parallel()
	e := NewEnv(t) // no devcontainer.json
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status = %q, want ready", r.Stdout)
	}
	r := e.Devcrate("ssh", "--", storageRoots)
	if r.Code != 0 || r.Stdout != "/var/lib/devcrate/docker\n/var/lib/devcrate/containerd\n" {
		t.Errorf("storage roots = %q exit %d, want both under /var/lib/devcrate", r.Stdout, r.Code)
	}
	if r := e.Devcrate("ssh", "--", "docker run --rm "+alpineImage+" echo nested"); r.Code != 0 || r.Stdout != "nested\n" {
		t.Errorf("docker run on the VM = %q exit %d", r.Stdout, r.Code)
	}
	if out, _ := e.VM("docker ps -aq"); strings.TrimSpace(out) != "" {
		t.Errorf("VM-only Environment has containers: %q", out)
	}
	r = e.Devcrate("ssh", "--container", "--", "true")
	if r.Code != 1 || !strings.Contains(r.Stderr, e.Name+" has no devcontainer") {
		t.Errorf("ssh --container without a devcontainer = exit %d stderr %q, want exit 1 saying there is none", r.Code, r.Stderr)
	}
}

// alpineImage is alpine from Google's Docker Hub mirror, which has no anonymous
// pull limit (Hub allows 100 pulls an hour, which parallel e2e runs exhaust).
const alpineImage = "mirror.gcr.io/library/alpine:3.20"

// devcontainerJSON is an image-only devcontainer whose postCreateCommand
// appends a line to /created in the container each time it runs.
const devcontainerJSON = `{"image": "` + alpineImage + `", "postCreateCommand": "echo created >> /created"}`

// devcontainer is the VM's devcontainer as "<id> <state>", or "".
func (e *Env) devcontainer() string {
	e.t.Helper()
	out, _ := e.VM("docker ps -a --filter label=devcontainer.local_folder=/var/lib/devcrate/workspace --format '{{.ID}} {{.State}}'")
	return strings.TrimSpace(out)
}

func TestDevcontainerSurvivesStop(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	e.WriteFile(".devcontainer/devcontainer.json", devcontainerJSON)
	e.Push()

	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status = %q, want ready", r.Stdout)
	}
	first := e.devcontainer()
	id, state, _ := strings.Cut(first, " ")
	if id == "" || state != "running" {
		t.Fatalf("devcontainer after up = %q, want one running", first)
	}
	// ssh --container lands in the (alpine) devcontainer, not the (ubuntu) VM.
	if r := e.Devcrate("ssh", "--container", "--", "grep '^ID=' /etc/os-release; cat /created"); r.Code != 0 || r.Stdout != "ID=alpine\ncreated\n" {
		t.Errorf("ssh --container = %q exit %d, want alpine and one postCreateCommand run", r.Stdout, r.Code)
	}

	if r := e.Devcrate("stop"); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up after stop exit %d", r.Code)
	}
	if got := e.devcontainer(); got != id+" running" {
		t.Errorf("devcontainer after resume = %q, want the same container %s running", got, id)
	}
	if r := e.Devcrate("ssh", e.Name, "--container", "--", "cat /created"); r.Code != 0 || r.Stdout != "created\n" {
		t.Errorf("postCreateCommand marker after resume = %q exit %d, want one run", r.Stdout, r.Code)
	}

	e.VM("docker stop " + id)
	r := e.Devcrate("ssh", "--container", "--", "true")
	if r.Code != 1 || !strings.Contains(r.Stderr, "devcontainer is exited, not running") {
		t.Errorf("ssh --container to a stopped devcontainer = exit %d stderr %q, want exit 1 saying it is not running", r.Code, r.Stderr)
	}
}

func TestDevcontainerFalseIsVMOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		repo, user string
	}{
		{name: "repo", repo: "devcontainer = false\n"},
		{name: "user config", user: "devcontainer = false\n"},
		{name: "repo wins", repo: "devcontainer = false\n", user: "devcontainer = true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
			e.WriteFile(".devcontainer/devcontainer.json", devcontainerJSON)
			e.WriteFile(".devcrate.toml", "provider = \"fake\"\n"+tc.repo)
			e.Push()
			if tc.user != "" {
				e.UserConfig(tc.user)
			}

			if r := e.Devcrate("up"); r.Code != 0 {
				t.Fatalf("up exit %d", r.Code)
			}
			if r := e.Devcrate("status"); r.Stdout != "ready\n" {
				t.Errorf("status = %q, want ready", r.Stdout)
			}
			if got := e.devcontainer(); got != "" {
				t.Errorf("devcontainer = false Environment has a devcontainer: %q", got)
			}
			if r := e.Devcrate("ssh", "--", "docker info -f '{{.ServerVersion}}'"); r.Code != 0 {
				t.Errorf("docker on the VM-only Environment exit %d", r.Code)
			}
		})
	}
}

func TestPrerequisitesInstalledOnlyWhenMissing(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	e.WriteFile(".devcontainer/devcontainer.json", devcontainerJSON)
	e.Push()

	// The fake VM image carries both, like a snapshot image.
	r := e.Devcrate("up")
	if r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if strings.Contains(r.Stdout, "installing") {
		t.Errorf("up reinstalled a prerequisite already present:\n%s", r.Stdout)
	}

	// A VM without them, like a fresh cloud image.
	if out, code := e.VM("apt-get purge -y -q docker-ce docker-ce-cli containerd.io && rm -rf /opt/devcontainers"); code != 0 {
		t.Fatalf("removing the prerequisites: %s", out)
	}
	r = e.Devcrate("up")
	if r.Code != 0 {
		out, _ := e.VM("journalctl -u docker -u containerd -n 40 --no-pager; cat /etc/containerd/config.toml /etc/docker/daemon.json")
		t.Fatalf("up on a VM without prerequisites exit %d\n%s", r.Code, out)
	}
	wantLines(t, r.Stdout, "installing Docker…", "installing the devcontainer CLI…")
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status = %q, want ready", r.Stdout)
	}
	if got := e.devcontainer(); !strings.HasSuffix(got, " running") {
		t.Errorf("devcontainer after reinstalling = %q, want running", got)
	}
}

func TestUpFailsWhenStorageIsNotOnTheVolume(t *testing.T) {
	for _, tc := range []struct{ dir, what string }{
		{"docker", "Docker's data-root"},
		{"containerd", "containerd's root"},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			if r := e.Devcrate("up"); r.Code != 0 {
				t.Fatalf("up exit %d", r.Code)
			}
			// Point the storage directory off the volume, at the VM's root disk.
			if out, code := e.VM("rm -rf /var/lib/devcrate/" + tc.dir + " && ln -s /opt /var/lib/devcrate/" + tc.dir); code != 0 {
				t.Fatalf("redirecting storage: %s", out)
			}
			if r := e.Devcrate("stop"); r.Code != 0 {
				t.Fatalf("stop exit %d", r.Code)
			}

			r := e.Devcrate("up")
			want := "devcrate: " + tc.what + " is /opt, not under /var/lib/devcrate"
			if r.Code != 1 || !strings.Contains(r.Stderr, want) {
				t.Errorf("up = exit %d stderr %q, want exit 1 with %q", r.Code, r.Stderr, want)
			}
			if r := e.Devcrate("status"); r.Stdout != "running\n" {
				t.Errorf("status = %q, want running (not ready)", r.Stdout)
			}
		})
	}
}
