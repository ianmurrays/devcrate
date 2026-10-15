// Package vultr tests devcrate-provider-vultr, devcrate's Vultr Provider (a
// shell script next to this file), through the Provider contract (ADR 0001)
// against a fake Vultr API.
package vultr

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const pubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyExampleKeyExampleKeyExample devcrate"

const userData = "#!/bin/sh\n# devcrate user data\nset -eu\necho booted\n"

const apiKey = "VULTRTESTKEY"

type fixture struct {
	t    *testing.T
	api  *fakeVultr
	home string // HOME the Provider runs with
	path string // PATH the Provider runs with
	env  []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, api: newFakeVultr(t, apiKey), home: t.TempDir(), path: os.Getenv("PATH"),
		env: []string{"VULTR_API_KEY=" + apiKey}}
}

type result struct {
	Stdout, Stderr string
	Code           int
}

// run runs the Provider verb with a request for Environment env.
func (f *fixture) run(verb, env string, options map[string]any) result {
	f.t.Helper()
	if options == nil {
		options = map[string]any{}
	}
	req, _ := json.Marshal(map[string]any{
		"environment_id": env, "options": options, "ssh_public_key": pubKey, "user_data": userData,
	})
	cmd := exec.Command("./devcrate-provider-vultr", verb)
	cmd.Stdin = bytes.NewReader(req)
	cmd.Env = append([]string{"PATH=" + f.path, "HOME=" + f.home, "VULTR_API_URL=" + f.api.URL + "/v2", "DEVCRATE_VULTR_POLL=0"}, f.env...)
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

type upResult struct {
	State       string            `json:"state"`
	Host        string            `json:"host"`
	User        string            `json:"user"`
	Port        int               `json:"port"`
	SelfStopEnv map[string]string `json:"self_stop_env"`
}

var opts = map[string]any{"region": "ewr"}

func TestUpCreatesVolumeThenInstanceAndAttachesIt(t *testing.T) {
	f := newFixture(t)
	var res upResult
	f.ok("up", "api", opts, &res)

	v := f.api
	if len(v.blocks) != 1 || len(v.instances) != 1 {
		t.Fatalf("account has %d volumes and %d instances, want 1 and 1", len(v.blocks), len(v.instances))
	}
	b, i := v.blocks[0], v.instances[0]
	if b.Label != "devcrate-api" || b.Region != "ewr" || b.BlockType != "high_perf" || b.SizeGB != 50 {
		t.Errorf("volume = %+v, want devcrate-api, high_perf, 50 GB, in ewr", b)
	}
	// The newest x64 Ubuntu LTS, since there is no image option.
	if i.Label != "devcrate-api" || !slices.Equal(i.Tags, []string{"devcrate:api"}) || i.Region != "ewr" || i.Plan != "vc2-2c-4gb" || i.OSID != 2284 || i.SnapshotID != "" {
		t.Errorf("instance = %+v, want devcrate-api tagged devcrate:api, vc2-2c-4gb in ewr from os_id 2284", i)
	}
	if b.AttachedTo != i.ID {
		t.Errorf("volume attached to %q, want the instance %s", b.AttachedTo, i.ID)
	}

	// devcrate's public key is registered once and given to the instance.
	if len(v.sshKeys) != 1 || !strings.HasPrefix(pubKey, v.sshKeys[0].SSHKey) || !slices.Equal(i.SSHKeys, []string{v.sshKeys[0].ID}) {
		t.Errorf("ssh keys = %+v, instance's = %v", v.sshKeys, i.SSHKeys)
	}

	want := upResult{State: "running", Host: i.MainIP, User: "root", Port: 22,
		SelfStopEnv: map[string]string{"VULTR_API_KEY": v.users[0].keys[0].APIKey}}
	if mustJSON(res) != mustJSON(want) || i.MainIP == "0.0.0.0" {
		t.Errorf("up = %s, want %s", mustJSON(res), mustJSON(want))
	}
}

// The instance's user data mounts the volume at /var/lib/devcrate with an
// fstab entry (the Agent re-mounts with `mount /var/lib/devcrate` after a
// failed Self-stop), then runs devcrate's user data unchanged.
func TestUserDataMountsTheVolumeThenRunsDevcratesUserData(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	ud := f.api.userData["devcrate-api"]
	mount, devcrate, ok := strings.Cut(ud, userData)
	if !ok || strings.TrimSpace(devcrate) != "" {
		t.Fatalf("user data does not end with devcrate's:\n%s", ud)
	}
	for _, want := range []string{
		"dev=/dev/vdb",
		`mkfs.ext4 -q -L devcrate "$dev"`,
		`echo "LABEL=devcrate /var/lib/devcrate ext4 defaults,nofail,discard,noatime 0 2" >> /etc/fstab`,
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
	v := f.api
	if len(v.blocks) != 1 || len(v.instances) != 1 || len(v.sshKeys) != 1 {
		t.Errorf("after two ups: %d volumes, %d instances, %d ssh keys; want 1 each", len(v.blocks), len(v.instances), len(v.sshKeys))
	}
	if n := len(v.callsWith("/attach")); n != 1 {
		t.Errorf("%d attach calls, want 1", n)
	}
	// Every up mints a new Self-stop key; the rest stays the same.
	firstKey, secondKey := first.SelfStopEnv["VULTR_API_KEY"], second.SelfStopEnv["VULTR_API_KEY"]
	if firstKey == "" || firstKey == secondKey {
		t.Errorf("Self-stop keys %q then %q, want two different keys", firstKey, secondKey)
	}
	first.SelfStopEnv, second.SelfStopEnv = nil, nil
	if mustJSON(first) != mustJSON(second) {
		t.Errorf("second up = %s, want %s", mustJSON(second), mustJSON(first))
	}
}

// selfStopKey checks the IAM up leaves for env: a service user and a policy
// named devcrate-<env>, the policy allowing only the delete of the instance
// it returns, the user allowlisted to that instance's IP alone, and key as
// the user's only key.
func (f *fixture) selfStopKey(env, key string) *instance {
	f.t.Helper()
	v := f.api
	var u *user
	var p *policy
	for _, x := range v.users {
		if x.Name == "devcrate-"+env {
			u = x
		}
	}
	for _, x := range v.policies {
		if x.Name == "devcrate-"+env {
			p = x
		}
	}
	var i *instance
	for _, x := range v.instances {
		if x.Label == "devcrate-"+env {
			i = x
		}
	}
	if u == nil || p == nil || i == nil {
		f.t.Fatalf("user %+v, policy %+v, instance %+v; want all three named devcrate-%s", u, p, i, env)
	}
	if !u.ServiceUser || !u.APIEnabled || !slices.Equal(u.policies, []string{p.ID}) {
		f.t.Errorf("user = %+v, want a service user with API access and only the policy %s", u, p.ID)
	}
	if want := `{"Version":"2012-10-17","Statement":[{"Action":["compute.instance.Delete"],"Effect":"Allow","Resource":["instance:` + i.ID + `"]}]}`; mustJSON(p.Document) != want {
		f.t.Errorf("policy = %s, want %s", mustJSON(p.Document), want)
	}
	if want := []allowed{{i.MainIP, 32}}; !slices.Equal(u.allowlist, want) {
		f.t.Errorf("allowlist = %+v, want %+v", u.allowlist, want)
	}
	if len(u.keys) != 1 || u.keys[0].APIKey != key || u.keys[0].Expire {
		f.t.Errorf("user's keys = %+v, want only %q, never expiring", u.keys, key)
	}
	return i
}

func TestUpMintsASelfStopKeyScopedToTheInstance(t *testing.T) {
	f := newFixture(t)
	var res upResult
	f.ok("up", "api", opts, &res)
	if len(res.SelfStopEnv) != 1 {
		t.Fatalf("self_stop_env = %v, want VULTR_API_KEY alone", res.SelfStopEnv)
	}
	f.selfStopKey("api", res.SelfStopEnv["VULTR_API_KEY"])
}

// A resume repoints the same user and policy at the new instance, mints a
// new key and revokes the old one.
func TestUpAfterStopRotatesTheSelfStopKey(t *testing.T) {
	f := newFixture(t)
	var first, second upResult
	f.ok("up", "api", opts, &first)
	old := f.selfStopKey("api", first.SelfStopEnv["VULTR_API_KEY"])
	userID, policyID := f.api.users[0].ID, f.api.policies[1].ID

	f.ok("stop", "api", opts, nil)
	f.ok("up", "api", opts, &second)
	i := f.selfStopKey("api", second.SelfStopEnv["VULTR_API_KEY"])
	if i.ID == old.ID || i.MainIP == old.MainIP {
		t.Fatalf("resumed on %+v, want an instance other than %+v", i, old)
	}
	if len(f.api.users) != 1 || f.api.users[0].ID != userID || len(f.api.policies) != 2 || f.api.policies[1].ID != policyID {
		t.Errorf("users %+v, policies %+v; want the same user and policy", f.api.users, f.api.policies)
	}
}

func TestStopKeepsTheVolumeAndUpReattachesIt(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	vol := f.api.blocks[0].ID

	f.ok("stop", "api", opts, nil)
	f.ok("stop", "api", opts, nil) // idempotent
	if v := f.api; len(v.instances) != 0 || len(v.blocks) != 1 {
		t.Fatalf("after stop: %d instances, %d volumes; want 0 and 1", len(v.instances), len(v.blocks))
	}
	var st upResult
	f.ok("status", "api", opts, &st)
	if st.State != "stopped" {
		t.Errorf("status after stop = %+v, want stopped", st)
	}

	// Straight away, while the volume is still detaching from the old
	// instance.
	var res upResult
	f.ok("up", "api", opts, &res)
	v := f.api
	if len(v.blocks) != 1 || len(v.instances) != 1 || v.blocks[0].ID != vol || v.blocks[0].AttachedTo != v.instances[0].ID {
		t.Errorf("after resume: volumes %+v, instances %+v; want the old volume %s on a new instance", v.blocks, v.instances, vol)
	}
	if res.State != "running" {
		t.Errorf("up after stop = %+v", res)
	}
	if n := len(v.callsWith("/attach")); n < 3 {
		t.Errorf("%d attach calls, want the first and at least one retry after stop", n)
	}
}

func TestDeleteLeavesNoInstanceOrVolume(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.ok("up", "web", opts, nil)

	f.ok("delete", "api", opts, nil)
	f.ok("delete", "api", opts, nil) // idempotent
	v := f.api
	if len(v.instances) != 1 || v.instances[0].Label != "devcrate-web" {
		t.Errorf("instances = %+v, want only devcrate-web", v.instances)
	}
	if len(v.blocks) != 1 || v.blocks[0].Label != "devcrate-web" {
		t.Errorf("volumes = %+v, want only devcrate-web", v.blocks)
	}
	if len(v.users) != 1 || v.users[0].Name != "devcrate-web" {
		t.Errorf("users = %+v, want only devcrate-web", v.users)
	}
	var custom []string
	for _, p := range v.policies {
		if !p.System {
			custom = append(custom, p.Name)
		}
	}
	if !slices.Equal(custom, []string{"devcrate-web"}) {
		t.Errorf("policies = %v, want only devcrate-web (and the system ones)", custom)
	}
	var st upResult
	f.ok("status", "api", opts, &st)
	if st.State != "not_found" {
		t.Errorf("status after delete = %+v, want not_found", st)
	}
}

func TestListReadsEveryEnvironment(t *testing.T) {
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

func TestMissingCurlOrJqIsAClearError(t *testing.T) {
	f := newFixture(t)
	for _, tool := range []string{"curl", "jq"} {
		other := map[string]string{"curl": "jq", "jq": "curl"}[tool]
		path, err := exec.LookPath(other)
		if err != nil {
			t.Skipf("%s is not installed here", other)
		}
		dir := t.TempDir() // the other tool alone
		if err := os.Symlink(path, filepath.Join(dir, other)); err != nil {
			t.Fatal(err)
		}
		f.path = pathWithout(t, "curl", "jq") + ":" + dir
		r := f.run("status", "api", opts)
		if want := "vultr: " + tool + " is not installed (or not on PATH)"; r.Code == 0 || !strings.Contains(r.Stderr, want) {
			t.Errorf("without %s: exit %d, stderr %q", tool, r.Code, r.Stderr)
		}
	}
	if len(f.api.calls) != 0 {
		t.Errorf("calls made without the tools: %v", f.api.calls)
	}
}

func TestMissingAPIKeyIsAClearError(t *testing.T) {
	f := newFixture(t)
	f.env = nil
	r := f.run("list", "", nil)
	if r.Code == 0 || !strings.Contains(r.Stderr, "vultr: no API key: set VULTR_API_KEY, or api-key in ~/.vultr-cli.yaml") {
		t.Errorf("exit %d, stderr %q", r.Code, r.Stderr)
	}
	if len(f.api.calls) != 0 {
		t.Errorf("calls made without a key: %v", f.api.calls)
	}
}

// Without VULTR_API_KEY, the key vultr-cli is configured with is used.
func TestAPIKeyFallsBackToVultrCLIConfig(t *testing.T) {
	f := newFixture(t)
	f.env = nil
	cfg := "# vultr-cli\napi-key: " + apiKey + "\noutput: text\n"
	if err := os.WriteFile(filepath.Join(f.home, ".vultr-cli.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var items []map[string]string
	f.ok("list", "", nil, &items)

	// VULTR_API_KEY wins over the file.
	f.env = []string{"VULTR_API_KEY=wrong"}
	if r := f.run("list", "", nil); r.Code == 0 || !strings.Contains(r.Stderr, "Invalid API token.") {
		t.Errorf("with a wrong VULTR_API_KEY: exit %d, stderr %q", r.Code, r.Stderr)
	}
}

func TestUpRegionErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options map[string]any
		want    string
	}{
		{"missing", map[string]any{}, "vultr: region is required: set region under [providers.vultr]"},
		{"unknown", map[string]any{"region": "xyz"}, `vultr: unknown region "xyz"`},
		{"without NVMe", map[string]any{"region": "sao"}, `vultr: region "sao" has no NVMe block storage (high_perf)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := f.run("up", "api", tc.options)
			if r.Code == 0 || !strings.Contains(r.Stderr, tc.want) {
				t.Errorf("exit %d, stderr %q, want %q", r.Code, r.Stderr, tc.want)
			}
			if len(f.api.blocks) != 0 || len(f.api.instances) != 0 {
				t.Errorf("created %+v, %+v; want nothing", f.api.blocks, f.api.instances)
			}
		})
	}
}

// A volume stays in the region it was created in.
func TestUpInAnotherRegionThanTheVolumeFails(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", opts, nil)
	f.ok("stop", "api", opts, nil)
	r := f.run("up", "api", map[string]any{"region": "ams"})
	if r.Code == 0 || !strings.Contains(r.Stderr, `vultr: the volume devcrate-api is in ewr, not ams: set region = "ewr"`) {
		t.Errorf("exit %d, stderr %q", r.Code, r.Stderr)
	}
}

// The API's own error reaches the user as is, such as the one an account
// flagged for inactivity gets on every create.
func TestUpSurfacesTheAPIsError(t *testing.T) {
	f := newFixture(t)
	f.api.inactive = true
	r := f.run("up", "api", opts)
	if r.Code == 0 || !strings.Contains(r.Stderr, "vultr: POST /blocks: Due to account inactivity, please verify your payment details") {
		t.Errorf("exit %d, stderr %q", r.Code, r.Stderr)
	}
}

func TestOptionsReachTheAPI(t *testing.T) {
	f := newFixture(t)
	f.ok("up", "api", map[string]any{"region": "ams", "plan": "vc2-4c-8gb", "image": "snap-123", "volume_size": 100}, nil)
	b, i := f.api.blocks[0], f.api.instances[0]
	if b.Region != "ams" || b.SizeGB != 100 || i.Region != "ams" || i.Plan != "vc2-4c-8gb" || i.SnapshotID != "snap-123" || i.OSID != 0 {
		t.Errorf("volume %+v, instance %+v", b, i)
	}
}

func TestUpWithoutAnUbuntuLTSImageIsAClearError(t *testing.T) {
	f := newFixture(t)
	f.api.images = []osImage{{2465, "Ubuntu 24.10 x64"}}
	r := f.run("up", "api", opts)
	if r.Code == 0 || !strings.Contains(r.Stderr, "vultr: no Ubuntu LTS x64 image in GET /v2/os: set image to a snapshot ID") {
		t.Errorf("exit %d, stderr %q", r.Code, r.Stderr)
	}
	if len(f.api.instances) != 0 {
		t.Errorf("instances = %+v, want none", f.api.instances)
	}
}

// The self-stop script, run on the instance with self_stop_env, destroys the
// instance it runs on (found through the metadata service) and leaves the
// volume.
func TestSelfStopDeletesOnlyItsOwnInstance(t *testing.T) {
	f := newFixture(t)
	var api, web upResult
	f.ok("up", "api", opts, &api)
	f.ok("up", "web", opts, &web)
	var out struct{ Script string }
	f.ok("self-stop", "api", opts, &out)
	if !strings.Contains(out.Script, "curl -4") {
		t.Errorf("the script does not force IPv4, which the allowlist needs:\n%s", out.Script)
	}
	script := filepath.Join(t.TempDir(), "self-stop")
	if err := os.WriteFile(script, []byte(out.Script), 0o755); err != nil {
		t.Fatal(err)
	}
	// As the Agent runs it on the instance: only self_stop_env, plus PATH.
	selfStop := func(from, key string) error {
		f.api.from = from
		defer func() { f.api.from = laptopIP }()
		cmd := exec.Command(script)
		cmd.Env = []string{"PATH=" + f.path, "VULTR_API_KEY=" + key,
			"VULTR_API_URL=" + f.api.URL + "/v2", "VULTR_METADATA_URL=" + f.api.URL}
		b, err := cmd.CombinedOutput()
		t.Logf("self-stop from %s: %v: %s", from, err, b)
		return err
	}
	apiKey, webKey := api.SelfStopEnv["VULTR_API_KEY"], web.SelfStopEnv["VULTR_API_KEY"]

	// The key works only from its own instance's IP, and only on it.
	if selfStop(web.Host, apiKey) == nil || selfStop(api.Host, webKey) == nil {
		t.Error("self-stop succeeded with a key from another instance's IP")
	}
	// Even allowlisted on another instance, the policy refuses its delete.
	for _, u := range f.api.users {
		if u.Name == "devcrate-api" {
			u.allowlist = append(u.allowlist, allowed{web.Host, 32})
		}
	}
	if selfStop(web.Host, apiKey) == nil {
		t.Error("self-stop on another instance succeeded")
	}
	if len(f.api.instances) != 2 {
		t.Fatalf("instances = %+v, want both", f.api.instances)
	}

	before := len(f.api.calls)
	if err := selfStop(api.Host, apiKey); err != nil {
		t.Fatalf("self-stop: %v", err)
	}
	v := f.api
	if len(v.instances) != 1 || v.instances[0].Label != "devcrate-web" {
		t.Errorf("instances after self-stop = %+v, want only devcrate-web", v.instances)
	}
	if len(v.blocks) != 2 {
		t.Errorf("volumes after self-stop = %+v, want both", v.blocks)
	}
	if calls := v.calls[before:]; len(calls) != 2 || !strings.HasPrefix(calls[1], "DELETE /v2/instances/") {
		t.Errorf("self-stop calls = %v, want a metadata read and one DELETE", calls)
	}
}

// A key a later up revoked no longer works.
func TestSelfStopWithARevokedKeyFails(t *testing.T) {
	f := newFixture(t)
	var first, second upResult
	f.ok("up", "api", opts, &first)
	f.ok("up", "api", opts, &second)
	f.api.from = second.Host
	req, _ := http.NewRequest("DELETE", f.api.URL+"/v2/instances/"+f.api.instances[0].ID, nil)
	req.Header.Set("Authorization", "Bearer "+first.SelfStopEnv["VULTR_API_KEY"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || len(f.api.instances) != 1 {
		t.Errorf("DELETE with the old key: %s, %d instances; want 401 and the instance kept", resp.Status, len(f.api.instances))
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
