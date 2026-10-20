//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// credentialFill asks git for its GitHub HTTPS credentials, never prompting.
const credentialFill = `printf 'protocol=https\nhost=github.com\n\n' | GIT_TERMINAL_PROMPT=0 git credential fill`

func TestGitHubGoesOverHTTPSWithTheForwardedToken(t *testing.T) {
	for _, inContainer := range []bool{false, true} {
		name := "on the VM"
		if inContainer {
			name = "in the devcontainer"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			inside := e.VM
			if inContainer {
				must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
				e.WriteFile(".devcontainer/devcontainer.json", `{"build": {"dockerfile": "Dockerfile"}}`)
				e.WriteFile(".devcontainer/Dockerfile", "FROM alpine:3.20\nRUN apk add --no-cache git\n")
				e.Push()
				inside = func(script string) (string, int) {
					return e.VM("docker exec $(docker ps -q --filter label=devcontainer.local_folder=/var/lib/devcrate/workspace) sh -c " + shellQuote(script))
				}
			}
			getURL := func(url string) string {
				t.Helper()
				out, _ := inside("git ls-remote --get-url " + url)
				return strings.TrimSpace(out)
			}
			token := func() string {
				b := make([]byte, 8)
				rand.Read(b)
				return "ghp_" + hex.EncodeToString(b)
			}

			// Token mode: no agent, GITHUB_TOKEN forwarded.
			first := token()
			e.LaptopEnv = []string{"GITHUB_TOKEN=" + first}
			e.UserConfig("forward_agent = false\nforward_env = [\"GITHUB_TOKEN\"]\n")
			for i := 0; i < 2; i++ { // the second up meets the first one's git config
				if r := e.Devcrate("up"); r.Code != 0 {
					t.Fatalf("up %d exit %d", i+1, r.Code)
				}
			}
			for _, url := range []string{"https://github.com/a/b", "git@github.com:a/b", "ssh://git@github.com/a/b"} {
				if got := getURL(url); got != "https://github.com/a/b" {
					t.Errorf("token mode: %s goes to %q, want https://github.com/a/b", url, got)
				}
			}
			if out, code := inside(credentialFill); code != 0 || !strings.Contains(out, "\npassword="+first+"\n") {
				t.Errorf("token mode: credential fill = exit %d %q, want the token", code, out)
			}
			if out, _ := inside("git config -l"); strings.Contains(out, first) {
				t.Errorf("token in git config: %q", out)
			}
			if out, code := e.VM("grep -rlF -- " + shellQuote(first) + " /var/lib/devcrate /etc/gitconfig"); code != 1 {
				t.Errorf("token found on the volume or in /etc/gitconfig (grep exit %d): %s", code, out)
			}

			// A refreshed token takes effect on the next push, with no setup.
			second := token()
			e.LaptopEnv = []string{"GITHUB_TOKEN=" + second}
			if r := e.Devcrate("ssh", "--", "true"); r.Code != 0 {
				t.Fatalf("ssh exit %d", r.Code)
			}
			if out, _ := inside(credentialFill); !strings.Contains(out, "\npassword="+second+"\n") {
				t.Errorf("after a refresh, credential fill = %q, want the new token", out)
			}

			// SSH agent mode, as before: the token is for gh only, and the VM
			// (not the devcontainer) sends GitHub HTTPS URLs over SSH.
			e.UserConfig("forward_env = [\"GITHUB_TOKEN\"]\n")
			if r := e.Devcrate("up"); r.Code != 0 {
				t.Fatalf("up in agent mode exit %d", r.Code)
			}
			want := map[string]string{"https://github.com/a/b": "git@github.com:a/b", "git@github.com:a/b": "git@github.com:a/b"}
			if inContainer {
				want["https://github.com/a/b"] = "https://github.com/a/b"
			}
			for url, w := range want {
				if got := getURL(url); got != w {
					t.Errorf("agent mode: %s goes to %q, want %q", url, got, w)
				}
			}
			if out, code := inside(credentialFill); code == 0 {
				t.Errorf("agent mode: credential fill = %q, want no GitHub credentials", out)
			}
		})
	}
}
