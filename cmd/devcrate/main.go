// devcrate runs dev Environments on cloud VMs.
package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/ianmurrays/devcrate/internal/provider"
	"github.com/ianmurrays/devcrate/internal/remote"
	"github.com/ianmurrays/devcrate/internal/repo"
)

var (
	//go:embed volume.sh
	volumeScript string
	//go:embed setup.sh
	setupSh string
	//go:embed userdata.sh
	userDataSh string
	//go:embed repofiles.sh
	repoFilesSh string
	//go:embed scrub.sh
	scrubScript string
	// Both put the volume's state in place with volume.sh's function.
	setupScript = volumeScript + repoFilesSh + setupSh
	userData    = volumeScript + userDataSh
)

const usage = "usage: devcrate up [--rebuild] [--name <name>] [git-url] | stop [name] | save-image [--delete] [name] | delete [--yes] [name] | status [--json] [name] | list [--json] | ssh [--container] [name] [-- <cmd>] | keep-alive [name] <duration> | forward [--no-detect] [name] [-p <remote> | -p <local>:<remote>]...  (each takes --provider <name>)"

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
	var cmd func(flags map[string]string, args []string) error
	// Every command takes --provider <name>; a trailing = marks a value flag.
	allowed := []string{"--provider="}
	switch args[0] {
	case "up":
		cmd, allowed = up, append(allowed, "--rebuild", "--name=")
	case "stop":
		cmd = stop
	case "save-image":
		cmd, allowed = saveImage, append(allowed, "--delete")
	case "delete":
		cmd, allowed = del, append(allowed, "--yes")
	case "status":
		cmd, allowed = status, append(allowed, "--json")
	case "list":
		cmd, allowed = list, append(allowed, "--json")
	case "ssh":
		cmd, allowed = sshCmd, append(allowed, "--container")
	case "keep-alive":
		cmd = keepAlive
	case "forward":
		cmd, allowed = forward, append(allowed, "-p=", "--no-detect")
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

// parseFlags splits the allowed flags from positional arguments. A boolean
// flag's value is "true"; a value flag (listed as "--name=") takes the next
// argument or --name=value; repeated, its last value wins, except -p, whose
// values are newline-joined.
func parseFlags(args, allowed []string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case slices.Contains(allowed, name) && !hasValue:
			flags[name] = "true"
		case slices.Contains(allowed, name+"="):
			if !hasValue {
				if i++; i == len(args) {
					return nil, nil, fmt.Errorf("flag %s needs a value\n%s", name, usage)
				}
				value = args[i]
			}
			if name == "-p" && flags[name] != "" {
				value = flags[name] + "\n" + value
			}
			flags[name] = value
		default:
			return nil, nil, fmt.Errorf("unknown flag %s\n%s", a, usage)
		}
	}
	return flags, rest, nil
}

func up(flags map[string]string, args []string) error {
	if len(args) > 1 {
		return errors.New(usage)
	}
	if len(args) == 1 && !isGitURL(args[0]) {
		return fmt.Errorf("%q is a bare Environment name: run devcrate up inside its repo, or pass a git URL", args[0])
	}
	name := flags["--name"]
	if name != "" && len(args) == 1 {
		return errors.New("--name works only in a local clone, not with a git URL")
	}
	env, err := fromRepo(flags["--provider"], name, args...)
	if err != nil {
		return err
	}
	// An Environment cannot be renamed: the worktree's stored one must go
	// first, so it is never orphaned.
	renaming := name != "" && name != env.storedName
	if renaming && env.storedName != "" {
		stored := *env
		stored.Name = env.storedName
		res, err := stored.provider.Status(stored.request())
		if err != nil {
			return err
		}
		if res.State != provider.NotFound {
			return fmt.Errorf("this worktree uses Environment %s, which still exists: run devcrate delete %s first, then devcrate up --name %s", stored.Name, stored.Name, name)
		}
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
		if res.State != provider.Busy {
			break
		}
		if !waited {
			fmt.Printf("%s is busy; waiting…\n", env.Name)
		}
		time.Sleep(busyPoll)
	}

	// Idle stop needs the Provider's self-stop script, and the verb fails
	// when no Self-stop credential is configured: ask before Up creates
	// anything.
	noCredential := fmt.Sprintf("so %s could not stop itself when Idle: configure the Provider's Self-stop credential (see its README), or set idle_timeout = 0 to turn idle stop off", env.Name)
	var selfStop string
	if env.idleTimeout != 0 {
		if selfStop, err = env.provider.SelfStop(env.request()); err != nil {
			return fmt.Errorf("%w, %s", err, noCredential)
		}
	}

	if renaming {
		if err := repo.StoreName(env.root, name); err != nil {
			return err
		}
	}

	fmt.Println("creating VM…") // "droplet" on DigitalOcean; Providers name it differently
	res, err := env.provider.Up(env.request())
	if err != nil {
		return err
	}
	if res.State != provider.Running {
		return fmt.Errorf("provider %s up returned state %q, want running", env.provider.Name, res.State)
	}
	if env.idleTimeout != 0 && len(res.SelfStopEnv) == 0 {
		return fmt.Errorf("provider %s returned no self_stop_env, %s", env.provider.Name, noCredential)
	}

	fmt.Println("waiting for SSH…")
	client, err := env.dialBooted(res, 3*time.Minute)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", env.Name, err)
	}
	defer client.Close()
	// Before setup, so a failed setup can still be inspected over ssh.
	if err := env.writeSSHConfig(res); err != nil {
		return err
	}
	if err := pushCreds(client, env); err != nil {
		return err
	}

	if err := env.installAgent(client, res, selfStop); err != nil {
		return fmt.Errorf("installing the Agent: %w", err)
	}
	// On the volume, so later commands can read it from the VM even outside
	// the clone.
	if err := remote.Run(client, "cat > /var/lib/devcrate/base-name", strings.NewReader(env.baseName()), os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("recording the base name: %w", err)
	}

	// setup.sh runs the user's script from here. 0600: /run/devcrate is
	// traversable by everyone (creds.sh).
	push := "rm -f /run/devcrate/user-setup.sh"
	if env.userScript != nil {
		push = "umask 077 && mkdir -p /run/devcrate && rm -f /run/devcrate/user-setup.sh && cat > /run/devcrate/user-setup.sh"
	}
	if err := remote.Run(client, push, bytes.NewReader(env.userScript), os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("pushing ~/.config/devcrate/setup.sh: %w", err)
	}

	fmt.Println("running setup…")
	cmd := fmt.Sprintf("sh -s -- %s %t %s %t %t %s", shellQuote(env.origin), env.devcontainer, shellQuote(env.setupScript), env.userScriptInContainer, flags["--rebuild"] != "", shellQuote(env.branch))
	// Setup gets the laptop's agent, for the setup scripts, git and builds
	// (and, through the Agent's relay, the devcontainer's lifecycle hooks).
	agentSock, err := forwardAgent()
	if err != nil {
		return err
	}
	if err := remote.RunWithAgent(client, agentSock, cmd, strings.NewReader(setupScript), os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}

	fmt.Printf("ready: ssh devcrate-%s  (or: devcrate ssh %s)\n", env.Name, env.Name)
	return nil
}

// stop ends compute billing and keeps the volume. It never asks.
func stop(flags map[string]string, args []string) error {
	env, err := resolve(flags["--provider"], args)
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

// saveImage saves a Ready Environment's root disk as its repo's Base image,
// leaving it Stopped (ADR 0009). With --delete it deletes the Base image.
func saveImage(flags map[string]string, args []string) error {
	env, err := resolve(flags["--provider"], args)
	if err != nil {
		return err
	}
	if env.provider == nil {
		return env.missing()
	}
	if flags["--delete"] != "" {
		// ponytail: the base name comes from the repo context, so a worktree
		// Environment's bare name outside its clone deletes nothing; read it
		// from the volume if that case matters.
		req := env.request()
		req.Delete = true
		if err := env.provider.SaveImage(req); err != nil {
			return err
		}
		fmt.Printf("deleted the Base image of %s\n", req.BaseName)
		return nil
	}
	unlock, err := lock(env.Name)
	if err != nil {
		return err
	}
	defer unlock()
	res, err := env.provider.Status(env.request())
	if err != nil {
		return err
	}
	client, ready := env.dialReady(res)
	if !ready {
		return fmt.Errorf("%s is not Ready: run devcrate up first", env.Name)
	}
	defer client.Close()

	// Read from the volume, as the request has no repo context for a bare
	// name.
	var base strings.Builder
	if err := remote.Run(client, "cat /var/lib/devcrate/base-name", nil, &base, os.Stderr); err != nil {
		return fmt.Errorf("reading %s's base name: %w: run devcrate up first", env.Name, err)
	}
	req := env.request()
	req.BaseName = strings.TrimSpace(base.String())
	req.Check = true
	var stderr strings.Builder
	check := *env.provider
	check.Stderr = &stderr
	if err := check.SaveImage(req); err != nil {
		return fmt.Errorf("provider %s does not support save-image: %s", env.provider.Name, strings.TrimSpace(stderr.String()))
	}

	fmt.Printf("saving the Base image of %s…\n", env.Name)
	if err := remote.Run(client, "sh -s", strings.NewReader(scrubScript), os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("removing devcrate's state from %s: %w: run devcrate up to recover it", env.Name, err)
	}
	req.Check = false
	if err := env.provider.SaveImage(req); err != nil {
		return fmt.Errorf("%w: run devcrate up to recover %s", err, env.Name)
	}
	// The host key pin stays, as on stop: the host keys live on the volume.
	fmt.Printf("saved the Base image of %s; %s is stopped\n", req.BaseName, env.Name)
	return nil
}

// del removes the VM and the volume, after a y/N prompt unless --yes.
func del(flags map[string]string, args []string) error {
	env, err := resolve(flags["--provider"], args)
	if err != nil {
		return err
	}
	if env.provider == nil {
		return env.missing()
	}
	if flags["--yes"] == "" {
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

// status prints one state word, then the Base image if any, and succeeds
// for every state.
func status(flags map[string]string, args []string) error {
	env, err := resolve(flags["--provider"], args)
	if err != nil {
		return err
	}
	state := "missing"
	var base *provider.BaseImage
	if env.provider != nil {
		res, err := env.provider.Status(env.request())
		if err != nil {
			return err
		}
		state, base = string(res.State), res.BaseImage
		switch res.State {
		case provider.NotFound:
			state = "missing"
		case provider.Running:
			if client, ready := env.dialReady(res); ready {
				state = "ready"
				client.Close()
			}
		}
	}
	if flags["--json"] != "" {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Name      string              `json:"name"`
			State     string              `json:"state"`
			BaseImage *provider.BaseImage `json:"base_image,omitempty"`
		}{env.Name, state, base})
	}
	fmt.Println(state)
	if base != nil {
		fmt.Printf("base image: %s (created %s)\n", base.ID, base.Created)
	}
	return nil
}

// dialReady connects to the VM res when the Environment is Ready.
func (e *environment) dialReady(res provider.Result) (*ssh.Client, bool) {
	if res.State != provider.Running {
		return nil, false
	}
	client, err := e.dial(res, 0)
	if err != nil {
		return nil, false
	}
	if remote.Run(client, "test -f /run/devcrate/ready", nil, nil, nil) != nil {
		client.Close()
		return nil, false
	}
	return client, true
}

// list prints every Environment of every known Provider: the current repo's
// and those configured under [providers.*] in the user config.
func list(flags map[string]string, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	names := cfg.providerNames()
	if flags["--provider"] != "" {
		names = []string{flags["--provider"]}
	} else if ctx, err := repo.Load(".", ""); err == nil {
		p := ctx.Provider
		if p == "" {
			p = cfg.Provider
		}
		if _, ok := cfg.Providers[p]; !ok && p != "" {
			names = append(names, p)
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
			rows = append(rows, row{it.EnvironmentID, name, string(it.State)})
		}
	}
	if flags["--json"] != "" {
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
	Name   string
	origin string // clone URL; empty when resolved outside a repo
	root   string // the laptop clone; empty without one
	base   string // the base name: Name without a linked worktree's suffix, --name or the stored name
	// storedName is the worktree's remembered name (repo.StoreName), "" when
	// it has none.
	storedName string
	// branch is the laptop checkout's branch, for a new clone: "HEAD" when
	// detached, "" for `up <git-url>` (the default branch, no warning).
	branch   string
	options  map[string]any // passed to the Provider unchanged
	provider *provider.Provider
	key      *remote.Key
	searched []string // Providers asked when resolving a bare name
	// devcontainer is false when the Environment is VM-only by choice.
	devcontainer bool
	// idleTimeout of continuous Idle makes the Agent Self-stop; 0 never.
	idleTimeout time.Duration
	// setupScript is the repo's setup_script; userScript is the user's
	// ~/.config/devcrate/setup.sh, nil when there is none.
	setupScript string
	userScript  []byte
	// userScriptInContainer is the user config's setup_script_in_container.
	userScriptInContainer bool
}

// fromRepo is the Environment of the repo at the git URL, if given, else of
// the repo in the current directory. providerName, if set, overrides the
// repo's Provider; name, if set, overrides the Environment name.
func fromRepo(providerName, name string, url ...string) (*environment, error) {
	var ctx *repo.Context
	var err error
	if len(url) == 1 {
		ctx, err = repo.FromURL(url[0])
	} else {
		ctx, err = repo.Load(".", name)
	}
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	for _, p := range []string{ctx.Provider, cfg.Provider} { // repo wins
		if providerName == "" {
			providerName = p
		}
	}
	if providerName == "" {
		return nil, fmt.Errorf(`no provider set: add provider = "<name>" to .devcrate.toml or %s`, filepath.Join(cfg.dir, "config.toml"))
	}
	p, err := provider.Find(providerName, cfg.dir)
	if err != nil {
		return nil, err
	}
	key, err := remote.LoadKey(cfg.dir)
	if err != nil {
		return nil, err
	}
	devcontainer := true
	for _, v := range []*bool{cfg.Devcontainer, ctx.Devcontainer} { // repo wins
		if v != nil {
			devcontainer = *v
		}
	}
	idleTimeout := repo.DefaultIdleTimeout
	if cfg.IdleTimeout != nil {
		if idleTimeout, err = repo.ParseIdleTimeout(cfg.IdleTimeout); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(cfg.dir, "config.toml"), err)
		}
	}
	if ctx.IdleTimeout != nil { // repo wins
		idleTimeout = *ctx.IdleTimeout
	}
	if _, ok := ctx.Options["setup_script_in_container"]; ok {
		fmt.Fprintln(os.Stderr, "devcrate: ignoring setup_script_in_container in .devcrate.toml: it is only read from ~/.config/devcrate/config.toml")
		delete(ctx.Options, "setup_script_in_container")
	}
	userScript, err := os.ReadFile(filepath.Join(cfg.dir, "setup.sh"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &environment{
		Name: ctx.Name, storedName: ctx.StoredName, branch: ctx.Branch, origin: ctx.Origin, root: ctx.Root, base: ctx.Base, options: cfg.options(providerName, ctx.Options), provider: p, key: key, devcontainer: devcontainer,
		setupScript: ctx.SetupScript, userScript: userScript, userScriptInContainer: cfg.SetupScriptInContainer,
		idleTimeout: idleTimeout,
	}, nil
}

// resolve finds the Environment for `[name]`. Without a name, it is the
// current repo's. A bare name is looked up in every Provider under
// [providers.*] (and the current repo's), and must be reported by at most
// one; provider is nil when none reports it. providerName, if set, is used
// without asking.
func resolve(providerName string, args []string) (*environment, error) {
	if len(args) > 1 {
		return nil, errors.New(usage)
	}
	env, repoErr := fromRepo(providerName, "")
	if len(args) == 0 {
		if errors.Is(repoErr, repo.ErrNotRepo) {
			return nil, errors.New("outside a repo, pass an Environment name")
		}
		return env, repoErr
	}
	name := args[0]
	if err := repo.CheckName(name); err != nil {
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

	// Each candidate Provider with the options it gets.
	candidates := map[string]map[string]any{}
	for p, opts := range cfg.Providers {
		candidates[p] = opts
	}
	if repoErr == nil {
		candidates[env.provider.Name] = env.options
	}
	if providerName != "" {
		candidates = map[string]map[string]any{providerName: candidates[providerName]}
	}
	searched := slices.Sorted(maps.Keys(candidates))
	if len(searched) == 0 {
		return nil, fmt.Errorf("outside a repo, devcrate needs Providers configured as [providers.<name>] in %s", filepath.Join(cfg.dir, "config.toml"))
	}

	var found []*environment
	for _, p := range searched {
		prov, err := provider.Find(p, cfg.dir)
		if err != nil {
			return nil, err
		}
		e := &environment{Name: name, options: candidates[p], provider: prov, key: key, searched: searched}
		if repoErr == nil && name == env.Name {
			e.root, e.base = env.root, env.base // the current repo's Environment, by name
		}
		if providerName != "" {
			return e, nil
		}
		res, err := prov.Status(e.request())
		if err != nil {
			return nil, err
		}
		if res.State != provider.NotFound {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return &environment{Name: name, key: key, searched: searched}, nil
	case 1:
		return found[0], nil
	}
	var names []string
	for _, e := range found {
		names = append(names, e.provider.Name)
	}
	return nil, fmt.Errorf("more than one Provider has an Environment named %s (%s): pass --provider <name> to pick one", name, strings.Join(names, ", "))
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
		BaseName:      e.baseName(),
	}
}

// baseName is the base name, or the Environment name when there is no repo
// context (`up <git-url>`, a bare name outside its clone).
func (e *environment) baseName() string {
	if e.base != "" {
		return e.base
	}
	return e.Name
}

// config is the user config, ~/.config/devcrate/config.toml.
type config struct {
	dir          string                    // ~/.config/devcrate
	Provider     string                    `toml:"provider"` // used when .devcrate.toml sets none
	Providers    map[string]map[string]any `toml:"providers"`
	Devcontainer *bool                     `toml:"devcontainer"`
	ForwardEnv   []string                  `toml:"forward_env"`
	// ForwardEnvCommands maps an env var to a laptop shell command whose
	// stdout is the forwarded value.
	ForwardEnvCommands map[string]string `toml:"forward_env_commands"`
	ForwardFiles       []string          `toml:"forward_files"`
	// ForwardRepoFiles maps an Environment name to paths in its repo,
	// copied from the laptop clone into the VM's clone (ADR 0007).
	ForwardRepoFiles map[string][]string `toml:"forward_repo_files"`
	IdleTimeout      any                 `toml:"idle_timeout"` // see repo.ParseIdleTimeout
	// SetupScriptInContainer runs ~/.config/devcrate/setup.sh in the
	// devcontainer instead of on the VM.
	SetupScriptInContainer bool `toml:"setup_script_in_container"`
	// ForwardAgent (default true) forwards the laptop's SSH agent on every
	// devcrate SSH connection and in the SSH config entry.
	ForwardAgent *bool `toml:"forward_agent"`
	// SSHAgent is the agent socket to forward instead of $SSH_AUTH_SOCK,
	// e.g. 1Password's.
	SSHAgent string `toml:"ssh_agent"`
	// GitSigningKey is a private SSH key under $HOME that signs every commit
	// and tag on the Environment (ADR 0006).
	GitSigningKey string `toml:"git_signing_key"`
}

// agentSock is the laptop's SSH agent socket to forward, or "" when the user
// config turns forwarding off.
func (c *config) agentSock() string {
	if c.ForwardAgent != nil && !*c.ForwardAgent {
		return ""
	}
	if c.SSHAgent == "" {
		return os.Getenv("SSH_AUTH_SOCK")
	}
	if c.SSHAgent == "~" || strings.HasPrefix(c.SSHAgent, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, c.SSHAgent[1:])
		}
	}
	return c.SSHAgent
}

// forwardAgent is the user config's agentSock.
func forwardAgent() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	return cfg.agentSock(), nil
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
	for _, name := range cfg.ForwardEnv {
		if _, ok := cfg.ForwardEnvCommands[name]; ok {
			return nil, fmt.Errorf("%s: %s is in both forward_env and forward_env_commands; keep one", path, name)
		}
	}
	for _, paths := range cfg.ForwardRepoFiles {
		for _, p := range paths {
			if err := checkRepoPath(p); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return cfg, nil
}

// options is the user's [providers.<name>] table overlaid with repo, whose
// values win.
func (c *config) options(name string, repo map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range c.Providers[name] {
		merged[k] = v
	}
	for k, v := range repo {
		merged[k] = v
	}
	return merged
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
