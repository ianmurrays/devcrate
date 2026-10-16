//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const signingConfig = "git_signing_key = \"~/.ssh/devcrate_signing\"\n"

// signingScript commits and tags in a new repo, then verifies both against
// pub, printing "verified" on success. Unsigned, it prints "unsigned".
func signingScript(pub string) string {
	return `set -e
d=$(mktemp -d); cd "$d"; git init -q
echo "e2e@devcrate ` + pub + `" > "$d/allowed"
git -c user.name=e2e -c user.email=e2e@devcrate commit -q --allow-empty -m signed
git -c user.name=e2e -c user.email=e2e@devcrate tag -m signed v1
git cat-file commit HEAD | grep -q "^gpgsig " || { echo unsigned; exit; }
git -c gpg.ssh.allowedSignersFile="$d/allowed" verify-commit HEAD
git -c gpg.ssh.allowedSignersFile="$d/allowed" verify-tag v1
echo verified`
}

func TestCommitsAreSigned(t *testing.T) {
	for _, inContainer := range []bool{false, true} {
		name := "on the VM"
		if inContainer {
			name = "in the devcontainer"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			ssh := []string{"ssh", "--"}
			// inside runs a command where git runs, without an SSH connection.
			inside := e.VM
			if inContainer {
				must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
				e.WriteFile(".devcontainer/devcontainer.json", `{"build": {"dockerfile": "Dockerfile"}}`)
				// The image's own /etc/gitconfig settings must survive.
				e.WriteFile(".devcontainer/Dockerfile", "FROM alpine:3.20\nRUN apk add --no-cache git openssh-keygen && git config --system core.editor vi\n")
				e.Push()
				ssh = []string{"ssh", "--container", "--"}
				inside = func(script string) (string, int) {
					return e.VM("docker exec $(docker ps -q --filter label=devcontainer.local_folder=/var/lib/devcrate/workspace) sh -c " + shellQuote(script))
				}
			}
			run := func(script string) Result {
				t.Helper()
				return e.Devcrate(append(ssh, "sh -c "+shellQuote(script))...)
			}

			key := filepath.Join(e.Home, ".ssh", "devcrate_signing")
			must(os.MkdirAll(filepath.Dir(key), 0o700))
			if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", key).CombinedOutput(); err != nil {
				t.Fatalf("ssh-keygen: %v: %s", err, out)
			}
			pubBytes, err := os.ReadFile(key + ".pub")
			must(err)
			pub := strings.TrimSpace(string(pubBytes))
			privBytes, err := os.ReadFile(key)
			must(err)
			// A line of the private key's base64, to look for on the volume.
			secret := strings.Split(string(privBytes), "\n")[2]

			e.UserConfig(signingConfig)
			for i := 0; i < 2; i++ {
				if r := e.Devcrate("up"); r.Code != 0 {
					t.Fatalf("up %d exit %d", i+1, r.Code)
				}
			}
			if r := run(signingScript(pub)); r.Code != 0 || !strings.HasSuffix(r.Stdout, "verified\n") {
				t.Errorf("signed commit and tag = exit %d stdout %q stderr %q, want verified", r.Code, r.Stdout, r.Stderr)
			}
			// Two ups leave one include and the existing settings.
			if out, _ := inside("git config --system --get-all include.path"); out != "/run/devcrate/creds/git/config\n" {
				t.Errorf("system include.path = %q, want exactly one include of the fragment", out)
			}
			// Another user can't read the key, but its git still runs.
			if out, code := inside("su -s /bin/sh nobody -c 'git config -l'"); code != 0 {
				t.Errorf("git as nobody = exit %d %q, want it to ignore the unreadable fragment", code, out)
			}
			existing := "git config --system url.git@github.com:.insteadOf"
			if inContainer {
				existing = "git config --system core.editor"
			}
			if out, code := inside(existing); code != 0 {
				t.Errorf("%s = exit %d %q, want the existing setting kept", existing, code, out)
			}

			// Signed with nobody connected: the forwarded agent refuses by then.
			if r := run("nohup sh -c " + shellQuote("sleep 5; "+signingScript(pub)) + " > /tmp/bg 2>&1 < /dev/null &"); r.Code != 0 {
				t.Fatalf("starting the background commit exit %d stderr %q", r.Code, r.Stderr)
			}
			var bg string
			for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
				if bg, _ = inside("cat /tmp/bg"); strings.HasSuffix(bg, "verified\n") || strings.HasSuffix(bg, "unsigned\n") {
					break
				}
			}
			if !strings.HasSuffix(bg, "verified\n") {
				t.Errorf("commit with nobody connected printed %q, want verified", bg)
			}

			// After a Stop and a start, the key is on tmpfs only.
			if r := e.Devcrate("stop"); r.Code != 0 {
				t.Fatalf("stop exit %d", r.Code)
			}
			if r := e.Devcrate("up"); r.Code != 0 {
				t.Fatalf("up after stop exit %d", r.Code)
			}
			if out, code := e.VM("grep -rlF -- " + shellQuote(secret) + " /var/lib/devcrate"); code != 1 {
				t.Errorf("signing key found on the volume (grep exit %d): %s", code, out)
			}

			// A missing key warns, and commits are unsigned, not failing.
			must(os.Rename(key, key+".away"))
			r := run(signingScript(pub))
			if !strings.Contains(r.Stderr, "git_signing_key: ~/.ssh/devcrate_signing does not exist here") {
				t.Errorf("ssh with a missing key stderr %q, want a warning", r.Stderr)
			}
			if r.Code != 0 || !strings.HasSuffix(r.Stdout, "unsigned\n") {
				t.Errorf("commit with a missing key = exit %d stdout %q stderr %q, want unsigned", r.Code, r.Stdout, r.Stderr)
			}

			// Without git_signing_key, commits are unsigned too.
			must(os.Rename(key+".away", key))
			e.UserConfig("")
			if r := run(signingScript(pub)); r.Code != 0 || !strings.HasSuffix(r.Stdout, "unsigned\n") {
				t.Errorf("commit without git_signing_key = exit %d stdout %q stderr %q, want unsigned", r.Code, r.Stdout, r.Stderr)
			}
		})
	}
}
