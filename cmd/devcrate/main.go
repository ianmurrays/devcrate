// devcrate runs dev Environments on cloud VMs.
package main

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

const usage = "usage: devcrate up | status [name]"

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
	switch args[0] {
	case "up":
		return up(args[1:])
	case "status":
		return status(args[1:])
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}

func up(args []string) error {
	if len(args) > 0 {
		if !isGitURL(args[0]) {
			return fmt.Errorf("%q is a bare Environment name: run devcrate up inside its repo, or pass a git URL", args[0])
		}
		return errors.New("devcrate up <git-url> is not supported yet")
	}
	env, err := load("")
	if err != nil {
		return err
	}

	fmt.Printf("creating %s…\n", env.Name)
	res, err := env.provider.Up(env.request())
	if err != nil {
		return err
	}
	if res.State != "running" {
		return fmt.Errorf("provider %s up returned state %q, want running", env.Provider, res.State)
	}

	fmt.Println("waiting for SSH…")
	client, err := remote.Dial(res.Host, res.Port, res.User, env.key, 3*time.Minute)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", env.Name, err)
	}
	defer client.Close()

	fmt.Println("running setup…")
	if err := remote.Run(client, "sh -s -- "+shellQuote(env.Origin), strings.NewReader(setupScript), os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}

	fmt.Printf("ready: ssh devcrate-%s  (or: devcrate ssh %s)\n", env.Name, env.Name)
	return nil
}

// status prints one state word and succeeds for every state.
func status(args []string) error {
	env, err := load("")
	if err != nil {
		return err
	}
	if len(args) > 0 {
		env.Name = args[0]
	}
	res, err := env.provider.Status(env.request())
	if err != nil {
		return err
	}
	state := res.State
	switch state {
	case "not_found":
		state = "missing"
	case "running":
		if client, err := remote.Dial(res.Host, res.Port, res.User, env.key, 0); err == nil {
			if remote.Run(client, "test -f /run/devcrate/ready", nil, nil, nil) == nil {
				state = "ready"
			}
			client.Close()
		}
	}
	fmt.Println(state)
	return nil
}

type environment struct {
	*repo.Context
	provider *provider.Provider
	key      *remote.Key
}

func load(dir string) (*environment, error) {
	if dir == "" {
		dir = "."
	}
	ctx, err := repo.Load(dir)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	configDir := filepath.Join(home, ".config", "devcrate")
	p, err := provider.Find(ctx.Provider, configDir)
	if err != nil {
		return nil, err
	}
	key, err := remote.LoadKey(configDir)
	if err != nil {
		return nil, err
	}
	return &environment{Context: ctx, provider: p, key: key}, nil
}

func (e *environment) request() provider.Request {
	return provider.Request{
		EnvironmentID: e.Name,
		Options:       e.Options,
		SSHPublicKey:  e.key.AuthorizedKey,
		UserData:      userData,
	}
}

// isGitURL reports whether s looks like a URL (scheme://…) or scp-style
// (user@host:path) git remote rather than a bare name.
func isGitURL(s string) bool {
	return strings.Contains(s, "://") || (strings.Contains(s, "@") && strings.Contains(s, ":"))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
