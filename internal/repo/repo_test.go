package repo

import "testing"

func TestParseRejectsUserOnlyKeys(t *testing.T) {
	for _, toml := range []string{
		`forward_env = ["X"]`,
		"[forward_env_commands]\nX = \"echo x\"",
		`forward_files = ["~/.npmrc"]`,
		`forward_agent = true`,
		`ssh_agent = "/tmp/agent.sock"`,
	} {
		if _, err := parse("https://github.com/a/b.git", toml); err == nil {
			t.Errorf("%q was accepted, want an error", toml)
		}
	}
}
