//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoCannotAskForCredentials(t *testing.T) {
	for _, key := range []string{"forward_env", "forward_files", "forward_agent", "git_signing_key"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			value := `["GITHUB_TOKEN"]`
			if key == "forward_agent" {
				value = "true"
			}
			e.WriteFile(".devcrate.toml", "provider = \"fake\"\n"+key+" = "+value+"\n")
			r := e.Devcrate("up")
			want := "devcrate: .devcrate.toml: " + key + " can only be set in your user config"
			if r.Code != 1 || !strings.Contains(r.Stderr, want) {
				t.Errorf("up = exit %d stderr %q, want exit 1 with %q", r.Code, r.Stderr, want)
			}
			if e.ContainerID() != "" {
				t.Errorf("up created a VM for a repo asking for credentials")
			}
		})
	}
}

// forwardConfig forwards DEVCRATE_E2E_TOKEN and ~/.config/e2e/token.
const forwardConfig = "forward_env = [\"DEVCRATE_E2E_TOKEN\"]\nforward_files = [\"~/.config/e2e/token\"]\n"

// forward sets the laptop's DEVCRATE_E2E_TOKEN to token and its
// ~/.config/e2e/token file to "file " + token.
func (e *Env) forward(token string) {
	e.t.Helper()
	e.LaptopEnv = []string{"DEVCRATE_E2E_TOKEN=" + token}
	must(os.MkdirAll(filepath.Join(e.Home, ".config", "e2e"), 0o700))
	must(os.WriteFile(filepath.Join(e.Home, ".config", "e2e", "token"), []byte("file "+token), 0o600))
}

// randomToken is a fresh secret with characters that need shell quoting.
func randomToken() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "tok 'en' $HOME " + hex.EncodeToString(b)
}

// loginShellSees is what a login shell prints for the forwarded var and file.
const loginShellSees = `printf '%s|' "${DEVCRATE_E2E_TOKEN-unset}"; cat "$HOME/.config/e2e/token" 2>/dev/null || echo missing`

func TestForwardedCredentialsOnTheVM(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.UserConfig(forwardConfig)
	first := randomToken()
	e.forward(first)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("ssh", "--", "bash -lc "+shellQuote(loginShellSees)); r.Stdout != first+"|file "+first {
		t.Errorf("VM login shell sees %q exit %d, want %q", r.Stdout, r.Code, first+"|file "+first)
	}
	if r := e.PlainSSH("stat -c '%a %U' /run/devcrate/creds/env /run/devcrate/creds/files/.config/e2e/token"); r != "600 root\n600 root\n" {
		t.Errorf("creds modes = %q, want 0600 root", r)
	}

	// A rotated secret reaches the VM on the next devcrate ssh.
	second := randomToken()
	e.forward(second)
	if r := e.Devcrate("ssh", "--", "bash -lc "+shellQuote(loginShellSees)); r.Stdout != second+"|file "+second {
		t.Errorf("after rotating, VM login shell sees %q exit %d, want %q", r.Stdout, r.Code, second+"|file "+second)
	}

	for _, token := range []string{first, second} {
		if out, code := e.VM("grep -rlF -- " + shellQuote(token) + " /var/lib/devcrate"); code != 1 {
			t.Errorf("forwarded value found on the volume (grep exit %d): %s", code, out)
		}
	}

	// Stop clears tmpfs; an up without the keys forwards nothing.
	if r := e.Devcrate("stop"); r.Code != 0 {
		t.Fatalf("stop exit %d", r.Code)
	}
	e.UserConfig("")
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up without forwarding exit %d", r.Code)
	}
	if r := e.Devcrate("ssh", "--", "bash -lc "+shellQuote(loginShellSees)); r.Stdout != "unset|missing\n" {
		t.Errorf("after stop and up without the keys, VM login shell sees %q, want nothing forwarded", r.Stdout)
	}
	if out, code := e.VM("grep -rlF -- " + shellQuote(second) + " /run /var/lib/devcrate"); code != 1 {
		t.Errorf("forwarded value still on the VM (grep exit %d): %s", code, out)
	}
}

func TestForwardedCredentialsInTheDevcontainer(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	// postCreateCommand records a hash, so the value itself never lands on the volume.
	e.WriteFile(".devcontainer/devcontainer.json", `{"image": "alpine:3.20", "postCreateCommand": "printf %s \"$DEVCRATE_E2E_TOKEN\" | sha256sum > /post-create"}`)
	e.Push()
	e.UserConfig(forwardConfig)
	first := randomToken()
	e.forward(first)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("ssh", "--container", "--", "sh -lc "+shellQuote(loginShellSees)); r.Stdout != first+"|file "+first {
		t.Errorf("container login shell sees %q exit %d, want %q", r.Stdout, r.Code, first+"|file "+first)
	}
	if r := e.Devcrate("ssh", "--container", "--", "cut -c1-64 /post-create"); r.Stdout != sha256hex(first)+"\n" {
		t.Errorf("postCreateCommand saw a value hashing to %q, want %q", r.Stdout, sha256hex(first))
	}
	if r := e.Devcrate("ssh", "--container", "--", "touch /run/devcrate/creds/env"); r.Code == 0 {
		t.Errorf("the devcontainer can write to the forwarded credentials")
	}

	// A rotated secret reaches the running container through the mount.
	second := randomToken()
	e.forward(second)
	if r := e.Devcrate("ssh", "--container", "--", "sh -lc "+shellQuote(loginShellSees)); r.Stdout != second+"|file "+second {
		t.Errorf("after rotating, container login shell sees %q exit %d, want %q", r.Stdout, r.Code, second+"|file "+second)
	}
	for _, token := range []string{first, second} {
		if out, code := e.VM("grep -rlF -- " + shellQuote(token) + " /var/lib/devcrate"); code != 1 {
			t.Errorf("forwarded value found on the volume (grep exit %d): %s", code, out)
		}
	}
}

func TestNonRootRemoteUserReadsForwardedCredentials(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	// The devcontainer CLI runs as root on the VM, so by default it rewrites
	// the remoteUser's uid to 0; updateRemoteUserUID = false keeps it 1000.
	e.WriteFile(".devcontainer/devcontainer.json", `{"build": {"dockerfile": "Dockerfile"}, "remoteUser": "dev", "updateRemoteUserUID": false}`)
	e.WriteFile(".devcontainer/Dockerfile", "FROM alpine:3.20\nRUN adduser -D -u 1000 dev\n")
	e.Push()
	e.UserConfig(forwardConfig)
	first := randomToken()
	e.forward(first)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	asDev := func() string {
		out, _ := e.VM("docker exec -u dev $(docker ps -q --filter label=devcontainer.local_folder=/var/lib/devcrate/workspace) sh -lc " + shellQuote(loginShellSees))
		return out
	}
	if got := asDev(); got != first+"|file "+first {
		t.Errorf("remoteUser's login shell sees %q, want %q", got, first+"|file "+first)
	}
	// devcrate ssh --container lands as the remoteUser, not the image's user.
	if r := e.Devcrate("ssh", "--container", "--", "id -un; echo $HOME"); r.Code != 0 || r.Stdout != "dev\n/home/dev\n" {
		t.Errorf("ssh --container -- id -un = %q exit %d, want dev with its home", r.Stdout, r.Code)
	}
	// The SSH-agent relay is for root and the remoteUser only.
	if out, _ := e.VM("stat -c '%a %u' /run/devcrate/ssh-agent/agent.sock"); out != "600 1000\n" {
		t.Errorf("agent relay socket mode and owner = %q, want 600 owned by the remoteUser's uid 1000", out)
	}
	// Readable by its owner only, on the VM too; the directory is traversable.
	if out, _ := e.VM("stat -c '%a %u' /run/devcrate/creds /run/devcrate/creds/env /run/devcrate/creds/files/.config/e2e/token"); out != "711 1000\n600 1000\n600 1000\n" {
		t.Errorf("creds modes and owners = %q, want 0711/0600 owned by the remoteUser's uid 1000", out)
	}

	// A push from devcrate ssh keeps them readable by the remoteUser.
	second := randomToken()
	e.forward(second)
	if r := e.Devcrate("ssh", "--", "true"); r.Code != 0 {
		t.Fatalf("ssh exit %d", r.Code)
	}
	if got := asDev(); got != second+"|file "+second {
		t.Errorf("after rotating, remoteUser's login shell sees %q, want %q", got, second+"|file "+second)
	}
}

func TestDockerfileReceivesBuildSecret(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	must(os.MkdirAll(filepath.Join(e.Dir, ".devcontainer"), 0o755))
	// The README's opt-in snippet. The image keeps only a hash of the secret.
	e.WriteFile(".devcontainer/devcontainer.json", `{"build": {"dockerfile": "Dockerfile", "options": ["--secret", "id=DEVCRATE_E2E_TOKEN"]}}`)
	e.WriteFile(".devcontainer/Dockerfile", "FROM alpine:3.20\n"+
		"RUN --mount=type=secret,id=DEVCRATE_E2E_TOKEN sha256sum /run/secrets/DEVCRATE_E2E_TOKEN > /build-secret\n")
	e.Push()
	e.UserConfig(forwardConfig)
	token := randomToken()
	e.forward(token)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	if r := e.Devcrate("ssh", "--container", "--", "cut -c1-64 /build-secret"); r.Stdout != sha256hex(token)+"\n" {
		t.Errorf("the build saw a secret hashing to %q exit %d, want %q", r.Stdout, r.Code, sha256hex(token))
	}
}

// recordSeen appends hashes of the forwarded var and file, as the script
// sees them, to /tmp/seen.
const recordSeen = `printf %s "$DEVCRATE_E2E_TOKEN" | sha256sum | cut -c1-64 >> /tmp/seen
sha256sum < "$HOME/.config/e2e/token" | cut -c1-64 >> /tmp/seen
`

func TestSetupScriptsSeeForwardedCredentials(t *testing.T) {
	for _, inContainer := range []bool{false, true} {
		name := "on the VM"
		if inContainer {
			name = "user script in the devcontainer"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := NewEnv(t)
			e.WriteFile(".devcrate.toml", "provider = \"fake\"\nsetup_script = \"setup.sh\"\n")
			e.WriteFile("setup.sh", recordSeen)
			if inContainer {
				e.withDevcontainer()
			}
			e.Push()
			e.UserConfig(forwardConfig + fmt.Sprintf("setup_script_in_container = %t\n", inContainer))
			e.UserSetupScript(recordSeen)
			token := randomToken()
			e.forward(token)
			if r := e.Devcrate("up"); r.Code != 0 {
				t.Fatalf("up exit %d", r.Code)
			}
			seen := sha256hex(token) + "\n" + sha256hex("file "+token) + "\n"
			if out, _ := e.VM("cat /tmp/seen"); inContainer && out != seen || !inContainer && out != seen+seen {
				t.Errorf("VM scripts saw %q, want the repo script's (and on the VM the user script's) %q", out, seen)
			}
			if inContainer {
				if r := e.Devcrate("ssh", "--container", "--", "cat /tmp/seen"); r.Stdout != seen {
					t.Errorf("container user script saw %q exit %d, want %q", r.Stdout, r.Code, seen)
				}
			}
		})
	}
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
