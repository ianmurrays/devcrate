package provider

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeProvider(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, "devcrate-provider-"+name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunSendsRequestOnStdinAndParsesStdout(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "stdin.json")
	writeProvider(t, dir, "test", `echo "$1" > `+got+`.verb
cat > `+got+`
echo '{"state":"running","host":"10.0.0.1","user":"root","port":2222,"self_stop_env":{"TOKEN":"t0k"}}'
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p, err := Find("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Up(Request{
		EnvironmentID: "api",
		Options:       map[string]any{"region": "ams3"},
		SSHPublicKey:  "ssh-ed25519 AAAA",
		UserData:      "#!/bin/sh\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Result{State: "running", Host: "10.0.0.1", User: "root", Port: 2222, SelfStopEnv: map[string]string{"TOKEN": "t0k"}}); !reflect.DeepEqual(res, want) {
		t.Errorf("result = %+v", res)
	}

	verb, _ := os.ReadFile(got + ".verb")
	if strings.TrimSpace(string(verb)) != "up" {
		t.Errorf("verb = %q, want up", verb)
	}
	raw, _ := os.ReadFile(got)
	var sent map[string]any
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("stdin is not one JSON object: %q", raw)
	}
	want := map[string]any{
		"environment_id": "api",
		"options":        map[string]any{"region": "ams3"},
		"ssh_public_key": "ssh-ed25519 AAAA",
		"user_data":      "#!/bin/sh\n",
	}
	if a, b := mustJSON(sent), mustJSON(want); a != b {
		t.Errorf("stdin = %s, want %s", a, b)
	}
}

func TestRunPassesStderrAndFailsOnNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	writeProvider(t, dir, "broken", `echo "quota exceeded" >&2; exit 3`)

	p, err := Find("broken", dir)
	if err == nil {
		t.Fatalf("found provider %v without it being on PATH or in the providers dir", p)
	}

	cfg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfg, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProvider(t, filepath.Join(cfg, "providers"), "broken", `echo "quota exceeded" >&2; exit 3`)
	p, err = Find("broken", cfg)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	p.Stderr = &stderr
	if _, err := p.Status(Request{EnvironmentID: "api"}); err == nil {
		t.Fatal("want error on non-zero exit")
	}
	if !strings.Contains(stderr.String(), "quota exceeded") {
		t.Errorf("stderr = %q, want provider stderr passed through", stderr.String())
	}
}

func TestStopDeleteAndList(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "verbs")
	writeProvider(t, dir, "test", `echo "$1" >> `+log+`
cat > /dev/null
case $1 in
list) echo '[{"environment_id":"api","state":"running"},{"environment_id":"web","state":"stopped"}]' ;;
esac
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	p, err := Find("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// stop and delete print nothing on stdout; that is not an error.
	if err := p.Stop(Request{EnvironmentID: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(Request{EnvironmentID: "api"}); err != nil {
		t.Fatal(err)
	}
	items, err := p.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{{EnvironmentID: "api", State: "running"}, {EnvironmentID: "web", State: "stopped"}}
	if mustJSON(items) != mustJSON(want) {
		t.Errorf("list = %+v, want %+v", items, want)
	}
	verbs, _ := os.ReadFile(log)
	if string(verbs) != "stop\ndelete\nlist\n" {
		t.Errorf("verbs run = %q", verbs)
	}
}

// Providers that ship with devcrate (DigitalOcean) sit next to its executable.
func TestFindLooksNextToDevcrateLast(t *testing.T) {
	shipped := t.TempDir()
	writeProvider(t, shipped, "shipped", "")
	old := executableDir
	executableDir = func() (string, error) { return shipped, nil }
	t.Cleanup(func() { executableDir = old })
	t.Setenv("PATH", t.TempDir())

	cfg := t.TempDir()
	p, err := Find("shipped", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != filepath.Join(shipped, "devcrate-provider-shipped") {
		t.Errorf("path = %s", p.Path)
	}

	// The user's own copy in the providers dir wins over the shipped one.
	if err := os.MkdirAll(filepath.Join(cfg, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProvider(t, filepath.Join(cfg, "providers"), "shipped", "")
	if p, err = Find("shipped", cfg); err != nil || p.Path != filepath.Join(cfg, "providers", "devcrate-provider-shipped") {
		t.Errorf("Find = %v, %v; want the providers dir copy", p, err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
