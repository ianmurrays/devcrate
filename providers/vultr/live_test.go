package vultr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveSmoke runs devcrate against a real Vultr account. It creates an
// instance and a volume (billed), and the Self-stop service user, policy and
// key, and deletes them all at the end. It is skipped
// unless all of these are set:
//
//	VULTR_API_KEY                  the account's API key
//	DEVCRATE_VULTR_SMOKE_REGION    a region with NVMe block storage, such as ewr
//	DEVCRATE_VULTR_SMOKE_REPO      public https git URL of a repo with a devcontainer.json
//
// and optionally DEVCRATE_VULTR_SMOKE_IMAGE (a snapshot ID) and
// DEVCRATE_VULTR_SMOKE_PLAN. It stops once with devcrate stop, and once by
// leaving the Environment Idle until it Self-stops. Run with
//
//	go test -v -run TestLiveSmoke -timeout 60m ./providers/vultr/
func TestLiveSmoke(t *testing.T) {
	need := []string{"VULTR_API_KEY", "DEVCRATE_VULTR_SMOKE_REGION", "DEVCRATE_VULTR_SMOKE_REPO"}
	for _, v := range need {
		if os.Getenv(v) == "" {
			t.Skipf("live smoke test: set %s to run it", strings.Join(need, ", "))
		}
	}
	for _, tool := range []string{"curl", "jq", "git", "ssh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH", tool)
		}
	}

	// devcrate, the Agent (instances are amd64) and this Provider, side by side.
	bin := t.TempDir()
	build := func(goos, goarch, out, pkg string) {
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, out), pkg)
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, out)
		}
	}
	build("", "", "devcrate", "./cmd/devcrate")
	build("linux", "amd64", "devcrate-agent-linux-amd64", "./cmd/devcrate-agent")
	script, err := os.ReadFile("devcrate-provider-vultr")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "devcrate-provider-vultr"), script, 0o755); err != nil {
		t.Fatal(err)
	}

	suffix := make([]byte, 4)
	rand.Read(suffix)
	name := "smoke-" + hex.EncodeToString(suffix)
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), name)
	// The Agent checks for Idle every 10s instead of every minute.
	env := append(os.Environ(), "HOME="+home, "DEVCRATE_TEST_AGENT_POLL=10s")

	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		t.Logf("%s %s:\n%s", name, strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("%s %v: %v", name, args, err)
		}
		return string(out)
	}
	devcrate := func(args ...string) string { return run(filepath.Join(bin, "devcrate"), args...) }
	get := func(path string, out any) {
		t.Helper()
		req, _ := http.NewRequest("GET", "https://api.vultr.com/v2"+path, nil)
		req.Header.Set("Authorization", "Bearer "+os.Getenv("VULTR_API_KEY"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %s", path, resp.Status)
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	instances := func() int {
		var out struct{ Instances []struct{ Tags []string } }
		get("/instances?per_page=500", &out)
		n := 0
		for _, i := range out.Instances {
			if slices.Contains(i.Tags, "devcrate:"+name) {
				n++
			}
		}
		return n
	}
	// iam counts the service users and policies named devcrate-<name>, and
	// the user's keys.
	iam := func() (users, policies, keys int) {
		var u struct {
			Users []struct{ ID, Name string }
		}
		get("/users?per_page=500", &u)
		for _, x := range u.Users {
			if x.Name == "devcrate-"+name {
				users++
				var k struct {
					APIKeys []any `json:"api_keys"`
				}
				get("/users/"+x.ID+"/apikeys", &k)
				keys += len(k.APIKeys)
			}
		}
		var p struct{ Policies []struct{ Name string } }
		get("/policies?per_page=500", &p)
		for _, x := range p.Policies {
			if x.Name == "devcrate-"+name {
				policies++
			}
		}
		return
	}
	volumes := func() int {
		var out struct{ Blocks []struct{ Label string } }
		get("/blocks?per_page=500", &out)
		n := 0
		for _, b := range out.Blocks {
			if b.Label == "devcrate-"+name {
				n++
			}
		}
		return n
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config/devcrate"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[providers.vultr]\nregion = " + quote(os.Getenv("DEVCRATE_VULTR_SMOKE_REGION")) + "\n"
	for opt, v := range map[string]string{"image": "DEVCRATE_VULTR_SMOKE_IMAGE", "plan": "DEVCRATE_VULTR_SMOKE_PLAN"} {
		if os.Getenv(v) != "" {
			cfg += opt + " = " + quote(os.Getenv(v)) + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".config/devcrate/config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	toml := "provider = \"vultr\"\nname = " + quote(name) + "\nidle_timeout = \"3m\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".devcrate.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "init", "-q")
	run("git", "remote", "add", "origin", os.Getenv("DEVCRATE_VULTR_SMOKE_REPO"))
	t.Cleanup(func() {
		cmd := exec.Command(filepath.Join(bin, "devcrate"), "delete", "--yes")
		cmd.Dir, cmd.Env = dir, env
		out, _ := cmd.CombinedOutput()
		t.Logf("cleanup: devcrate delete --yes:\n%s", out)
	})

	// 1. up to Ready.
	start := time.Now()
	devcrate("up")
	t.Logf("FIRST UP TOOK %s", time.Since(start).Round(time.Second))
	devcrate("ssh", "--", "touch /var/lib/devcrate/workspace/.devcrate-smoke")
	container := devcrate("ssh", "--container", "--", "hostname")
	if out := devcrate("list"); !strings.Contains(out, name) {
		t.Errorf("devcrate list does not show %s", name)
	}

	// 2. stop keeps only the volume.
	devcrate("stop")
	if instances() != 0 || volumes() != 1 {
		t.Fatalf("after stop: %d instances, %d volumes; want 0 and 1", instances(), volumes())
	}
	if out := devcrate("status"); !strings.Contains(out, "stopped") {
		t.Errorf("devcrate status after stop does not say stopped")
	}

	// 3. up again restores the workspace and container.
	start = time.Now()
	devcrate("up")
	t.Logf("RESUME TOOK %s", time.Since(start).Round(time.Second))
	devcrate("ssh", "--", "test -f /var/lib/devcrate/workspace/.devcrate-smoke")
	if again := devcrate("ssh", "--container", "--", "hostname"); again != container {
		t.Errorf("devcontainer after resume = %q, want the same container %q", again, container)
	}

	// The resume rotated the Self-stop key: one user, policy and key.
	if u, p, k := iam(); u != 1 || p != 1 || k != 1 {
		t.Errorf("after resume: %d users, %d policies, %d keys; want 1 each", u, p, k)
	}

	// 4. Left Idle, it Self-stops with that key and keeps the volume.
	start = time.Now()
	for instances() != 0 {
		if time.Since(start) > 15*time.Minute {
			t.Fatalf("still running 15 minutes after the last ssh, with idle_timeout = 3m")
		}
		time.Sleep(15 * time.Second)
	}
	t.Logf("SELF-STOPPED %s AFTER THE LAST SSH", time.Since(start).Round(time.Second))
	if volumes() != 1 {
		t.Fatalf("after Self-stop: %d volumes, want 1", volumes())
	}

	// 5. delete leaves no instance, volume, user, policy or key behind.
	devcrate("delete", "--yes")
	if instances() != 0 || volumes() != 0 {
		t.Errorf("after delete: %d instances, %d volumes of %s", instances(), volumes(), name)
	}
	if u, p, k := iam(); u != 0 || p != 0 || k != 0 {
		t.Errorf("after delete: %d users, %d policies, %d keys", u, p, k)
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
