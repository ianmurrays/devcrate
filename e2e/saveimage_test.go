//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// BaseImages are the fake Provider's Base images for base name e.Name,
// newest first.
func (e *Env) BaseImages() []string {
	e.t.Helper()
	out, err := exec.Command("docker", "images", "devcrate-fake-base-"+e.Name, "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		e.t.Fatalf("docker images: %v", err)
	}
	return strings.Fields(string(out))
}

func TestSaveImageOnStoppedEnvironmentFails(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("stop"); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	r := e.Devcrate("save-image")
	if want := e.Name + " is not Ready: run devcrate up first"; r.Code != 1 || !strings.Contains(r.Stderr, want) {
		t.Errorf("save-image on a Stopped Environment = exit %d stderr %q, want exit 1 with %q", r.Code, r.Stderr, want)
	}
	if imgs := e.BaseImages(); len(imgs) != 0 {
		t.Errorf("save-image on a Stopped Environment made Base images %v", imgs)
	}
	if r := e.Devcrate("status"); r.Stdout != "stopped\n" {
		t.Errorf("status after save-image = %q, want stopped", r.Stdout)
	}
}

func TestSaveImageFailingCheckLeavesTheEnvironmentReady(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nfail = \"save-image\"\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	r := e.Devcrate("save-image")
	if want := "provider fake does not support save-image: fake: failing save-image as asked"; r.Code != 1 || !strings.Contains(r.Stderr, want) {
		t.Errorf("save-image with a failing check = exit %d stderr %q, want exit 1 with %q", r.Code, r.Stderr, want)
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status after a failed check = %q, want ready", r.Stdout)
	}
	if out, code := e.VM("systemctl is-active devcrate-agent"); code != 0 {
		t.Errorf("the Agent is not running after a failed check: %s", out)
	}
	if imgs := e.BaseImages(); len(imgs) != 0 {
		t.Errorf("a failed check made Base images %v", imgs)
	}
}

// installOnceScript is a repo setup script whose install branch writes a
// marker on the root disk, outside /var/lib/devcrate, and counts its runs on
// the volume.
const installOnceScript = `if [ ! -f /opt/devcrate-e2e-marker ]; then
	touch /opt/devcrate-e2e-marker
	echo installed >> /var/lib/devcrate/installs
fi
`

// withInstallOnceScript makes installOnceScript e's repo setup script.
func (e *Env) withInstallOnceScript() {
	e.t.Helper()
	e.WriteFile("repo-setup.sh", installOnceScript)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nsetup_script = \"repo-setup.sh\"\n")
	e.Push()
}

// scrubbed checks that the Base image holds none of devcrate's state.
func scrubbed(t *testing.T, image string) {
	t.Helper()
	checks := `for f in /usr/local/lib/devcrate /usr/local/bin/devcrate-agent /etc/systemd/system/devcrate-agent.service \
	/etc/systemd/system/multi-user.target.wants/devcrate-agent.service \
	/etc/systemd/system/multi-user.target.wants/docker.service /etc/systemd/system/sockets.target.wants/docker.socket \
	/etc/systemd/system/multi-user.target.wants/containerd.service /root/.ssh/authorized_keys /var/lib/cloud/user-data; do
	if [ -e "$f" ]; then echo "$f is in the Base image"; fi
done
if grep ' /var/lib/devcrate ' /etc/fstab; then echo "the volume's fstab line is in the Base image"; fi`
	out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "sh", image, "-c", checks).CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Errorf("Base image %s is not scrubbed: %v\n%s", image, err, out)
	}
}

func TestSaveImageThenUpBootsFromTheBaseImage(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.withInstallOnceScript()
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	pin := e.ReadHome(".config/devcrate/known_hosts")

	if r := e.Devcrate("save-image"); r.Code != 0 {
		t.Fatalf("save-image exit %d", r.Code)
	}
	if r := e.Devcrate("status"); !strings.HasPrefix(r.Stdout, "stopped\n") {
		t.Errorf("status after save-image = %q, want stopped", r.Stdout)
	}
	if !e.VolumeExists() {
		t.Fatal("save-image removed the volume")
	}
	if got := e.ReadHome(".config/devcrate/known_hosts"); got != pin {
		t.Errorf("save-image changed the host key pin from %q to %q", pin, got)
	}
	imgs := e.BaseImages()
	if len(imgs) != 1 {
		t.Fatalf("Base images after save-image = %v, want one", imgs)
	}
	scrubbed(t, imgs[0])

	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up after save-image exit %d", r.Code)
	}
	if out, _ := e.VM("cat /var/lib/devcrate/installs"); out != "installed\n" {
		t.Errorf("install branch runs = %q, want one: the marker was not in the Base image", out)
	}
	if out, code := e.VM("systemctl is-active devcrate-agent && cmp /usr/local/lib/devcrate/devcrate-agent /var/lib/devcrate/agent/devcrate-agent"); code != 0 {
		t.Errorf("the Agent running is not the one up pushed: %s", out)
	}
	if out, _ := e.VM("docker info -f '{{.DockerRootDir}}'"); out != "/var/lib/devcrate/docker\n" {
		t.Errorf("Docker's data-root = %q, want it on the volume", out)
	}
	e.PlainSSH("true")
}

func TestWorktreeEnvironmentsShareTheBaseImage(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.withInstallOnceScript()
	w := e.Worktree("feat", e.Name+"-feat")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("save-image"); r.Code != 0 {
		t.Fatalf("save-image exit %d", r.Code)
	}
	saved := e.BaseImages()

	if r := w.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up in the linked worktree exit %d", r.Code)
	}
	if out, code := w.VM("test -f /opt/devcrate-e2e-marker && test ! -e /var/lib/devcrate/installs"); code != 0 {
		t.Errorf("the worktree's Environment did not boot from the main Environment's Base image: %s", out)
	}

	// By name outside the clone: the base name comes from the volume.
	e.UserConfig("[providers.fake]\n")
	if r := e.DevcrateIn(e.Home, "save-image", w.Name); r.Code != 0 {
		t.Fatalf("save-image %s outside the clone exit %d", w.Name, r.Code)
	}
	if imgs := e.BaseImages(); len(imgs) != 1 || imgs[0] == saved[0] {
		t.Errorf("Base images after saving from the worktree = %v, want one replacing %v", imgs, saved)
	}
	if imgs := w.BaseImages(); len(imgs) != 0 {
		t.Errorf("saving from the worktree made Base images %v under its own name", imgs)
	}
}

func TestUpUsesTheNewestBaseImageAndSaveKeepsOnlyIt(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("save-image"); r.Code != 0 {
		t.Fatalf("save-image exit %d", r.Code)
	}
	older := e.BaseImages()[0]

	// A second, newer Base image, as an interrupted save or a sibling
	// worktree's save leaves: one save-image would delete the first.
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	e.VM("touch /opt/devcrate-e2e-newest")
	newer := fmt.Sprintf("devcrate-fake-base-%s:%d", e.Name, time.Now().UnixNano())
	commitLocked(func() {
		if out, err := exec.Command("docker", "commit", e.Container(), newer).CombinedOutput(); err != nil {
			t.Fatalf("docker commit: %v\n%s", err, out)
		}
	})
	if r := e.Devcrate("stop"); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up with two Base images exit %d", r.Code)
	}
	if out, code := e.VM("test -f /opt/devcrate-e2e-newest"); code != 0 {
		t.Errorf("up did not boot from the newest Base image: %s", out)
	}

	if r := e.Devcrate("save-image"); r.Code != 0 {
		t.Fatalf("save-image exit %d", r.Code)
	}
	if imgs := e.BaseImages(); len(imgs) != 1 || imgs[0] == older || imgs[0] == newer {
		t.Errorf("Base images after save-image = %v, want only a new one", imgs)
	}
}

func TestSaveImageFailingAfterTheScrubIsRecoveredByUp(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nsave_image_fails = true\n")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	r := e.Devcrate("save-image")
	for _, want := range []string{"fake: snapshot failing as asked", "run devcrate up to recover " + e.Name} {
		if r.Code != 1 || !strings.Contains(r.Stderr, want) {
			t.Errorf("failing save-image = exit %d stderr %q, want exit 1 with %q", r.Code, r.Stderr, want)
		}
	}
	if imgs := e.BaseImages(); len(imgs) != 0 {
		t.Errorf("a failed save made Base images %v", imgs)
	}

	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up after a failed save exit %d", r.Code)
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status after up = %q, want ready", r.Stdout)
	}
	if out, code := e.VM("systemctl is-active devcrate-agent"); code != 0 {
		t.Errorf("the Agent is not running after up: %s", out)
	}
}
