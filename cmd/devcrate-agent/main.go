// devcrate-agent runs on an Environment's VM as the root systemd unit
// devcrate-agent.service. It watches for Idle and performs the Self-stop.
//
// `up` pushes this binary, the Provider's self-stop script and config.json to
// /var/lib/devcrate/agent/ (the volume, so the Agent survives a Stop) and
// runs `devcrate-agent install`, as does user data at every boot.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	state = "/var/lib/devcrate"
	// pushed is where `up` puts the Agent's files, on the volume.
	pushed = state + "/agent"
	// installed is the root-disk copy the unit runs: a binary running from
	// the volume would keep it from unmounting.
	installed = "/usr/local/lib/devcrate"
	unitPath  = "/etc/systemd/system/devcrate-agent.service"
	// selfStopEnv holds the Provider's self_stop_env (tmpfs, root-only).
	selfStopEnv = "/run/devcrate/self-stop.env"
	// setupLock is held by setup.sh while it runs and touched when it exits.
	setupLock  = "/run/devcrate/setup.lock"
	agentError = state + "/agent-error"
)

const unit = `[Unit]
Description=devcrate Agent: Idle detection and Self-stop
After=local-fs.target

[Service]
ExecStart=` + installed + `/devcrate-agent run
Restart=always
RestartSec=5
# A Self-stop in progress finishes before the Agent stops: SIGTERM goes to
# the Agent alone, which exits between polls.
KillMode=mixed
TimeoutStopSec=10min

[Install]
WantedBy=multi-user.target
`

// Config is config.json, written by `up`.
type Config struct {
	// IdleTimeout of continuous Idle triggers a Self-stop; 0 disables it.
	IdleTimeout duration `json:"idle_timeout"`
	// Poll is how often Idle is checked: 60 s unless a test overrides it.
	Poll duration `json:"poll"`
}

type duration struct{ time.Duration }

func (d duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	var err error
	d.Duration, err = time.ParseDuration(s)
	return err
}

func main() {
	log.SetFlags(0) // journald adds timestamps
	if len(os.Args) < 2 {
		log.Fatal("usage: devcrate-agent install | run")
	}
	var err error
	switch os.Args[1] {
	case "install":
		err = install()
	case "run":
		err = run()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal("devcrate-agent: ", err)
	}
}

// install copies the pushed files to the root disk and (re)starts the unit.
func install() error {
	if err := os.MkdirAll(installed, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"devcrate-agent", "self-stop", "config.json"} {
		if err := copyFile(filepath.Join(pushed, name), filepath.Join(installed, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	return sh("systemctl daemon-reload && systemctl enable -q devcrate-agent && systemctl restart devcrate-agent")
}

// copyFile replaces dst atomically, so a running dst is never overwritten.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst+".new", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(dst+".new", dst)
}

func run() error {
	cfg := Config{Poll: duration{time.Minute}}
	if data, err := os.ReadFile(filepath.Join(installed, "config.json")); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("config.json: %w", err)
		}
	}
	log.Printf("idle_timeout %s, poll %s", cfg.IdleTimeout, cfg.Poll)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)

	clock := idleClock{since: time.Now()}
	tick := time.NewTicker(cfg.Poll.Duration)
	defer tick.Stop()
	for {
		select {
		case <-term:
			return nil
		case now := <-tick.C:
			idle := clock.observe(now, busy(), setupExited())
			if cfg.IdleTimeout.Duration == 0 || idle < cfg.IdleTimeout.Duration {
				continue
			}
			log.Printf("Idle for %s: Self-stop", cfg.IdleTimeout)
			if err := selfStop(); err != nil {
				log.Printf("Self-stop failed: %v", err)
				recoverFrom(err)
				continue
			}
			// The cloud may take a while to destroy the VM; never retry
			// on an unmounted volume.
			log.Print("Self-stop requested; waiting for the VM to go away")
			<-term
			return nil
		}
	}
}

// idleClock measures continuous Idle.
type idleClock struct{ since time.Time }

// observe records one poll and returns how long the Environment has been
// Idle. setupExit (zero if unknown) is when setup last exited: the clock never
// starts before it.
func (c *idleClock) observe(now time.Time, busy bool, setupExit time.Time) time.Duration {
	if busy {
		c.since = now
		return 0
	}
	if setupExit.After(c.since) {
		c.since = setupExit
	}
	return now.Sub(c.since)
}

// busy reports whether an SSH connection or a running setup holds the
// Environment awake.
func busy() bool {
	n, err := sshConnections("/proc/net/tcp", "/proc/net/tcp6")
	if err != nil {
		log.Printf("reading connections: %v", err)
		return true
	}
	return n > 0 || setupRunning()
}

// sshConnections counts established TCP connections to local port 22 in
// /proc/net/tcp-format files.
func sshConnections(files ...string) (int, error) {
	n := 0
	for _, path := range files {
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return 0, err
		}
		n += countEstablished(f, 22)
		f.Close()
	}
	return n, nil
}

func countEstablished(r io.Reader, port int) int {
	n := 0
	s := bufio.NewScanner(r)
	s.Scan() // header
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 4 || f[3] != "01" { // 01 = ESTABLISHED
			continue
		}
		_, local, _ := strings.Cut(f[1], ":")
		if local == fmt.Sprintf("%04X", port) {
			n++
		}
	}
	return n
}

// setupRunning reports whether setup.sh holds its lock.
func setupRunning() bool {
	f, err := os.Open(setupLock)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// setupExited is when setup last exited (its lock's mtime), or zero.
func setupExited() time.Time {
	info, err := os.Stat(setupLock)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// selfStop quiesces the volume and runs the Provider's self-stop script. It
// returns only if the script failed (on success the VM is gone).
func selfStop() error {
	// After a reboot (not a Stop) the credential is gone from tmpfs until the
	// next `up`; don't take Docker down for nothing.
	if _, err := os.Stat(selfStopEnv); err != nil {
		return fmt.Errorf("no Self-stop credential on the VM (%v); the next devcrate up pushes it", err)
	}
	steps := []string{
		`ids=$(docker ps -q); if [ -n "$ids" ]; then docker stop $ids; fi`,
		// docker.socket too, or socket activation restarts dockerd.
		"systemctl stop docker docker.socket containerd",
		"sync",
	}
	for _, step := range steps {
		if err := sh(step); err != nil {
			return err
		}
	}
	for _, mnt := range []string{"/root", state} { // /root is bind-mounted from the volume
		if err := sh("! mountpoint -q " + mnt + " || umount " + mnt); err != nil {
			log.Printf("unmounting %s: %v (continuing)", mnt, err)
		}
	}
	return sh("set -a; . " + selfStopEnv + "; set +a; exec " + installed + "/self-stop")
}

// recoverFrom undoes a failed Self-stop's quiescing and records err on the
// volume for the next `up` or `ssh` to print.
func recoverFrom(failure error) {
	for _, step := range []string{
		"mountpoint -q " + state + " || mount " + state,
		"mountpoint -q /root || mount --bind " + state + "/home /root",
		// reset-failed: retries restart Docker often enough to hit
		// systemd's start rate limit.
		"systemctl reset-failed containerd docker.socket docker; systemctl start containerd docker.socket docker",
	} {
		if err := sh(step); err != nil {
			log.Printf("recovering: %v", err)
		}
	}
	msg := fmt.Sprintf("%s: Self-stop failed: %v\n", time.Now().UTC().Format(time.RFC3339), failure)
	if err := os.WriteFile(agentError, []byte(msg), 0o644); err != nil {
		log.Printf("writing %s: %v", agentError, err)
	}
}

// sh runs a shell command with its output in the journal. The error carries
// the output's last line.
func sh(script string) error {
	cmd := exec.Command("sh", "-c", script)
	var out strings.Builder
	cmd.Stdout = io.MultiWriter(os.Stdout, &out)
	cmd.Stderr = io.MultiWriter(os.Stderr, &out)
	if err := cmd.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		return fmt.Errorf("%s: %v: %s", script, err, lines[len(lines)-1])
	}
	return nil
}
