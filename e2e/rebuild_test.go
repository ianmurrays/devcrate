//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildDockerfile has a layer whose output differs on every build, so a
// reused cache layer shows up as an unchanged /built.
const buildDockerfile = "FROM " + alpineImage + "\nRUN cat /proc/sys/kernel/random/uuid > /built\n"

// buildDevcontainerJSON builds buildDockerfile, with env as containerEnv.
func buildDevcontainerJSON(env string) string {
	return `{
	// JSONC, as editors write it
	"build": {"dockerfile": "Dockerfile"},
	"containerEnv": {"FLAVOUR": "` + env + `"},
	"postCreateCommand": "echo created >> /created",
}`
}

// newBuildEnv is an Environment, already up, whose devcontainer is built
// from a Dockerfile.
func newBuildEnv(t *testing.T) *Env {
	e := NewEnv(t)
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	e.WriteFile(".devcontainer/devcontainer.json", buildDevcontainerJSON("plain"))
	e.WriteFile(".devcontainer/Dockerfile", buildDockerfile)
	e.Push()
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	return e
}

// editWorkspace overwrites a file in the VM's clone, where the developer
// edits the repo (setup never pulls).
func (e *Env) editWorkspace(rel, content string) {
	e.t.Helper()
	if out, code := e.VM("cat > /var/lib/devcrate/workspace/" + rel + " <<'DEVCRATE_EOF'\n" + content + "\nDEVCRATE_EOF"); code != 0 {
		e.t.Fatalf("editing %s: %s", rel, out)
	}
}

// running is the ID of the one running devcontainer; it fails the test
// unless exactly one devcontainer exists and it is running.
func (e *Env) running() string {
	e.t.Helper()
	got := e.devcontainer()
	id, state, _ := strings.Cut(got, " ")
	if id == "" || state != "running" || strings.Contains(got, "\n") {
		e.t.Fatalf("devcontainers = %q, want exactly one running", got)
	}
	return id
}

// inContainer runs cmd in the devcontainer and returns its stdout.
func (e *Env) inContainer(cmd string) string {
	e.t.Helper()
	r := e.Devcrate("ssh", "--container", "--", cmd)
	if r.Code != 0 {
		e.t.Fatalf("ssh --container -- %s: exit %d", cmd, r.Code)
	}
	return r.Stdout
}

func (e *Env) up(args ...string) {
	e.t.Helper()
	if r := e.Devcrate(append([]string{"up"}, args...)...); r.Code != 0 {
		e.t.Fatalf("up %v exit %d", args, r.Code)
	}
}

func TestUpRebuildsOnlyWhenBuildConfigChanges(t *testing.T) {
	t.Parallel()
	e := newBuildEnv(t)
	first := e.running()
	built := e.inContainer("cat /built")

	e.up()
	if got := e.running(); got != first {
		t.Errorf("up with no config change replaced the devcontainer %s with %s", first, got)
	}

	// Not build-relevant: applied when attaching, not when creating.
	e.editWorkspace(".devcontainer/devcontainer.json", strings.Replace(buildDevcontainerJSON("plain"), "{", `{"forwardPorts": [3000],`, 1))
	e.up()
	if got := e.running(); got != first {
		t.Errorf("up after a forwardPorts change replaced the devcontainer %s with %s", first, got)
	}

	e.editWorkspace(".devcontainer/devcontainer.json", buildDevcontainerJSON("spicy"))
	e.up()
	second := e.running()
	if second == first {
		t.Fatalf("up after a containerEnv change kept the devcontainer %s", first)
	}
	if got := e.inContainer("cat /created; echo $FLAVOUR"); got != "created\nspicy\n" {
		t.Errorf("after the rebuild = %q, want postCreateCommand run once in a fresh container with the new env", got)
	}
	if got := e.inContainer("cat /built"); got != built {
		t.Errorf("/built = %q, was %q: the Dockerfile layer was rebuilt instead of cached", got, built)
	}

	e.editWorkspace(".devcontainer/Dockerfile", buildDockerfile+"RUN echo second > /second\n")
	e.up()
	if got := e.running(); got == second {
		t.Fatalf("up after a Dockerfile change kept the devcontainer %s", second)
	}
	if got := e.inContainer("cat /created /second /built"); got != "created\nsecond\n"+built {
		t.Errorf("after the Dockerfile rebuild = %q, want one postCreateCommand run, the new layer and the cached /built %q", got, built)
	}
}

func TestUpRebuildRecreatesTheDevcontainerOnTheLayerCache(t *testing.T) {
	t.Parallel()
	e := newBuildEnv(t)
	first := e.running()
	built := e.inContainer("cat /built")
	e.inContainer("echo old > /root/scratch")

	r := e.Devcrate("up", "--rebuild")
	if r.Code != 0 {
		t.Fatalf("up --rebuild exit %d", r.Code)
	}
	wantLines(t, r.Stdout, "rebuilding the devcontainer…")
	if got := e.running(); got == first {
		t.Fatalf("up --rebuild kept the devcontainer %s", first)
	}
	if got := e.inContainer("cat /created; test -e /root/scratch && echo stale home || true"); got != "created\n" {
		t.Errorf("after up --rebuild = %q, want one postCreateCommand run and a fresh home", got)
	}
	if got := e.inContainer("cat /built"); got != built {
		t.Errorf("/built = %q, was %q: up --rebuild rebuilt the Dockerfile layer instead of using the cache", got, built)
	}

	// A leftover container with the same label (say, from an interrupted
	// Rebuild) does not hide the running one from ssh --container.
	if out, code := e.VM("docker create --label devcontainer.local_folder=/var/lib/devcrate/workspace " + alpineImage); code != 0 {
		t.Fatalf("creating a leftover devcontainer: %s", out)
	}
	if got := e.inContainer("cat /created"); got != "created\n" {
		t.Errorf("ssh --container with a leftover container = %q, want the running devcontainer", got)
	}
}
