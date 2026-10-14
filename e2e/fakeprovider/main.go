// devcrate-provider-fake is the test-only Provider. Its "VM" is a privileged
// systemd container (image from $DEVCRATE_FAKE_IMAGE, built from e2e/vm) with
// a named Docker volume at /var/lib/devcrate. The container's port 22 is
// published on 127.0.0.1, since container IPs are not routable from macOS.
//
// A stopped Environment is its volume with no container.
//
// Test plumbing: the option `fail = "<verb>"` makes that verb fail, and
// `busy_while = "<host path>"` makes status report busy while that file exists.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type request struct {
	EnvironmentID string         `json:"environment_id"`
	Options       map[string]any `json:"options"`
	SSHPublicKey  string         `json:"ssh_public_key"`
	UserData      string         `json:"user_data"`
}

type result struct {
	State string `json:"state"`
	Host  string `json:"host,omitempty"`
	User  string `json:"user,omitempty"`
	Port  int    `json:"port,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		fail("usage: devcrate-provider-fake <verb>")
	}
	verb := os.Args[1]
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fail("decoding request: %v", err)
	}
	if req.Options["fail"] == verb {
		fail("failing %s as asked", verb)
	}
	var out any
	switch verb {
	case "up":
		out = up(req)
	case "status":
		out = status(req.EnvironmentID)
		if path, ok := req.Options["busy_while"].(string); ok {
			if _, err := os.Stat(path); err == nil {
				out = result{State: "busy"}
			}
		}
	case "list":
		out = list()
	case "stop":
		// A stopped fake VM has no container, only its volume, like a
		// destroyed droplet with its block volume left behind.
		removeContainer(req.EnvironmentID)
		return
	case "delete":
		removeContainer(req.EnvironmentID)
		if volumeExists(name(req.EnvironmentID)) {
			docker(nil, "volume", "rm", name(req.EnvironmentID))
		}
		return
	default:
		fail("unknown verb %q", verb)
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

func name(env string) string { return "devcrate-fake-" + env }

func up(req request) result {
	c := name(req.EnvironmentID)
	labels := []string{"--label", "devcrate.fake=1", "--label", "devcrate.environment=" + req.EnvironmentID}
	switch containerState(c) {
	case "running":
		return status(req.EnvironmentID)
	case "":
		image := os.Getenv("DEVCRATE_FAKE_IMAGE")
		if image == "" {
			fail("DEVCRATE_FAKE_IMAGE is not set")
		}
		docker(nil, append([]string{"volume", "create"}, append(labels, c)...)...)
		args := []string{"run", "-d", "--name", c, "--hostname", c,
			"--privileged", "--cgroupns=private",
			"--tmpfs", "/run", "--tmpfs", "/run/lock",
			"-v", c + ":/var/lib/devcrate",
			// dockerd can't put overlayfs on the container's overlay rootfs.
			"-v", "/var/lib/docker", "-v", "/var/lib/containerd",
			"-p", "127.0.0.1::22",
			"--add-host", "host.docker.internal:host-gateway",
		}
		docker(nil, append(append(args, labels...), image)...)
	default:
		docker(nil, "start", c)
	}
	// Boot: wait for systemd, then do what a cloud does with ssh_public_key
	// and user_data. Exit code is non-zero when "degraded"; that's fine here.
	exec.Command("docker", "exec", c, "systemctl", "is-system-running", "--wait").Run()
	docker(strings.NewReader(req.SSHPublicKey+"\n"), "exec", "-i", c, "sh", "-c",
		"mkdir -p /root/.ssh && cat > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys")
	docker(strings.NewReader(req.UserData), "exec", "-i", c, "sh", "-s")
	return status(req.EnvironmentID)
}

func status(env string) result {
	c := name(env)
	if s := state(env); s != "running" {
		return result{State: s}
	}
	hostPort := strings.TrimSpace(docker(nil, "port", c, "22/tcp"))
	hostPort = strings.SplitN(hostPort, "\n", 2)[0]
	port, err := strconv.Atoi(hostPort[strings.LastIndex(hostPort, ":")+1:])
	if err != nil {
		fail("parsing published port %q: %v", hostPort, err)
	}
	return result{State: "running", Host: "127.0.0.1", User: "root", Port: port}
}

// state is the Environment's state word. Unlike status it reads no port, so
// list doesn't fail on another test's VM that is being removed.
func state(env string) string {
	c := name(env)
	switch containerState(c) {
	case "":
		if volumeExists(c) {
			return "stopped"
		}
		return "not_found"
	case "running":
		return "running"
	default:
		return "stopped"
	}
}

type listItem struct {
	EnvironmentID string `json:"environment_id"`
	State         string `json:"state"`
}

func list() []listItem {
	items := []listItem{}
	// Every Environment has a volume; a stopped one has nothing else.
	out := docker(nil, "volume", "ls", "--filter", "label=devcrate.fake=1", "--format", `{{.Label "devcrate.environment"}}`)
	for _, env := range strings.Fields(out) {
		items = append(items, listItem{env, state(env)})
	}
	return items
}

func removeContainer(env string) {
	if containerState(name(env)) != "" {
		docker(nil, "rm", "-f", "-v", name(env)) // -v: the anonymous docker/containerd volumes
	}
}

func volumeExists(v string) bool {
	return exec.Command("docker", "volume", "inspect", v).Run() == nil
}

// containerState is Docker's state word for the container, or "" if missing.
func containerState(c string) string {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", c).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// docker runs a docker command, sending its output to stderr (stdout is
// reserved for the contract's JSON), and returns its stdout.
func docker(stdin *strings.Reader, args ...string) string {
	cmd := exec.Command("docker", args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fail("docker %s: %v", strings.Join(args, " "), err)
	}
	return stdout.String()
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fake: "+format+"\n", args...)
	os.Exit(1)
}
