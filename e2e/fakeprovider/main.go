// devcrate-provider-fake is the test-only Provider. Its "VM" is a privileged
// systemd container (image from $DEVCRATE_FAKE_IMAGE, built from e2e/vm) with
// a named Docker volume at /var/lib/devcrate. The container's port 22 is
// published on 127.0.0.1, since container IPs are not routable from macOS.
//
// A stopped Environment is its volume with no container.
//
// $DEVCRATE_FAKE_REGISTRY_MIRROR, when set, becomes the VM's
// /etc/docker/daemon.json registry-mirrors at boot.
//
// A Base image is a committed VM container, tagged
// devcrate-fake-base-<base_name>:<unix nanoseconds>; up boots from the newest,
// and status reports it.
//
// Test plumbing: the option `fail = "<verb>"` makes that verb fail, and
// `unknown = "<verb>"` makes it fail as an unknown verb, as in a Provider
// that does not implement it, and
// `busy_while = "<host path>"` makes status report busy while that file exists.
// `no_self_stop_env = true` makes up return no self_stop_env and self-stop
// fail, as when no Self-stop credential is configured, and
// `self_stop_fails = true` makes the self-stop script fail on the VM, and
// `save_image_fails = true` makes save-image fail after shutting the VM down.
//
// Self-stop: the VM reaches the host's Docker through its socket, mounted at
// hostDocker, and the script removes its own container, keeping the volume.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type request struct {
	EnvironmentID string         `json:"environment_id"`
	Options       map[string]any `json:"options"`
	SSHPublicKey  string         `json:"ssh_public_key"`
	UserData      string         `json:"user_data"`
	BaseName      string         `json:"base_name"`
	Check         bool           `json:"check"`
	Delete        bool           `json:"delete"`
}

type result struct {
	State string `json:"state"`
	Host  string `json:"host,omitempty"`
	User  string `json:"user,omitempty"`
	Port  int    `json:"port,omitempty"`

	SelfStopEnv map[string]string `json:"self_stop_env,omitempty"`
	BaseImage   *baseImage        `json:"base_image,omitempty"`
}

type baseImage struct {
	ID      string `json:"id"`
	Created string `json:"created"`
}

// hostDocker is where the host's Docker socket is mounted in the VM.
const hostDocker = "/devcrate-host-docker.sock"

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
	if req.Options["unknown"] == verb {
		verb = ""
	}
	var out any
	switch verb {
	case "up":
		res := up(req)
		res.SelfStopEnv = selfStopEnv(req)
		out = res
	case "status":
		res := status(req.EnvironmentID)
		if path, ok := req.Options["busy_while"].(string); ok {
			if _, err := os.Stat(path); err == nil {
				res = result{State: "busy"}
			}
		}
		if base := baseImages(req.BaseName); len(base) > 0 {
			created := docker(nil, "image", "inspect", "-f", "{{.Created}}", base[0])
			res.BaseImage = &baseImage{ID: base[0], Created: strings.TrimSpace(created)}
		}
		out = res
	case "list":
		out = list()
	case "self-stop":
		if req.Options["no_self_stop_env"] == true {
			fail("no Self-stop credential configured (no_self_stop_env)")
		}
		script := "#!/bin/sh\nexec docker rm -f -v \"$FAKE_VM\"\n"
		if req.Options["self_stop_fails"] == true {
			script = "#!/bin/sh\necho 'fake: self-stop failing as asked' >&2\nexit 1\n"
		}
		out = map[string]string{"script": script}
	case "save-image":
		if req.Delete {
			commitLock()
			deleteBaseImages(baseImages(req.BaseName))
			return
		}
		if req.Check {
			return
		}
		saveImage(req)
		return
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
		fail("unknown verb %q", os.Args[1])
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
		if base := baseImages(req.BaseName); len(base) > 0 {
			image = base[0]
		}
		if image == "" {
			fail("DEVCRATE_FAKE_IMAGE is not set")
		}
		docker(nil, append([]string{"volume", "create"}, append(labels, c)...)...)
		args := []string{"run", "-d", "--name", c, "--hostname", c,
			"--privileged", "--cgroupns=private",
			"--tmpfs", "/run", "--tmpfs", "/run/lock",
			"-v", c + ":/var/lib/devcrate",
			// A second mount of the volume and an fstab entry, so that the
			// Agent can re-mount /var/lib/devcrate as on a cloud.
			"-v", c + ":/mnt/devcrate-volume",
			"-v", "/var/run/docker.sock:" + hostDocker,
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
	// The image's own Docker settings, read by dockerd once setup restarts it:
	// pull Hub images through the harness's cache.
	if mirror := os.Getenv("DEVCRATE_FAKE_REGISTRY_MIRROR"); mirror != "" {
		docker(strings.NewReader(`{"registry-mirrors": ["`+mirror+`"]}`+"\n"), "exec", "-i", c, "sh", "-c",
			"mkdir -p /etc/docker && cat > /etc/docker/daemon.json")
	}
	docker(strings.NewReader(req.SSHPublicKey+"\n"), "exec", "-i", c, "sh", "-c",
		"mkdir -p /root/.ssh && cat > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys")
	// Kept at /var/lib/cloud/user-data (as cloud-init does), so tests can
	// check what it holds.
	docker(strings.NewReader(req.UserData), "exec", "-i", c, "sh", "-c",
		"grep -q devcrate-volume /etc/fstab || echo '/mnt/devcrate-volume /var/lib/devcrate none bind 0 0' >> /etc/fstab; "+
			"mkdir -p /var/lib/cloud && cat > /var/lib/cloud/user-data && sh /var/lib/cloud/user-data")
	return status(req.EnvironmentID)
}

// saveImage shuts the VM down, commits it as the newest Base image for
// base_name, untags every older one and removes the container, keeping the
// volume.
func saveImage(req request) {
	c := name(req.EnvironmentID)
	repo := baseRepo(req.BaseName)
	docker(nil, "stop", c)
	// As a cloud's snapshot can fail once the VM is halted.
	if req.Options["save_image_fails"] == true {
		fail("snapshot failing as asked (save_image_fails)")
	}
	commitLock()
	// The label lets the harness prune the untagged older ones.
	docker(nil, "commit", "-c", "LABEL devcrate.fake.base="+req.BaseName, c, fmt.Sprintf("%s:%d", repo, time.Now().UnixNano()))
	removeContainer(req.EnvironmentID)
	// Untagged, as a cloud deletes a snapshot that VMs still run from: a
	// Base image saved from a VM booted from an older one is its child, and
	// a sibling's VM may still run from it.
	deleteBaseImages(baseImages(req.BaseName)[1:])
}

// deleteBaseImages untags images.
func deleteBaseImages(images []string) {
	for _, image := range images {
		docker(nil, "rmi", "-f", image)
	}
}

// commitLock serializes image commits and removals: parallel commits of the
// same layer race in Docker's containerd image store ("failed to export
// layer"). Released on exit; held in a global so no finalizer closes it.
var lock *os.File

func commitLock() {
	var err error
	lock, err = os.OpenFile(filepath.Join(os.TempDir(), "devcrate-fake-commit.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fail("%v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		fail("locking %s: %v", lock.Name(), err)
	}
}

func baseRepo(baseName string) string { return "devcrate-fake-base-" + baseName }

// baseImages are base_name's Base images, newest first.
func baseImages(baseName string) []string {
	out := docker(nil, "images", baseRepo(baseName), "--format", "{{.Tag}}")
	tags := strings.Fields(out)
	sort.Sort(sort.Reverse(sort.StringSlice(tags))) // same-length decimal nanoseconds
	for i, tag := range tags {
		tags[i] = baseRepo(baseName) + ":" + tag
	}
	return tags
}

// selfStopEnv is what the self-stop script needs: how to reach the host's
// Docker and which container to remove (plus the token option, if any, so
// tests can follow a secret).
func selfStopEnv(req request) map[string]string {
	if req.Options["no_self_stop_env"] == true {
		return nil
	}
	env := map[string]string{"DOCKER_HOST": "unix://" + hostDocker, "FAKE_VM": name(req.EnvironmentID)}
	if token, ok := req.Options["token"].(string); ok {
		env["FAKE_TOKEN"] = token
	}
	return env
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
