package main

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveFiles returns the regular files of a credsArchive by name.
func archiveFiles(t *testing.T, c *config) map[string]string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	buf, err := c.credsArchive()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		} else if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[h.Name] = string(data)
	}
}

func TestForwardEnvCommandsForwardStdout(t *testing.T) {
	c := &config{ForwardEnvCommands: map[string]string{
		"TOKEN":  "printf 'it'\"'\"'s\\n'",
		"BROKEN": "echo nope; exit 3", // skipped, like an unset forward_env var
	}}
	files := archiveFiles(t, c)
	if got, want := files["env"], "export TOKEN='it'\\''s'\n"; got != want {
		t.Errorf("env = %q, want %q", got, want)
	}
	if got := files["names"]; got != "TOKEN\n" {
		t.Errorf("names = %q, want TOKEN only", got)
	}
}

func TestForwardEnvCommandsConflictWithForwardEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "devcrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := "forward_env = [\"TOKEN\"]\n[forward_env_commands]\nTOKEN = \"echo x\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "TOKEN") {
		t.Errorf("err = %v, want a conflict naming TOKEN", err)
	}
}

func TestForwardEnvCommandsRejectBadNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &config{ForwardEnvCommands: map[string]string{"A-B": "echo x"}}
	if _, err := c.credsArchive(); err == nil {
		t.Error("A-B was accepted, want an error")
	}
}
