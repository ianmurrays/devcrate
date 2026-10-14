package provider

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
echo '{"state":"running","host":"10.0.0.1","user":"root","port":2222}'
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
	if res != (Result{State: "running", Host: "10.0.0.1", User: "root", Port: 2222}) {
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

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
