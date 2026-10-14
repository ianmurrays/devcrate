package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/ianmurrays/devcrate/internal/provider"
	"github.com/ianmurrays/devcrate/internal/remote"
	"github.com/ianmurrays/devcrate/internal/vm"
)

// agentDir is where the Agent's files go on the VM: on the volume, as user
// data installs from here at boot. devcrate-agent's main.go has the rest.
const agentDir = vm.VolumeDir + "/agent"

// agentPollEnv is the hidden, test-only override of the Agent's 60 s poll.
const agentPollEnv = "DEVCRATE_TEST_AGENT_POLL"

type pushFile struct {
	path string
	mode os.FileMode
	data []byte
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// installAgent pushes the Agent, its config, the Provider's self-stop script
// (when idle stop is on) and self_stop_env to the VM and (re)starts the
// Agent. It first stops the running Agent, which lets any Self-stop in
// progress finish, and prints the last Self-stop error.
func (e *environment) installAgent(client *ssh.Client, res provider.Result, selfStopScript string) error {
	if err := remote.Run(client, "systemctl stop devcrate-agent 2>/dev/null || true", nil, nil, os.Stderr); err != nil {
		return err
	}
	if err := printAgentError(client, true); err != nil {
		return err
	}

	var arch strings.Builder
	if err := remote.Run(client, "uname -m", nil, &arch, os.Stderr); err != nil {
		return err
	}
	goarch, ok := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[strings.TrimSpace(arch.String())]
	if !ok {
		return fmt.Errorf("the VM's CPU (%s) is not one devcrate has an Agent for", strings.TrimSpace(arch.String()))
	}
	binary, err := agentBinary(goarch)
	if err != nil {
		return err
	}

	poll := os.Getenv(agentPollEnv)
	if poll == "" {
		poll = "1m"
	}
	config, err := json.Marshal(map[string]string{"idle_timeout": e.idleTimeout.String(), "poll": poll})
	if err != nil {
		return err
	}
	files := []pushFile{
		{agentDir + "/devcrate-agent", 0o755, binary},
		{agentDir + "/config.json", 0o644, config},
	}
	if e.idleTimeout != 0 {
		var env strings.Builder
		for _, k := range slices.Sorted(maps.Keys(res.SelfStopEnv)) {
			if !envName.MatchString(k) {
				return fmt.Errorf("provider %s returned an invalid self_stop_env name %q", e.provider.Name, k)
			}
			fmt.Fprintf(&env, "%s=%s\n", k, shellQuote(res.SelfStopEnv[k]))
		}
		files = append(files,
			pushFile{agentDir + "/self-stop", 0o755, []byte(selfStopScript)},
			// tmpfs and root-only: never on the volume or in user data.
			pushFile{vm.SelfStopEnv, 0o600, []byte(env.String())},
		)
	}
	for _, f := range files {
		// umask only for the file: /run/devcrate holds things others read.
		cmd := fmt.Sprintf("mkdir -p %[1]s && (umask 077 && cat > %[2]s.new) && chmod %#[3]o %[2]s.new && mv %[2]s.new %[2]s",
			filepath.Dir(f.path), f.path, f.mode)
		if err := remote.Run(client, cmd, bytes.NewReader(f.data), os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("writing %s: %w", f.path, err)
		}
	}
	return remote.Run(client, agentDir+"/devcrate-agent install", nil, os.Stdout, os.Stderr)
}

// agentBinary is the Agent built for linux/goarch, which ships next to the
// devcrate executable as devcrate-agent-linux-<goarch>.
func agentBinary(goarch string) ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(exe), "devcrate-agent-linux-"+goarch)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no Agent for linux/%s next to devcrate: build it with `CGO_ENABLED=0 GOOS=linux GOARCH=%s go build -o %s ./cmd/devcrate-agent`", goarch, goarch, path)
	}
	return data, nil
}

// printAgentError prints the Agent's last Self-stop error, if any, to stderr,
// removing it when clear is set.
func printAgentError(client *ssh.Client, clear bool) error {
	cmd := "cat " + vm.AgentError + " 2>/dev/null || true"
	if clear {
		cmd = "cat " + vm.AgentError + " 2>/dev/null && rm -f " + vm.AgentError + " || true"
	}
	var out strings.Builder
	if err := remote.Run(client, cmd, nil, &out, os.Stderr); err != nil {
		return err
	}
	if s := strings.TrimSpace(out.String()); s != "" {
		fmt.Fprintf(os.Stderr, "the Agent could not stop this Environment when Idle: %s\n", s)
	}
	return nil
}
