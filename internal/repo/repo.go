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

	"github.com/BurntSushi/toml"
)

// Context is what `up` needs to know about the repo it runs in.
type Context struct {
	Origin   string         // clone URL
	Name     string         // Environment name: `name` from .devcrate.toml, else the repo name from Origin
	Provider string         // `provider` from .devcrate.toml
	Options  map[string]any // every other .devcrate.toml key, passed to the Provider unchanged
	// Devcontainer is `devcontainer` from .devcrate.toml, nil when unset.
	Devcontainer *bool
	// SetupScript is `setup_script` from .devcrate.toml: a path in the repo,
	// run on the VM after the clone. Empty when unset.
	SetupScript string
}

// ErrNotRepo is Load's error when dir is not inside a git repo.
var ErrNotRepo = errors.New("not inside a git repo: run devcrate in a local clone")

// Load reads the local clone containing dir.
func Load(dir string) (*Context, error) {
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
	return parse(origin, string(data))
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
	c.Provider, _ = c.Options["provider"].(string)
	delete(c.Options, "provider")
	if c.Provider == "" {
		return nil, errors.New(`no provider set: add provider = "<name>" to .devcrate.toml`)
	}
	if v, ok := c.Options["devcontainer"]; ok {
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New(".devcrate.toml: devcontainer must be true or false")
		}
		c.Devcontainer = &b
		delete(c.Options, "devcontainer")
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
