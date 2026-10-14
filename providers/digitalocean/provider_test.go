// Package digitalocean tests devcrate-provider-digitalocean, devcrate's
// DigitalOcean Provider (a shell script next to this file), through the
// Provider contract (ADR 0001) against a fake cloud.
package digitalocean

import (
	"bytes"
	"encoding/json"
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

// run runs the Provider verb with a request for Environment env.
func (f *fixture) run(verb, env string, options map[string]any, extraEnv ...string) result {
	f.t.Helper()
	if options == nil {
		options = map[string]any{}
	}
	req, _ := json.Marshal(map[string]any{
		"environment_id": env, "options": options, "ssh_public_key": pubKey, "user_data": userData,
	})
	cmd := exec.Command("./devcrate-provider-digitalocean", verb)
	cmd.Stdin = bytes.NewReader(req)
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
