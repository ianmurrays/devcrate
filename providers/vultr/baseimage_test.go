package vultr

import (
	"slices"
	"strings"
	"testing"
)

// label is how the Provider labels base_name's Base image: the snapshot's
// description.
func label(baseName string) string { return "devcrate-base:" + baseName }

func TestSaveImageSnapshotsTheHaltedInstanceAndReplacesTheBaseImage(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api-wt", opts, nil)
	inst := f.api.instances[0]
	old := f.api.addSnapshot(label("api"), "complete")
	other := f.api.addSnapshot(label("web"), "complete")
	mine := f.api.addSnapshot("my own snapshot", "complete")

	f.ok("save-image", "api-wt", opts, nil, map[string]any{"base_name": "api"})

	var saved []*snapshot
	for _, s := range f.api.snapshots {
		if s.Description == label("api") {
			saved = append(saved, s)
		}
	}
	if len(saved) != 1 || saved[0].ID == old.ID || saved[0].Status != "complete" || saved[0].instance != inst.ID || !saved[0].halted {
		t.Fatalf("api's Base images = %+v, want one new complete snapshot of the halted instance %s", saved, inst.ID)
	}
	if !slices.Equal(f.api.deleted, []deletion{{ID: old.ID, NewerComplete: true}}) {
		t.Errorf("deleted snapshots = %+v, want only the old Base image %s, once the new one was complete", f.api.deleted, old.ID)
	}
	if f.api.snapshot(other.ID) == nil || f.api.snapshot(mine.ID) == nil {
		t.Errorf("save-image deleted a snapshot that is not api's Base image")
	}
	if len(f.api.instances) != 0 || len(f.api.blocks) != 1 {
		t.Errorf("after save-image: %d instances and %d volumes, want 0 and 1", len(f.api.instances), len(f.api.blocks))
	}
	var s upResult
	f.ok("status", "api-wt", opts, &s)
	if s.State != "stopped" {
		t.Errorf("status = %+v, want stopped", s)
	}
}

func TestAFailedSnapshotLeavesTheOldBaseImage(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	old := f.api.addSnapshot(label("api"), "complete")
	f.api.snapshotFails = true

	r := f.run("save-image", "api", opts)
	if r.Code == 0 || !strings.Contains(r.Stderr, "Unable to create snapshot: the server is locked.") {
		t.Fatalf("save-image = exit %d, stderr %q; want a failure with the API's error", r.Code, r.Stderr)
	}
	if len(f.api.snapshots) != 1 || f.api.snapshots[0] != old || len(f.api.deleted) != 0 {
		t.Errorf("snapshots = %+v, deleted %+v; want only the old Base image, untouched", f.api.snapshots, f.api.deleted)
	}
	// The halted instance stays, so up starts it again.
	if len(f.api.instances) != 1 {
		t.Fatalf("%d instances, want the halted one", len(f.api.instances))
	}
	var s upResult
	f.ok("up", "api", opts, &s)
	if s.State != "running" || len(f.api.instances) != 1 {
		t.Errorf("up after a failed save = %+v with %d instances, want the same instance running", s, len(f.api.instances))
	}
}

func TestASaveDeletesEveryOlderBaseImageButNotANewerOne(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	older1 := f.api.addSnapshot(label("api"), "complete")
	older2 := f.api.addSnapshot(label("api"), "pending")
	f.ok("save-image", "api", opts, nil)
	if len(f.api.deleted) != 2 || f.api.snapshot(older1.ID) != nil || f.api.snapshot(older2.ID) != nil {
		t.Errorf("deleted %+v, want both older Base images", f.api.deleted)
	}
}

func TestUpBootsFromTheNewestBaseImageOverTheImageOption(t *testing.T) {
	f := newFixture(t)
	f.api.addSnapshot(label("api"), "complete")
	newest := f.api.addSnapshot(label("api"), "complete")
	f.api.addSnapshot(label("api"), "pending") // not usable yet
	f.api.addSnapshot(label("web"), "complete")
	f.ok("up", "api-wt", map[string]any{"region": "ewr", "image": "shared-snapshot"}, nil, map[string]any{"base_name": "api"})
	if i := f.api.instances[0]; i.SnapshotID != newest.ID || i.OSID != 0 {
		t.Errorf("instance booted from snapshot %q, os %d; want the newest complete Base image %s", i.SnapshotID, i.OSID, newest.ID)
	}
}

func TestUpWithoutABaseImageUsesTheImageOptionThenUbuntu(t *testing.T) {
	f := newFixture(t)
	f.api.addSnapshot(label("web"), "complete")
	f.ok("up", "api", map[string]any{"region": "ewr", "image": "shared-snapshot"}, nil)
	f.ok("up", "db", opts, nil)
	if i := f.api.instances[0]; i.SnapshotID != "shared-snapshot" {
		t.Errorf("api booted from snapshot %q, want the image option", i.SnapshotID)
	}
	if i := f.api.instances[1]; i.SnapshotID != "" || i.OSID != 2284 {
		t.Errorf("db booted from snapshot %q, os %d; want Ubuntu 24.04 LTS (2284)", i.SnapshotID, i.OSID)
	}
}

func TestSaveImageCheckMakesNoChangingCall(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	before := len(f.api.calls)
	f.ok("save-image", "api", opts, nil, map[string]any{"check": true})
	for _, c := range f.api.calls[before:] {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("save-image with check made %s", c)
		}
	}
	if len(f.api.instances) != 1 || len(f.api.snapshots) != 0 {
		t.Errorf("after the check: %d instances and %d snapshots, want 1 and 0", len(f.api.instances), len(f.api.snapshots))
	}
	f.env = nil
	if r := f.run("save-image", "api", opts, map[string]any{"check": true}); r.Code == 0 || !strings.Contains(r.Stderr, "no API key") {
		t.Errorf("check without an API key = exit %d, stderr %q; want the missing key", r.Code, r.Stderr)
	}
}

func TestSaveImageDeleteRemovesOnlyTheBaseImage(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.api.addSnapshot(label("api"), "complete")
	f.api.addSnapshot(label("api"), "pending")
	other := f.api.addSnapshot(label("web"), "complete")
	mine := f.api.addSnapshot("my own snapshot", "complete")
	f.ok("save-image", "api", opts, nil, map[string]any{"delete": true})
	if len(f.api.snapshots) != 2 || f.api.snapshot(other.ID) == nil || f.api.snapshot(mine.ID) == nil {
		t.Errorf("snapshots after delete = %+v, want only web's Base image and my own", f.api.snapshots)
	}
	var s upResult
	f.ok("status", "api", opts, &s)
	if len(f.api.instances) != 1 || len(f.api.blocks) != 1 || s.State != "running" {
		t.Errorf("delete touched the Environment: %d instances, %d volumes, %+v", len(f.api.instances), len(f.api.blocks), s)
	}
}

func TestStatusShowsTheBaseImageUpBootsFrom(t *testing.T) {
	f := newFixture(t)
	var s struct {
		State     string
		BaseImage *struct{ ID, Created string } `json:"base_image"`
	}
	f.ok("status", "api", opts, &s)
	if s.State != "not_found" || s.BaseImage != nil {
		t.Errorf("status with no Base image = %+v", s)
	}
	f.api.addSnapshot(label("api"), "complete")
	newest := f.api.addSnapshot(label("api"), "complete")
	f.api.addSnapshot(label("api"), "pending")
	f.ok("status", "api", opts, &s)
	if s.BaseImage == nil || s.BaseImage.ID != newest.ID || s.BaseImage.Created != newest.DateCreated {
		t.Errorf("status base_image = %+v, want %s created %s", s.BaseImage, newest.ID, newest.DateCreated)
	}
}
