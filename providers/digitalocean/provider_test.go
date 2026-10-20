// Package digitalocean tests devcrate-provider-digitalocean, devcrate's
// DigitalOcean Provider (a shell script next to this file), through the
// Provider contract (ADR 0001) against a fake cloud.
package digitalocean

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "doctl":
		os.Exit(fakeDoctl(os.Args[1:]))
	case "curl":
		os.Exit(fakeCurl(os.Args[1:]))
	}
	os.Exit(m.Run())
}

const pubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyExampleKeyExampleKeyExample devcrate"

const userData = "#!/bin/sh\n# devcrate user data\nset -eu\necho booted\n"

type fixture struct {
	t     *testing.T
	state string // the fake cloud's state file
	bin   string // fake doctl and curl
	path  string // PATH the Provider runs with
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, state: filepath.Join(dir, "cloud.json"), bin: filepath.Join(dir, "bin")}
	if err := os.Mkdir(f.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"doctl", "curl"} {
		if err := os.Symlink(self, filepath.Join(f.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	f.path = f.bin + ":" + os.Getenv("PATH")
	return f
}

type result struct {
	Stdout, Stderr string
	Code           int
}

// run runs the Provider verb with a request for Environment env, the base
// Environment of its repo.
func (f *fixture) run(verb, env string, options map[string]any, extraEnv ...string) result {
	f.t.Helper()
	return f.request(verb, map[string]any{"environment_id": env, "base_name": env, "options": options}, extraEnv...)
}

// request runs the Provider verb with req, completed with devcrate's key
// and user data.
func (f *fixture) request(verb string, req map[string]any, extraEnv ...string) result {
	f.t.Helper()
	if req["options"] == nil {
		req["options"] = map[string]any{}
	}
	req["ssh_public_key"], req["user_data"] = pubKey, userData
	env := req["environment_id"]
	body, _ := json.Marshal(req)
	cmd := exec.Command("./devcrate-provider-digitalocean", verb)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(), append([]string{"PATH=" + f.path, "FAKE_CLOUD=" + f.state}, extraEnv...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := result{stdout.String(), stderr.String(), 0}
	if exit, ok := err.(*exec.ExitError); ok {
		r.Code = exit.ExitCode()
	} else if err != nil {
		f.t.Fatal(err)
	}
	f.t.Logf("%s %s: exit %d\nstdout: %s\nstderr: %s", verb, env, r.Code, r.Stdout, r.Stderr)
	return r
}

// ok runs a verb that must succeed and decodes its stdout into out (if non-nil).
func (f *fixture) ok(verb, env string, options map[string]any, out any) {
	f.t.Helper()
	r := f.run(verb, env, options)
	if r.Code != 0 {
		f.t.Fatalf("%s failed: %s", verb, r.Stderr)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(r.Stdout), out); err != nil {
			f.t.Fatalf("%s: stdout is not JSON: %q", verb, r.Stdout)
		}
	}
}

func (f *fixture) cloud() *cloud { return loadCloud(f.state) }

type upResult struct {
	State       string            `json:"state"`
	Host        string            `json:"host"`
	User        string            `json:"user"`
	Port        int               `json:"port"`
	SelfStopEnv map[string]string `json:"self_stop_env"`
}

var opts = map[string]any{"image": "123456789", "token": "dop_v1_selfstop"}

func TestUpCreatesVolumeThenDropletWithIt(t *testing.T) {
	f := newFixture(t)
	var res upResult
	f.ok("up", "api", opts, &res)

	c := f.cloud()
	if len(c.Volumes) != 1 || len(c.Droplets) != 1 {
		t.Fatalf("cloud has %d volumes and %d droplets, want 1 and 1", len(c.Volumes), len(c.Droplets))
	}
	v, d := c.Volumes[0], c.Droplets[0]
	if v.Name != "devcrate-api" || !slices.Equal(v.Tags, []string{"devcrate:api"}) || v.SizeGiB != 50 || v.Region.Slug != "nyc3" {
		t.Errorf("volume = %+v, want devcrate-api tagged devcrate:api, 50 GiB, in nyc3", v)
	}
	if d.Name != "devcrate-api" || !slices.Equal(d.Tags, []string{"devcrate:api"}) || d.Region != "nyc3" || d.Size != "s-2vcpu-4gb" || d.Image != "123456789" {
		t.Errorf("droplet = %+v, want devcrate-api tagged devcrate:api, s-2vcpu-4gb in nyc3 from image 123456789", d)
	}
	if !slices.Equal(d.Volumes, []string{v.ID}) {
		t.Errorf("droplet volumes = %v, want [%s]", d.Volumes, v.ID)
	}

	// devcrate's public key is registered once and given to the droplet.
	if len(c.SSHKeys) != 1 || !strings.HasPrefix(pubKey, c.SSHKeys[0].PublicKey) || !slices.Equal(d.SSHKeys, []string{itoa(c.SSHKeys[0].ID)}) {
		t.Errorf("ssh keys = %+v, droplet's = %v", c.SSHKeys, d.SSHKeys)
	}

	want := upResult{State: "running", Host: "203.0.113." + itoa(d.ID%250), User: "root", Port: 22,
		SelfStopEnv: map[string]string{"DIGITALOCEAN_TOKEN": "dop_v1_selfstop"}}
	if mustJSON(res) != mustJSON(want) {
		t.Errorf("up = %s, want %s", mustJSON(res), mustJSON(want))
	}
}

// The droplet's user data mounts the volume at /var/lib/devcrate with an
// fstab entry (the Agent re-mounts with `mount /var/lib/devcrate` after a
// failed Self-stop), then runs devcrate's user data unchanged.
func TestUserDataMountsTheVolumeThenRunsDevcratesUserData(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	ud := f.cloud().UserData["devcrate-api"]
	mount, devcrate, ok := strings.Cut(ud, userData)
	if !ok || strings.TrimSpace(devcrate) != "" {
		t.Fatalf("user data does not end with devcrate's:\n%s", ud)
	}
	for _, want := range []string{
		"dev=/dev/disk/by-id/scsi-0DO_Volume_devcrate-api",
		`echo "$dev /var/lib/devcrate ext4 defaults,nofail,discard,noatime 0 2" >> /etc/fstab`,
		"mount /var/lib/devcrate",
	} {
		if !strings.Contains(mount, want) {
			t.Errorf("user data before devcrate's lacks %q:\n%s", want, mount)
		}
	}
	if out, err := exec.Command("sh", "-n", "-c", ud).CombinedOutput(); err != nil {
		t.Errorf("user data is not valid sh: %s", out)
	}
}

func TestUpIsIdempotent(t *testing.T) {
	f := newFixture(t)
	var first, second upResult
	f.ok("up", "api", opts, &first)
	f.ok("up", "api", opts, &second)
	c := f.cloud()
	if len(c.Volumes) != 1 || len(c.Droplets) != 1 || len(c.SSHKeys) != 1 {
		t.Errorf("after two ups: %d volumes, %d droplets, %d ssh keys; want 1 each", len(c.Volumes), len(c.Droplets), len(c.SSHKeys))
	}
	if mustJSON(first) != mustJSON(second) {
		t.Errorf("second up = %s, want %s", mustJSON(second), mustJSON(first))
	}
}

func TestStopKeepsTheVolumeAndUpReattachesIt(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	vol := f.cloud().Volumes[0].ID

	f.ok("stop", "api", opts, nil)
	f.ok("stop", "api", opts, nil) // idempotent
	if c := f.cloud(); len(c.Droplets) != 0 || len(c.Volumes) != 1 {
		t.Fatalf("after stop: %d droplets, %d volumes; want 0 and 1", len(c.Droplets), len(c.Volumes))
	}
	var st upResult
	f.ok("status", "api", opts, &st)
	if st.State != "stopped" {
		t.Errorf("status after stop = %+v, want stopped", st)
	}

	var res upResult
	f.ok("up", "api", opts, &res)
	c := f.cloud()
	if len(c.Volumes) != 1 || len(c.Droplets) != 1 || !slices.Equal(c.Droplets[0].Volumes, []string{vol}) {
		t.Errorf("after resume: volumes %+v, droplets %+v; want the old volume %s on a new droplet", c.Volumes, c.Droplets, vol)
	}
	if res.State != "running" {
		t.Errorf("up after stop = %+v", res)
	}
}

func TestDeleteLeavesNoDropletVolumeOrTag(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.ok("up", "web", opts, nil)

	f.ok("delete", "api", opts, nil)
	f.ok("delete", "api", opts, nil) // idempotent
	c := f.cloud()
	for _, d := range c.Droplets {
		if d.Name == "devcrate-api" {
			t.Errorf("droplet left behind: %+v", d)
		}
	}
	for _, v := range c.Volumes {
		if v.Name == "devcrate-api" {
			t.Errorf("volume left behind: %+v", v)
		}
	}
	if !slices.Equal(c.Tags, []string{"devcrate:web"}) {
		t.Errorf("tags = %v, want only devcrate:web", c.Tags)
	}
	var st upResult
	f.ok("status", "api", opts, &st)
	if st.State != "not_found" {
		t.Errorf("status after delete = %+v, want not_found", st)
	}
}

func TestListReadsEveryEnvironmentByTag(t *testing.T) {
	f := newFixture(t)
	var empty []map[string]string
	f.ok("list", "", nil, &empty)
	if empty == nil || len(empty) != 0 {
		t.Errorf("list on an empty account = %v, want []", empty)
	}

	f.ok("up", "api", opts, nil)
	f.ok("up", "web", opts, nil)
	f.ok("stop", "web", opts, nil)
	var items []map[string]string
	f.ok("list", "", nil, &items)
	want := []map[string]string{{"environment_id": "api", "state": "running"}, {"environment_id": "web", "state": "stopped"}}
	if mustJSON(items) != mustJSON(want) {
		t.Errorf("list = %s, want %s", mustJSON(items), mustJSON(want))
	}
}

// pathWithout is a PATH holding links to every command in /bin and /usr/bin
// except the named ones (macOS ships /usr/bin/jq).
func pathWithout(t *testing.T, except ...string) string {
	dir := t.TempDir()
	for _, sys := range []string{"/bin", "/usr/bin"} {
		entries, _ := os.ReadDir(sys)
		for _, e := range entries {
			if slices.Contains(except, e.Name()) {
				continue
			}
			os.Symlink(filepath.Join(sys, e.Name()), filepath.Join(dir, e.Name())) // first one wins
		}
	}
	return dir
}

func TestMissingDoctlOrJqIsAClearError(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not installed here")
	}
	f := newFixture(t)
	f.path = pathWithout(t, "doctl", "jq") + ":" + filepath.Dir(jq)
	r := f.run("status", "api", opts)
	if r.Code == 0 || !strings.Contains(r.Stderr, "digitalocean: doctl is not installed (or not on PATH): install it and run doctl auth init") {
		t.Errorf("without doctl: exit %d, stderr %q", r.Code, r.Stderr)
	}

	f.path = f.bin + ":" + pathWithout(t, "jq")
	r = f.run("status", "api", opts)
	if r.Code == 0 || !strings.Contains(r.Stderr, "digitalocean: jq is not installed (or not on PATH)") {
		t.Errorf("without jq: exit %d, stderr %q", r.Code, r.Stderr)
	}
	if len(f.cloud().Calls) != 0 {
		t.Errorf("calls made without the tools: %v", f.cloud().Calls)
	}
}

// A missing image fails up with doctl's own error, with no fallback image.
func TestUpWithoutImageFailsWithDoctlsError(t *testing.T) {
	f := newFixture(t)
	r := f.run("up", "api", map[string]any{"token": "t"})
	if r.Code == 0 || !strings.Contains(r.Stderr, "(droplet.create.image) command is missing required arguments") {
		t.Errorf("exit %d, stderr %q", r.Code, r.Stderr)
	}
	if len(f.cloud().Droplets) != 0 {
		t.Errorf("droplets = %+v, want none", f.cloud().Droplets)
	}
}

func TestOptionsReachDoctl(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", map[string]any{"image": "987", "region": "ams3", "size": "s-4vcpu-8gb", "volume_size": 100}, nil)
	c := f.cloud()
	if v, d := c.Volumes[0], c.Droplets[0]; v.SizeGiB != 100 || v.Region.Slug != "ams3" || d.Region != "ams3" || d.Size != "s-4vcpu-8gb" || d.Image != "987" {
		t.Errorf("volume %+v, droplet %+v", v, d)
	}
}

// Without a token there is no Self-stop credential; devcrate then refuses
// idle stop with a message pointing at the README.
func TestUpWithoutTokenReturnsNoSelfStopEnv(t *testing.T) {
	f := newFixture(t)
	r := f.run("up", "api", map[string]any{"image": "1"})
	if r.Code != 0 || strings.Contains(r.Stdout, "self_stop_env") {
		t.Errorf("exit %d, stdout %q", r.Code, r.Stdout)
	}
}

// devcrate asks for the self-stop script before up, so without a token it
// fails before anything is created.
func TestSelfStopWithoutTokenFails(t *testing.T) {
	f := newFixture(t)
	r := f.run("self-stop", "api", map[string]any{"image": "1"})
	if r.Code == 0 || !strings.Contains(r.Stderr, "digitalocean: no Self-stop token: set token under [providers.digitalocean]") {
		t.Errorf("exit %d, stderr %q", r.Code, r.Stderr)
	}
	if len(f.cloud().Calls) != 0 {
		t.Errorf("calls made: %v", f.cloud().Calls)
	}
}

// The self-stop script, run on the VM with self_stop_env, destroys the
// droplet it runs on (found through the metadata service) and nothing else.
func TestSelfStopDeletesOnlyItsOwnDroplet(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.ok("up", "web", opts, nil)
	var api *droplet
	for _, d := range f.cloud().Droplets {
		if d.Name == "devcrate-api" {
			api = d
		}
	}

	var out struct{ Script string }
	f.ok("self-stop", "api", opts, &out)
	script := filepath.Join(t.TempDir(), "self-stop")
	if err := os.WriteFile(script, []byte(out.Script), 0o755); err != nil {
		t.Fatal(err)
	}
	before := len(f.cloud().Calls)
	// As the Agent runs it: only self_stop_env, plus PATH.
	cmd := exec.Command(script)
	cmd.Env = []string{"PATH=" + f.path, "DIGITALOCEAN_TOKEN=dop_v1_selfstop",
		"FAKE_CLOUD=" + f.state, "FAKE_DROPLET_ID=" + itoa(api.ID), "FAKE_API_TOKEN=dop_v1_selfstop"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("self-stop: %v: %s", err, out)
	}

	c := f.cloud()
	if len(c.Droplets) != 1 || c.Droplets[0].Name != "devcrate-web" {
		t.Errorf("droplets after self-stop = %+v, want only devcrate-web", c.Droplets)
	}
	if len(c.Volumes) != 2 {
		t.Errorf("volumes after self-stop = %+v, want both", c.Volumes)
	}
	calls := c.Calls[before:]
	if len(calls) != 2 || calls[0][0] != "curl" || calls[1][0] != "curl" {
		t.Errorf("self-stop calls = %v, want a metadata read and one DELETE", calls)
	}

	// A wrong token fails the script (the Agent then recovers and retries).
	f.ok("up", "api", opts, nil)
	cmd = exec.Command(script)
	cmd.Env = []string{"PATH=" + f.path, "DIGITALOCEAN_TOKEN=wrong",
		"FAKE_CLOUD=" + f.state, "FAKE_DROPLET_ID=" + itoa(f.cloud().Droplets[1].ID), "FAKE_API_TOKEN=dop_v1_selfstop"}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("self-stop with a wrong token succeeded: %s", out)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// saveImage runs save-image for Environment env of base Environment base.
func (f *fixture) saveImage(env, base string, flags map[string]any) result {
	f.t.Helper()
	req := map[string]any{"environment_id": env, "base_name": base, "options": opts}
	maps.Copy(req, flags)
	return f.request("save-image", req)
}

// save-image powers the droplet off, snapshots it, waits for the snapshot,
// labels it as base's Base image, deletes every older one only then, and
// destroys the droplet, keeping the volume.
func TestSaveImageReplacesTheBaseImageAndStops(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.ok("up", "web", opts, nil) // another repo's Environment and Base image
	if r := f.saveImage("web", "web", nil); r.Code != 0 || r.Stdout != "" {
		t.Fatalf("save-image web: exit %d, stdout %q", r.Code, r.Stdout)
	}
	f.ok("up", "web", opts, nil)
	web := f.cloud().Snapshots

	for range 2 {
		if r := f.saveImage("api", "api", nil); r.Code != 0 || r.Stdout != "" {
			t.Fatalf("save-image: exit %d, stdout %q", r.Code, r.Stdout)
		}
		f.ok("up", "api", opts, nil)
	}

	c := f.cloud()
	var api []*snapshot
	for _, s := range c.Snapshots {
		if !slices.ContainsFunc(web, func(w *snapshot) bool { return w.ID == s.ID }) {
			api = append(api, s)
		}
	}
	if len(c.Snapshots) != 2 || len(api) != 1 {
		t.Fatalf("snapshots = %s, want web's and api's newest only", mustJSON(c.Snapshots))
	}
	// The newest is the one the second save took, after the first was used.
	var d *droplet
	for _, x := range c.Droplets {
		if x.Name == "devcrate-api" {
			d = x
		}
	}
	if d.Image != api[0].ID {
		t.Errorf("api's droplet booted from %s, want its Base image %s", d.Image, api[0].ID)
	}
	if len(c.Volumes) != 2 || !slices.Equal(d.Volumes, []string{c.Volumes[0].ID}) {
		t.Errorf("volumes = %s, droplet's = %v; want api's volume kept and reattached", mustJSON(c.Volumes), d.Volumes)
	}
}

// A snapshot that fails leaves the previous Base image, and the droplet off
// for devcrate up to power on again.
func TestFailedSaveImageKeepsThePreviousBaseImage(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.saveImage("api", "api", nil)
	f.ok("up", "api", opts, nil)
	before := f.cloud().Snapshots

	req := map[string]any{"environment_id": "api", "base_name": "api", "options": opts}
	r := f.request("save-image", req, "FAKE_DOCTL_FAIL=droplet-action snapshot")
	if r.Code == 0 || !strings.Contains(r.Stderr, "Error: failing as asked") {
		t.Fatalf("save-image with a failing snapshot: exit %d, stderr %q", r.Code, r.Stderr)
	}
	if c := f.cloud(); mustJSON(c.Snapshots) != mustJSON(before) || len(c.Droplets) != 1 || c.Droplets[0].Status != "off" {
		t.Errorf("after a failed save: snapshots %s (want %s), droplets %s", mustJSON(c.Snapshots), mustJSON(before), mustJSON(c.Droplets))
	}
	var res upResult
	f.ok("up", "api", opts, &res)
	if res.State != "running" {
		t.Errorf("up after a failed save = %+v, want running", res)
	}
}

// A worktree Environment in another region than its repo's Base image warns
// and boots from the image option instead.
func TestUpFallsBackToImageWhenTheBaseImageIsInAnotherRegion(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.saveImage("api", "api", nil)
	base := f.cloud().Snapshots[0].ID

	ams := map[string]any{"image": "123456789", "token": "t", "region": "ams3"}
	r := f.request("up", map[string]any{"environment_id": "api-feature", "base_name": "api", "options": ams})
	if r.Code != 0 || !strings.Contains(r.Stderr, "digitalocean: warning: the Base image "+base+" is not available in ams3, using the image option") {
		t.Fatalf("up in ams3: exit %d, stderr %q", r.Code, r.Stderr)
	}
	if d := f.cloud().Droplets[0]; d.Name != "devcrate-api-feature" || d.Image != "123456789" {
		t.Errorf("droplet = %+v, want devcrate-api-feature from image 123456789", d)
	}

	// In the Base image's region the worktree Environment boots from it.
	r = f.request("up", map[string]any{"environment_id": "api-other", "base_name": "api", "options": opts})
	c := f.cloud()
	if r.Code != 0 || strings.Contains(r.Stderr, "warning") || c.Droplets[1].Image != base {
		t.Errorf("up in nyc3: exit %d, stderr %q, droplet %+v; want Base image %s", r.Code, r.Stderr, c.Droplets[1], base)
	}
}

// changing reports whether a doctl call can change the account.
func changing(call []string) bool {
	return !slices.ContainsFunc(call, func(a string) bool { return a == "list" || a == "get" })
}

// save-image with check makes no changing call; with delete it removes only
// the Base image.
func TestSaveImageCheckAndDelete(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.ok("up", "web", opts, nil)
	f.saveImage("web", "web", nil)
	f.ok("up", "web", opts, nil)
	f.saveImage("api", "api", nil)
	f.ok("up", "api", opts, nil)
	before := f.cloud()

	if r := f.saveImage("api", "api", map[string]any{"check": true}); r.Code != 0 || r.Stdout != "" {
		t.Fatalf("check: exit %d, stdout %q", r.Code, r.Stdout)
	}
	if calls := f.cloud().Calls[len(before.Calls):]; slices.ContainsFunc(calls, changing) {
		t.Errorf("check made changing calls: %v", calls)
	}

	if r := f.saveImage("api", "api", map[string]any{"delete": true}); r.Code != 0 || r.Stdout != "" {
		t.Fatalf("delete: exit %d, stdout %q", r.Code, r.Stdout)
	}
	c := f.cloud()
	if len(c.Snapshots) != 1 || c.Snapshots[0].Name != "devcrate-base-web" {
		t.Errorf("snapshots after delete = %s, want only web's", mustJSON(c.Snapshots))
	}
	if mustJSON(c.Droplets) != mustJSON(before.Droplets) || mustJSON(c.Volumes) != mustJSON(before.Volumes) {
		t.Errorf("delete changed Environments: droplets %s, volumes %s", mustJSON(c.Droplets), mustJSON(c.Volumes))
	}
	if r := f.saveImage("api", "api", map[string]any{"delete": true}); r.Code != 0 {
		t.Errorf("second delete: exit %d, want 0 (idempotent)", r.Code)
	}
}

// status reports the Base image up boots from, so its storage bill is
// visible; list stays as it was.
func TestStatusShowsTheBaseImage(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	var st map[string]any
	f.ok("status", "api", opts, &st)
	if _, ok := st["base_image"]; ok {
		t.Errorf("status without a Base image = %v", st)
	}

	f.saveImage("api", "api", nil)
	s := f.cloud().Snapshots[0]
	st = nil
	f.ok("status", "api", opts, &st)
	want := map[string]any{"state": "stopped", "base_image": map[string]any{"id": s.ID, "created": s.Created}}
	if mustJSON(st) != mustJSON(want) {
		t.Errorf("status = %s, want %s", mustJSON(st), mustJSON(want))
	}
	var items []map[string]string
	f.ok("list", "", nil, &items)
	if want := `[{"environment_id":"api","state":"stopped"}]`; mustJSON(items) != want {
		t.Errorf("list = %s, want %s", mustJSON(items), want)
	}
}

// A droplet booted from a Base image has no volume line in /etc/fstab (devcrate
// removes it before the save): the user data adds one for the droplet's own
// volume and mounts it. Run with /etc/fstab, /dev and the mount point moved to
// a temporary directory and the disk tools stubbed.
func TestUserDataMountsItsOwnVolumeOnABaseImage(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api-feature", opts, nil)
	mount, _, _ := strings.Cut(f.cloud().UserData["devcrate-api-feature"], userData)

	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")
	if err := os.WriteFile(fstab, []byte("LABEL=cloudimg-rootfs / ext4 discard,errors=remount-ro 0 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(dir, "dev/scsi-0DO_Volume_devcrate-api-feature")
	os.MkdirAll(filepath.Dir(dev), 0o755)
	os.WriteFile(dev, nil, 0o644)
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	stubs := map[string]string{"blkid": "exit 0", "mountpoint": "exit 1", "mount": `echo "$@" >> ` + filepath.Join(dir, "mounted")}
	for tool, body := range stubs {
		os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	}
	script := strings.NewReplacer("/etc/fstab", fstab, "/dev/disk/by-id", filepath.Dir(dev), "/var/lib/devcrate", filepath.Join(dir, "mnt")).Replace(mount)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("user data: %v: %s", err, out)
	}

	got, _ := os.ReadFile(fstab)
	if want := dev + " " + filepath.Join(dir, "mnt") + " ext4 defaults,nofail,discard,noatime 0 2\n"; !strings.HasSuffix(string(got), want) {
		t.Errorf("fstab = %q, want it to end with %q", got, want)
	}
	if mounted, _ := os.ReadFile(filepath.Join(dir, "mounted")); string(mounted) != filepath.Join(dir, "mnt")+"\n" {
		t.Errorf("mounted %q, want the mount point", mounted)
	}
}
