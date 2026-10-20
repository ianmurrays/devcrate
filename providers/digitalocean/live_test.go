package digitalocean

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveSmoke runs devcrate against a real DigitalOcean account. It creates
// a droplet, a volume and a Base image (billed) and deletes them at the end. It is skipped
// unless all of these are set:
//
//	DIGITALOCEAN_ACCESS_TOKEN  full-access token doctl uses (HOME is isolated)
//	DEVCRATE_DO_SMOKE_IMAGE    snapshot ID of an image with Docker (README.md)
//	DEVCRATE_DO_SMOKE_TOKEN    the droplet:delete Self-stop token
//	DEVCRATE_DO_SMOKE_REPO     public https git URL of a repo with a devcontainer.json
//
// and optionally DEVCRATE_DO_SMOKE_REGION / DEVCRATE_DO_SMOKE_SIZE. Run with
//
//	go test -v -run TestLiveSmoke -timeout 60m ./providers/digitalocean/
func TestLiveSmoke(t *testing.T) {
	need := []string{"DIGITALOCEAN_ACCESS_TOKEN", "DEVCRATE_DO_SMOKE_IMAGE", "DEVCRATE_DO_SMOKE_TOKEN", "DEVCRATE_DO_SMOKE_REPO"}
	for _, v := range need {
		if os.Getenv(v) == "" {
			t.Skipf("live smoke test: set %s to run it", strings.Join(need, ", "))
		}
	}
	for _, tool := range []string{"doctl", "jq", "git", "ssh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH", tool)
		}
	}

	// devcrate, the Agent (droplets are amd64) and this Provider, side by side.
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
	script, err := os.ReadFile("devcrate-provider-digitalocean")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "devcrate-provider-digitalocean"), script, 0o755); err != nil {
		t.Fatal(err)
	}

	suffix := make([]byte, 4)
	rand.Read(suffix)
	name := "smoke-" + hex.EncodeToString(suffix)
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), name)
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
	doctlJSON := func(args ...string) []map[string]any {
		var items []map[string]any
		if err := json.Unmarshal([]byte(run("doctl", append(args, "-o", "json")...)), &items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	tag := "devcrate:" + name
	droplets := func() int { return len(doctlJSON("compute", "droplet", "list", "--tag-name", tag)) }
	volumes := func() int {
		n := 0
		for _, v := range doctlJSON("compute", "volume", "list") {
			tags, _ := v["tags"].([]any)
			if slices.Contains(tags, any(tag)) {
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
	cfg := "[providers.digitalocean]\nimage = " + quote(os.Getenv("DEVCRATE_DO_SMOKE_IMAGE")) + "\ntoken = " + quote(os.Getenv("DEVCRATE_DO_SMOKE_TOKEN")) + "\n"
	for opt, v := range map[string]string{"region": "DEVCRATE_DO_SMOKE_REGION", "size": "DEVCRATE_DO_SMOKE_SIZE"} {
		if os.Getenv(v) != "" {
			cfg += opt + " = " + quote(os.Getenv(v)) + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".config/devcrate/config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	toml := "provider = \"digitalocean\"\nname = " + quote(name) + "\nidle_timeout = \"1m\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".devcrate.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "init", "-q")
	run("git", "remote", "add", "origin", os.Getenv("DEVCRATE_DO_SMOKE_REPO"))
	t.Cleanup(func() {
		for _, args := range [][]string{{"delete", "--yes"}, {"save-image", "--delete"}} {
			cmd := exec.Command(filepath.Join(bin, "devcrate"), args...)
			cmd.Dir, cmd.Env = dir, env
			out, _ := cmd.CombinedOutput()
			t.Logf("cleanup: devcrate %s:\n%s", strings.Join(args, " "), out)
		}
	})

	// 1. up to Ready.
	devcrate("up")
	devcrate("ssh", "--", "touch /var/lib/devcrate/workspace/.devcrate-smoke")
	container := devcrate("ssh", "--container", "--", "hostname")

	// 2. Idle Self-stop with the configured token. (Not devcrate status: its
	// SSH probe would count as a connection.)
	deadline := time.Now().Add(15 * time.Minute)
	for droplets() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the droplet did not Self-stop within 15 minutes")
		}
		time.Sleep(15 * time.Second)
	}
	if volumes() != 1 {
		t.Fatal("the volume did not survive Self-stop")
	}

	// 3. up again restores the workspace and container.
	start := time.Now()
	devcrate("up")
	t.Logf("RESUME TOOK %s", time.Since(start).Round(time.Second))
	devcrate("ssh", "--", "test -f /var/lib/devcrate/workspace/.devcrate-smoke")
	if again := devcrate("ssh", "--container", "--", "hostname"); again != container {
		t.Errorf("devcontainer after resume = %q, want the same container %q", again, container)
	}

	// 4. save-image leaves the Environment Stopped with its volume, and up
	// boots from the Base image to Ready.
	devcrate("save-image")
	if droplets() != 0 || volumes() != 1 {
		t.Fatalf("after save-image: %d droplets, %d volumes; want 0 and 1", droplets(), volumes())
	}
	start = time.Now()
	devcrate("up")
	t.Logf("UP FROM THE BASE IMAGE TOOK %s", time.Since(start).Round(time.Second))
	if st := devcrate("status"); !strings.HasPrefix(st, "ready\nBase image: ") {
		t.Errorf("status after up from the Base image = %q, want ready with a Base image", st)
	}
	d := doctlJSON("compute", "droplet", "list", "--tag-name", tag)
	if img, _ := d[0]["image"].(map[string]any); img["name"] != "devcrate-base-"+name {
		t.Errorf("droplet booted from image %v, want devcrate-base-%s", img["name"], name)
	}
	devcrate("ssh", "--", "test -f /var/lib/devcrate/workspace/.devcrate-smoke")

	// 5. delete leaves no droplet, volume or tag behind; the Base image stays
	// until save-image --delete.
	devcrate("delete", "--yes")
	if droplets() != 0 || volumes() != 0 {
		t.Errorf("after delete: %d droplets, %d volumes tagged %s", droplets(), volumes(), tag)
	}
	if err := exec.Command("doctl", "compute", "tag", "get", tag).Run(); err == nil {
		t.Errorf("tag %s still exists after delete", tag)
	}
	devcrate("save-image", "--delete")
	for _, s := range doctlJSON("compute", "snapshot", "list", "--resource", "droplet") {
		if s["name"] == "devcrate-base-"+name {
			t.Errorf("Base image %v left after save-image --delete", s["id"])
		}
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
