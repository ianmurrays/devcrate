// devcrate runs dev Environments on cloud VMs.
package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/term"

	"github.com/ianmurrays/devcrate/internal/provider"
	"github.com/ianmurrays/devcrate/internal/remote"
	"github.com/ianmurrays/devcrate/internal/repo"
)

var (
	//go:embed setup.sh
	setupScript string
	//go:embed userdata.sh
	userData string
)

const usage = "usage: devcrate up | stop [name] | delete [--yes] [name] | status [--json] [name] | list [--json] | ssh [name] [-- <cmd>]"

// busyPoll is how often `up` re-asks the Provider while it reports busy.
const busyPoll = 2 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "devcrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	var cmd func(flags map[string]bool, args []string) error
	var allowed []string
	switch args[0] {
	case "up":
		cmd = up
	case "stop":
		cmd = stop
	case "delete":
		cmd, allowed = del, []string{"--yes"}
	case "status":
		cmd, allowed = status, []string{"--json"}
	case "list":
		cmd, allowed = list, []string{"--json"}
	case "ssh":
		cmd = sshCmd
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
	// Everything after `--` is a remote command (ssh only), not flags.
	var tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		if args[0] != "ssh" {
			return fmt.Errorf("unexpected --\n%s", usage)
		}
		args, tail = args[:i], args[i:]
	}
	flags, rest, err := parseFlags(args[1:], allowed)
	if err != nil {
		return err
	}
	return cmd(flags, append(rest, tail...))
}

// parseFlags splits the allowed boolean flags from positional arguments.
func parseFlags(args, allowed []string) (map[string]bool, []string, error) {
	flags := map[string]bool{}
	var rest []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		ok := false
		for _, f := range allowed {
			ok = ok || a == f
		}
		if !ok {
			return nil, nil, fmt.Errorf("unknown flag %s\n%s", a, usage)
		}
		flags[a] = true
	}
	return flags, rest, nil
}

func up(_ map[string]bool, args []string) error {
	if len(args) > 0 {
		if !isGitURL(args[0]) {
			return fmt.Errorf("%q is a bare Environment name: run devcrate up inside its repo, or pass a git URL", args[0])
		}
		return errors.New("devcrate up <git-url> is not supported yet")
	}
	env, err := fromRepo()
	if err != nil {
		return err
	}
	unlock, err := lock(env.Name)
	if err != nil {
		return err
	}
	defer unlock()

	// A busy Environment is mid-transition (e.g. a Self-stop); wait it out.
	for waited := false; ; waited = true {
		res, err := env.provider.Status(env.request())
		if err != nil {
			return err
		}
		if res.State != "busy" {
			break
		}
		if !waited {
			fmt.Printf("%s is busy; waiting…\n", env.Name)
		}
		time.Sleep(busyPoll)
	}

	fmt.Printf("creating %s…\n", env.Name)
	res, err := env.provider.Up(env.request())
	if err != nil {
		return err
	}
	if res.State != "running" {
		return fmt.Errorf("provider %s up returned state %q, want running", env.provider.Name, res.State)
	}

	fmt.Println("waiting for SSH…")
	client, err := env.dial(res, 3*time.Minute)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", env.Name, err)
	}
	defer client.Close()
	// Before setup, so a failed setup can still be inspected over ssh.
	if err := env.writeSSHConfig(res); err != nil {
		return err
	}

	fmt.Println("running setup…")
	if err := remote.Run(client, "sh -s -- "+shellQuote(env.origin), strings.NewReader(setupScript), os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}

	fmt.Printf("ready: ssh devcrate-%s  (or: devcrate ssh %s)\n", env.Name, env.Name)
	return nil
}

// stop ends compute billing and keeps the volume. It never asks.
func stop(_ map[string]bool, args []string) error {
	env, err := resolve(args)
	if err != nil {
		return err
	}
	if env.provider == nil {
		return env.missing()
	}
	unlock, err := lock(env.Name)
	if err != nil {
		return err
	}
	defer unlock()
	fmt.Printf("stopping %s…\n", env.Name)
	if err := env.provider.Stop(env.request()); err != nil {
		return err
	}
	fmt.Printf("stopped %s\n", env.Name)
	return nil
}

// del removes the VM and the volume, after a y/N prompt unless --yes.
func del(flags map[string]bool, args []string) error {
	env, err := resolve(args)
	if err != nil {
		return err
	}
	if env.provider == nil {
		return env.missing()
	}
	if !flags["--yes"] {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("refusing to delete %s without a terminal to confirm: pass --yes", env.Name)
		}
		fmt.Printf("Delete Environment %s and its volume? [y/N] ", env.Name)
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return fmt.Errorf("%s not deleted", env.Name)
		}
	}
	unlock, err := lock(env.Name)
	if err != nil {
		return err
	}
	defer unlock()
	fmt.Printf("deleting %s…\n", env.Name)
	if err := env.provider.Delete(env.request()); err != nil {
		return err
	}
	if err := env.removeSSHConfig(); err != nil {
		return err
	}
	fmt.Printf("deleted %s\n", env.Name)
	return nil
}

// status prints one state word and succeeds for every state.
func status(flags map[string]bool, args []string) error {
	env, err := resolve(args)
	if err != nil {
		return err
	}
	state := "missing"
	if env.provider != nil {
		res, err := env.provider.Status(env.request())
		if err != nil {
			return err
		}
		state = res.State
		switch state {
		case "not_found":
			state = "missing"
		case "running":
			if client, err := env.dial(res, 0); err == nil {
				if remote.Run(client, "test -f /run/devcrate/ready", nil, nil, nil) == nil {
					state = "ready"
				}
				client.Close()
			}
		}
	}
	if flags["--json"] {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"name": env.Name, "state": state})
	}
	fmt.Println(state)
	return nil
}

// list prints every Environment of every known Provider: the current repo's
// and those configured under [providers.*] in the user config.
func list(flags map[string]bool, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	names := cfg.providerNames()
	if ctx, err := repo.Load("."); err == nil {
		if _, ok := cfg.Providers[ctx.Provider]; !ok {
			names = append(names, ctx.Provider)
		}
	}
	type row struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		State    string `json:"state"`
	}
	rows := []row{}
	for _, name := range names {
		p, err := provider.Find(name, cfg.dir)
		if err != nil {
			return err
		}
		items, err := p.List()
		if err != nil {
			return err
		}
		for _, it := range items {
			rows = append(rows, row{it.EnvironmentID, name, it.State})
		}
	}
	if flags["--json"] {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tSTATE")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Name, r.Provider, r.State)
	}
	return w.Flush()
}

type environment struct {
	Name     string
	origin   string         // clone URL; empty when resolved outside a repo
	options  map[string]any // passed to the Provider unchanged
	provider *provider.Provider
	key      *remote.Key
	searched []string // Providers asked when resolving a bare name
}

// fromRepo is the Environment of the repo in the current directory.
func fromRepo() (*environment, error) {
	ctx, err := repo.Load(".")
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	p, err := provider.Find(ctx.Provider, cfg.dir)
	if err != nil {
		return nil, err
	}
	key, err := remote.LoadKey(cfg.dir)
	if err != nil {
		return nil, err
	}
	return &environment{Name: ctx.Name, origin: ctx.Origin, options: ctx.Options, provider: p, key: key}, nil
}

// resolve finds the Environment for `[name]`. Inside a repo, the repo's
// Provider is used and name defaults to the repo's. Outside one, name is
// required and the first Provider under [providers.*] that knows it wins;
// provider is nil when none does.
func resolve(args []string) (*environment, error) {
	if len(args) > 1 {
		return nil, errors.New(usage)
	}
	env, err := fromRepo()
	if err == nil {
		if len(args) == 1 {
			env.Name = args[0]
		}
		return env, nil
	}
	if len(args) == 0 {
		return nil, err
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	key, err := remote.LoadKey(cfg.dir)
	if err != nil {
		return nil, err
	}
	env = &environment{Name: args[0], key: key, searched: cfg.providerNames()}
	if len(env.searched) == 0 {
		return nil, fmt.Errorf("outside a repo, devcrate needs Providers configured as [providers.<name>] in %s", filepath.Join(cfg.dir, "config.toml"))
	}
	for _, name := range env.searched {
		p, err := provider.Find(name, cfg.dir)
		if err != nil {
			return nil, err
		}
		env.provider, env.options = p, cfg.Providers[name]
		res, err := p.Status(env.request())
		if err != nil {
			return nil, err
		}
		if res.State != "not_found" {
			return env, nil
		}
	}
	env.provider, env.options = nil, nil
	return env, nil
}

func (e *environment) missing() error {
	return fmt.Errorf("no Environment named %s in Providers %s", e.Name, strings.Join(e.searched, ", "))
}

func (e *environment) request() provider.Request {
	return provider.Request{
		EnvironmentID: e.Name,
		Options:       e.options,
		SSHPublicKey:  e.key.AuthorizedKey,
		UserData:      userData,
	}
}

// config is the user config, ~/.config/devcrate/config.toml.
type config struct {
	dir       string                    // ~/.config/devcrate
	Providers map[string]map[string]any `toml:"providers"`
}

func loadConfig() (*config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cfg := &config{dir: filepath.Join(home, ".config", "devcrate")}
	path := filepath.Join(cfg.dir, "config.toml")
	if _, err := toml.DecodeFile(path, cfg); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *config) providerNames() []string {
	var names []string
	for name := range c.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// lock takes the laptop-side exclusive lock for one Environment, waiting for
// any other devcrate command that holds it. The lock is released on exit.
func lock(name string) (unlock func(), err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".local", "state", "devcrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr, "waiting for another devcrate command on %s…\n", name)
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
		}
	}
	return func() { f.Close() }, nil
}

// isGitURL reports whether s looks like a URL (scheme://…) or scp-style
// (user@host:path) git remote rather than a bare name.
func isGitURL(s string) bool {
	return strings.Contains(s, "://") || (strings.Contains(s, "@") && strings.Contains(s, ":"))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
