//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// devcontainersRunning prints how many devcontainers run on the VM.
const devcontainersRunning = `$(docker ps -q --filter label=devcontainer.local_folder=/var/lib/devcrate/workspace | wc -l)`

// repoSetupScript appends to /var/lib/devcrate/trace whether the clone exists
// and how many devcontainers run at that point.
const repoSetupScript = `test -d /var/lib/devcrate/workspace/.git && cloned=yes || cloned=no
echo "repo cloned=$cloned devcontainers=` + devcontainersRunning + `" >> /var/lib/devcrate/trace
`

// UserSetupScript writes the isolated HOME's ~/.config/devcrate/setup.sh.
func (e *Env) UserSetupScript(content string) {
	e.t.Helper()
	dir := filepath.Join(e.Home, ".config", "devcrate")
	must(os.MkdirAll(dir, 0o755))
	must(os.WriteFile(filepath.Join(dir, "setup.sh"), []byte(content), 0o644))
}

// withDevcontainer adds devcontainerJSON to the fixture (not pushed).
func (e *Env) withDevcontainer() {
	e.t.Helper()
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	e.WriteFile(".devcontainer/devcontainer.json", devcontainerJSON)
}

func TestSetupScriptsRunOnTheVMOnEveryUp(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.withDevcontainer()
	e.WriteFile("repo-setup.sh", repoSetupScript)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nsetup_script = \"repo-setup.sh\"\n")
	e.Push()
	e.UserSetupScript(`echo "user devcontainers=` + devcontainersRunning + `" >> /var/lib/devcrate/trace` + "\n")

	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("second up exit %d", r.Code)
	}
	// The second up finds the devcontainer from the first still running.
	want := "repo cloned=yes devcontainers=0\nuser devcontainers=1\nrepo cloned=yes devcontainers=1\nuser devcontainers=1\n"
	if out, _ := e.VM("cat /var/lib/devcrate/trace"); out != want {
		t.Errorf("trace = %q, want %q", out, want)
	}
}

func TestFailingSetupScriptFailsUpUntilFixed(t *testing.T) {
	const failing = "echo out-before-failing\necho err-before-failing >&2\nexit 3\n"
	for _, script := range []string{"repo", "user"} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			fix := func(content string) {
				if script == "user" {
					e.UserSetupScript(content)
					return
				}
				e.WriteFile("repo-setup.sh", content)
				if e.VolumeExists() {
					// up never pulls: fix the clone on the VM, as a user would.
					e.Devcrate("ssh", "--", "printf '"+content+"' > /var/lib/devcrate/workspace/repo-setup.sh")
					return
				}
				e.Push()
			}
			if script == "repo" {
				e.WriteFile(".devcrate.toml", "provider = \"fake\"\nsetup_script = \"repo-setup.sh\"\n")
			}
			fix(failing)

			if r := e.Devcrate("up"); r.Code != 1 || !strings.Contains(r.Stderr, "setup failed") {
				t.Fatalf("up with a failing %s script = exit %d stderr %q, want exit 1 and setup failed", script, r.Code, r.Stderr)
			}
			if r := e.Devcrate("status"); r.Stdout != "running\n" {
				t.Errorf("status after failed setup = %q, want running (not ready)", r.Stdout)
			}
			r := e.Devcrate("ssh", e.Name, "--", "cat /var/lib/devcrate/setup.log")
			// Setup's own output comes first; the version is whatever the VM image installed.
			if !strings.HasPrefix(r.Stdout, "Docker ") {
				t.Errorf("setup.log = %q, want setup's Docker check first", r.Stdout)
			}
			wantLines(t, r.Stdout, "out-before-failing")
			if !strings.Contains(r.Stdout, "err-before-failing\n") {
				t.Errorf("setup.log = %q, want the script's stderr too", r.Stdout)
			}

			fix("echo fixed\n")
			if r := e.Devcrate("up"); r.Code != 0 {
				t.Fatalf("up after fixing the %s script exit %d", script, r.Code)
			}
			if r := e.Devcrate("status"); r.Stdout != "ready\n" {
				t.Errorf("status after fixing = %q, want ready", r.Stdout)
			}
			// The log holds only the latest setup.
			r = e.Devcrate("ssh", "--", "cat /var/lib/devcrate/setup.log")
			if !strings.Contains(r.Stdout, "fixed\n") || strings.Contains(r.Stdout, "before-failing") {
				t.Errorf("setup.log after fixing = %q, want only the latest setup", r.Stdout)
			}
		})
	}
}

func TestUserSetupScriptInContainer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		repo, user    string
		devcontainer  bool
		wantContainer bool // else on the VM
	}{
		{name: "user config", user: "setup_script_in_container = true\n", devcontainer: true, wantContainer: true},
		{name: "ignored when VM-only", user: "setup_script_in_container = true\n"},
		{name: "ignored in repo config", repo: "setup_script_in_container = true\n", devcontainer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			if tc.devcontainer {
				e.withDevcontainer()
			}
			if tc.repo != "" {
				e.WriteFile(".devcrate.toml", "provider = \"fake\"\n"+tc.repo)
			}
			if tc.devcontainer || tc.repo != "" {
				e.Push()
			}
			if tc.user != "" {
				e.UserConfig(tc.user)
			}
			e.UserSetupScript("grep '^ID=' /etc/os-release >> /user-ran\n")

			if r := e.Devcrate("up"); r.Code != 0 {
				t.Fatalf("up exit %d", r.Code)
			}
			vm, _ := e.VM("cat /user-ran 2>/dev/null")
			var container string
			if tc.devcontainer {
				container = e.Devcrate("ssh", "--container", "--", "cat /user-ran 2>/dev/null").Stdout
			}
			if tc.wantContainer && (container != "ID=alpine\n" || vm != "") {
				t.Errorf("user script ran in the container %q and on the VM %q, want only in the container", container, vm)
			}
			if !tc.wantContainer && (vm != "ID=ubuntu\n" || container != "") {
				t.Errorf("user script ran on the VM %q and in the container %q, want only on the VM", vm, container)
			}
		})
	}
}
