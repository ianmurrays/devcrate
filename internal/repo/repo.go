// Package repo reads the Environment's settings from a repo: a local clone,
// or a git URL.
package repo

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Context is what `up` needs to know about the repo it runs in.
type Context struct {
	Origin string // clone URL
	Root   string // the local clone's top directory; "" from FromURL
	Name   string // Environment name: see Load
	// Base is `name` from .devcrate.toml, else the repo name from Origin:
	// Name without a linked worktree's suffix, --name or the stored name.
	Base string
	// StoredName is the name in the worktree's devcrate-name file, "" when
	// it has none.
	StoredName string
	Provider   string         // `provider` from .devcrate.toml, "" when unset
	Options    map[string]any // every other .devcrate.toml key, passed to the Provider unchanged
	// Devcontainer is `devcontainer` from .devcrate.toml, nil when unset.
	Devcontainer *bool
	// IdleTimeout is `idle_timeout` from .devcrate.toml, nil when unset.
	IdleTimeout *time.Duration
	// SetupScript is `setup_script` from .devcrate.toml: a path in the repo,
	// run on the VM after the clone. Empty when unset.
	SetupScript string
}

// DefaultIdleTimeout applies when neither the repo nor the user sets one.
const DefaultIdleTimeout = 30 * time.Minute

// ParseIdleTimeout reads an idle_timeout value: a duration string such as
// "30m", or 0 to turn idle stop off.
func ParseIdleTimeout(v any) (time.Duration, error) {
	bad := fmt.Errorf(`idle_timeout must be a duration such as "30m", or 0 to turn idle stop off; got %v`, v)
	switch v := v.(type) {
	case int64:
		if v == 0 {
			return 0, nil
		}
	case string:
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d, nil
		}
	}
	return 0, bad
}

// ErrNotRepo is Load's error when dir is not inside a git repo.
var ErrNotRepo = errors.New("not inside a git repo: run devcrate in a local clone")

// Load reads the local clone containing dir. The Environment name is name,
// if set, else the worktree's stored name (see StoreName), else `name` from
// .devcrate.toml or the repo name, plus `-<worktree dir>` in a linked
// worktree (ADR 0008).
func Load(dir, name string) (*Context, error) {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, ErrNotRepo
	}
	origin, err := git(dir, "config", "--get", "remote.origin.url") // as written, without the laptop's insteadOf rewrites
	if err != nil {
		return nil, errors.New("the repo has no origin remote")
	}
	data, err := os.ReadFile(filepath.Join(root, ".devcrate.toml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	c, err := parse(origin, string(data))
	if err != nil {
		return nil, err
	}
	c.Root = root
	c.Base = c.Name
	file, err := nameFile(dir)
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(file); err == nil {
		c.StoredName = strings.TrimSpace(string(b))
		if err := CheckName(c.StoredName); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	switch {
	case name != "":
		if err := CheckName(name); err != nil {
			return nil, err
		}
		c.Name = name
	case c.StoredName != "":
		c.Name = c.StoredName
	default:
		linked, err := isLinkedWorktree(dir)
		if err != nil {
			return nil, err
		}
		if linked {
			// A linked worktree gets its own Environment (ADR 0008).
			c.Name += "-" + invalidNameChars.ReplaceAllString(strings.ToLower(filepath.Base(root)), "-")
			if !validName.MatchString(c.Name) {
				return nil, fmt.Errorf("this worktree's Environment name %q is longer than 32 characters: pick a shorter one with devcrate up --name <name>", c.Name)
			}
		}
	}
	return c, nil
}

// StoreName records name as the Environment name of the worktree containing
// dir, in devcrate-name in the worktree's own git dir, outside the working
// tree.
func StoreName(dir, name string) error {
	file, err := nameFile(dir)
	if err != nil {
		return err
	}
	return os.WriteFile(file, []byte(name+"\n"), 0o644)
}

func nameFile(dir string) (string, error) {
	gitDir, err := git(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --absolute-git-dir: %w", err)
	}
	return filepath.Join(gitDir, "devcrate-name"), nil
}

// FromURL reads .devcrate.toml from the default branch of the repo at url,
// with a shallow, blob-less, no-checkout clone that uses the local git
// credentials.
func FromURL(url string) (*Context, error) {
	tmp, err := os.MkdirTemp("", "devcrate-clone-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	clone := exec.Command("git", "clone", "-q", "--depth=1", "--filter=blob:none", "--no-checkout", url, tmp)
	clone.Stderr = os.Stderr
	if err := clone.Run(); err != nil {
		return nil, fmt.Errorf("cloning %s: %w", url, err)
	}
	data, err := git(tmp, "show", "HEAD:.devcrate.toml")
	if err != nil {
		return nil, fmt.Errorf("%s has no .devcrate.toml on its default branch", url)
	}
	return parse(url, data)
}

func parse(origin, devcrateToml string) (*Context, error) {
	c := &Context{
		Origin:  origin,
		Name:    strings.TrimSuffix(path.Base(strings.TrimSuffix(origin, "/")), ".git"),
		Options: map[string]any{},
	}
	if _, err := toml.Decode(devcrateToml, &c.Options); err != nil {
		return nil, fmt.Errorf(".devcrate.toml: %w", err)
	}
	// Which secrets leave the laptop is the user's call, never the repo's (ADR 0005).
	for _, key := range []string{"forward_env", "forward_env_commands", "forward_files", "forward_repo_files", "forward_agent", "ssh_agent", "git_signing_key"} {
		if _, ok := c.Options[key]; ok {
			return nil, fmt.Errorf(".devcrate.toml: %s can only be set in your user config (~/.config/devcrate/config.toml), so a repo cannot ask for your secrets", key)
		}
	}
	c.Provider, _ = c.Options["provider"].(string)
	delete(c.Options, "provider")
	if v, ok := c.Options["devcontainer"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New(".devcrate.toml: devcontainer must be true or false")
		}
		c.Devcontainer = &b
		delete(c.Options, "devcontainer")
	}
	if v, ok := c.Options["idle_timeout"]; ok {
		d, err := ParseIdleTimeout(v)
		if err != nil {
			return nil, fmt.Errorf(".devcrate.toml: %w", err)
		}
		c.IdleTimeout = &d
		delete(c.Options, "idle_timeout")
	}
	if v, ok := c.Options["setup_script"]; ok {
		if c.SetupScript, ok = v.(string); !ok {
			return nil, errors.New(".devcrate.toml: setup_script must be a path in the repo")
		}
		delete(c.Options, "setup_script")
	}
	if name, ok := c.Options["name"]; ok {
		c.Name = fmt.Sprint(name)
		delete(c.Options, "name")
	} else if CheckName(c.Name) != nil {
		return nil, fmt.Errorf("%w; set name in .devcrate.toml", CheckName(c.Name))
	}
	if err := CheckName(c.Name); err != nil {
		return nil, err
	}
	return c, nil
}

var validName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// CheckName fails unless name is a valid Environment name.
func CheckName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid Environment name %q: use only a-z, 0-9 and -, at most 32 characters", name)
	}
	return nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9-]`)

// isLinkedWorktree reports whether dir is in a linked git worktree rather
// than the main one: a linked worktree's git dir is not the common one.
func isLinkedWorktree(dir string) (bool, error) {
	var dirs [2]string
	for i, flag := range []string{"--git-dir", "--git-common-dir"} {
		d, err := git(dir, "rev-parse", flag)
		if err != nil {
			return false, fmt.Errorf("git rev-parse %s: %w", flag, err)
		}
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		if dirs[i], err = filepath.EvalSymlinks(d); err != nil {
			return false, err
		}
	}
	return dirs[0] != dirs[1], nil
}
