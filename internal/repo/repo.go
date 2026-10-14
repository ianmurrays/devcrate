// Package repo reads the Environment's settings from a local clone.
package repo

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Context is what `up` needs to know about the repo it runs in.
type Context struct {
	Origin   string         // clone URL from `git remote get-url origin`
	Name     string         // Environment name: the repo name from Origin
	Provider string         // `provider` from .devcrate.toml
	Options  map[string]any // every other .devcrate.toml key, passed to the Provider unchanged
}

// Load reads the local clone containing dir.
func Load(dir string) (*Context, error) {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, errors.New("not inside a git repo: run devcrate in a local clone")
	}
	origin, err := git(dir, "remote", "get-url", "origin")
	if err != nil {
		return nil, errors.New("the repo has no origin remote")
	}
	c := &Context{
		Origin:  origin,
		Name:    strings.TrimSuffix(path.Base(strings.TrimSuffix(origin, "/")), ".git"),
		Options: map[string]any{},
	}
	if _, err := toml.DecodeFile(filepath.Join(root, ".devcrate.toml"), &c.Options); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(".devcrate.toml: %w", err)
	}
	c.Provider, _ = c.Options["provider"].(string)
	delete(c.Options, "provider")
	if c.Provider == "" {
		return nil, errors.New(`no provider set: add provider = "<name>" to .devcrate.toml`)
	}
	return c, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
