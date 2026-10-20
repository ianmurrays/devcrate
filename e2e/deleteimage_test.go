//go:build e2e

package e2e

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestSaveImageDeleteRemovesTheBaseImageAndTouchesNoEnvironment(t *testing.T) {
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
	// One Environment Ready on the Base image, one Stopped.
	if r := w.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up in the linked worktree exit %d", r.Code)
	}
	container := w.ContainerID()

	if r := w.Devcrate("save-image", "--delete"); r.Code != 0 {
		t.Fatalf("save-image --delete exit %d stderr %q", r.Code, r.Stderr)
	}
	if imgs := e.BaseImages(); len(imgs) != 0 {
		t.Errorf("Base images after save-image --delete = %v, want none", imgs)
	}
	if r := w.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("worktree status after save-image --delete = %q, want ready", r.Stdout)
	}
	if got := w.ContainerID(); got != container {
		t.Errorf("save-image --delete replaced the worktree's VM %s with %q", container, got)
	}
	if r := e.Devcrate("status"); r.Stdout != "stopped\n" {
		t.Errorf("status after save-image --delete = %q, want stopped", r.Stdout)
	}

	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up after save-image --delete exit %d", r.Code)
	}
	if out, _ := e.VM("cat /var/lib/devcrate/installs"); out != "installed\ninstalled\n" {
		t.Errorf("install branch runs = %q, want two: up booted from the deleted Base image", out)
	}
}

func TestSaveImageDeleteWithoutABaseImageSucceeds(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	r := e.Devcrate("save-image", "--delete")
	if r.Code != 0 || strings.Contains(r.Stderr, "fake:") {
		t.Errorf("save-image --delete with no Base image = exit %d stderr %q, want success", r.Code, r.Stderr)
	}
}

func TestDeleteOfAWorktreeEnvironmentLeavesTheBaseImage(t *testing.T) {
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
	if r := w.Devcrate("delete", "--yes"); r.Code != 0 {
		t.Fatalf("delete in the linked worktree exit %d", r.Code)
	}
	if imgs := e.BaseImages(); len(imgs) != 1 || imgs[0] != saved[0] {
		t.Errorf("Base images after deleting the worktree Environment = %v, want %v", imgs, saved)
	}
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up after the worktree's delete exit %d", r.Code)
	}
	if out, _ := e.VM("cat /var/lib/devcrate/installs"); out != "installed\n" {
		t.Errorf("install branch runs = %q, want one: up did not boot from the Base image", out)
	}
}

func TestStatusShowsTheBaseImage(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status without a Base image = %q, want only ready", r.Stdout)
	}
	if r := e.Devcrate("status", "--json"); r.Stdout != `{"name":"`+e.Name+`","state":"ready"}`+"\n" {
		t.Errorf("status --json without a Base image = %q, want no base_image", r.Stdout)
	}
	if r := e.Devcrate("save-image"); r.Code != 0 {
		t.Fatalf("save-image exit %d", r.Code)
	}
	image := e.BaseImages()[0]
	out, err := exec.Command("docker", "image", "inspect", "-f", "{{.Created}}", image).Output()
	if err != nil {
		t.Fatalf("docker image inspect: %v", err)
	}
	created := strings.TrimSpace(string(out))

	if r := e.Devcrate("status"); r.Stdout != "stopped\nBase image: "+image+" (created "+created+")\n" {
		t.Errorf("status with a Base image = %q, want it after the state", r.Stdout)
	}
	var got struct {
		State     string `json:"state"`
		BaseImage struct {
			ID      string `json:"id"`
			Created string `json:"created"`
		} `json:"base_image"`
	}
	r := e.Devcrate("status", "--json")
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil {
		t.Fatalf("status --json = %q: %v", r.Stdout, err)
	}
	if got.State != "stopped" || got.BaseImage.ID != image || got.BaseImage.Created != created {
		t.Errorf("status --json = %q, want stopped with base_image %s created %s", r.Stdout, image, created)
	}
}

func TestStatusAndSaveImageDeleteOutsideTheCloneUseTheVolumesBaseName(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	w := e.Worktree("feat", e.Name+"-feat")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("save-image"); r.Code != 0 {
		t.Fatalf("save-image exit %d", r.Code)
	}
	image := e.BaseImages()[0]
	if r := w.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up in the linked worktree exit %d", r.Code)
	}
	elsewhere := t.TempDir()

	if r := e.DevcrateIn(elsewhere, "status", "--provider", "fake", w.Name); !strings.Contains(r.Stdout, image) {
		t.Errorf("status of the worktree Environment outside the clone = %q, want its repo's Base image %s", r.Stdout, image)
	}
	if r := e.DevcrateIn(elsewhere, "save-image", "--delete", "--provider", "fake", w.Name); r.Code != 0 || !strings.Contains(r.Stdout, "deleted the Base image of "+e.Name) {
		t.Errorf("save-image --delete of the worktree Environment outside the clone = exit %d stdout %q, want the Base image of %s deleted", r.Code, r.Stdout, e.Name)
	}
	if imgs := e.BaseImages(); len(imgs) != 0 {
		t.Errorf("Base images after save-image --delete outside the clone = %v, want none", imgs)
	}
}
