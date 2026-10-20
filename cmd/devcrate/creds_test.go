package main

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveFiles returns the entries of a credsArchive by name.
func archiveFiles(t *testing.T, c *config) map[string]string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	buf, err := c.credsArchive("", "", "")
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
	if _, err := c.credsArchive("", "", ""); err == nil {
		t.Error("A-B was accepted, want an error")
	}
}

func TestGitSigningKeyForwardsKeyAndConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "signing"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".config", "devcrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`git_signing_key = "~/.ssh/signing"`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	buf, err := c.credsArchive("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(buf)
	for h, err := tr.Next(); err != io.EOF; h, err = tr.Next() {
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[h.Name] = string(data)
	}
	if got := files["git/signing_key"]; got != "PRIVATE" {
		t.Errorf("git/signing_key = %q, want the key", got)
	}
	if got := files["git/config"]; got != signingGitConfig {
		t.Errorf("git/config = %q, want %q", got, signingGitConfig)
	}
}

func TestMissingGitSigningKeyForwardsNoConfig(t *testing.T) {
	files := archiveFiles(t, &config{GitSigningKey: "~/.ssh/missing"})
	if _, ok := files["git/config"]; ok {
		t.Error("git/config written for a missing key, want unsigned commits")
	}
	if _, ok := files["git/"]; !ok {
		t.Error("no git/ directory, want an empty one so creds.sh replaces the old one")
	}
}

func TestGitSigningKeyOutsideHomeIsRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := (&config{GitSigningKey: "/etc/ssh/key"}).credsArchive("", "", ""); err == nil || !strings.Contains(err.Error(), "git_signing_key") {
		t.Errorf("err = %v, want a git_signing_key error", err)
	}
}

func TestGitHubTokenModeNeedsNoAgentAndAForwardedToken(t *testing.T) {
	off, on := false, true
	t.Setenv("GITHUB_TOKEN", "secret")
	for _, tc := range []struct {
		name  string
		c     *config
		token bool
	}{
		{"agent off, token forwarded", &config{ForwardAgent: &off, ForwardEnv: []string{"GITHUB_TOKEN"}}, true},
		{"agent off, token from a command", &config{ForwardAgent: &off, ForwardEnvCommands: map[string]string{"GITHUB_TOKEN": "echo secret"}}, true},
		{"agent default", &config{ForwardEnv: []string{"GITHUB_TOKEN"}}, false},
		{"agent on", &config{ForwardAgent: &on, ForwardEnv: []string{"GITHUB_TOKEN"}}, false},
		{"agent off, no token", &config{ForwardAgent: &off}, false},
		{"agent off, empty token", &config{ForwardAgent: &off, ForwardEnvCommands: map[string]string{"GITHUB_TOKEN": "true"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := archiveFiles(t, tc.c)["git/config"]
			if tc.token != (got == githubTokenGitConfig) {
				t.Errorf("git/config = %q, want token mode %t", got, tc.token)
			}
			if strings.Contains(got, "secret") {
				t.Errorf("git/config = %q, want no token in it", got)
			}
		})
	}
}

func TestGitHubTokenModeKeepsSigning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GITHUB_TOKEN", "secret")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "signing"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	off := false
	c := &config{ForwardAgent: &off, ForwardEnv: []string{"GITHUB_TOKEN"}, GitSigningKey: "~/.ssh/signing"}
	buf, err := c.credsArchive("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(buf)
	for h, err := tr.Next(); err != io.EOF; h, err = tr.Next() {
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := io.ReadAll(tr); h.Name == "git/config" {
			if want := signingGitConfig + githubTokenGitConfig; string(data) != want {
				t.Errorf("git/config = %q, want %q", data, want)
			}
			return
		}
	}
	t.Error("no git/config")
}
