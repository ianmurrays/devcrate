//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
)

func TestRepoConfigWinsOverUserConfig(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)

	// A value only in the user file reaches the Provider.
	e.UserConfig("[providers.fake]\nfail = \"status\"\n")
	if r := e.Devcrate("status"); r.Code != 1 || !strings.Contains(r.Stderr, "fake: failing status as asked") {
		t.Fatalf("status with user-only fail = exit %d stderr %q, want the user value used", r.Code, r.Stderr)
	}

	// The same key in .devcrate.toml takes the repo's value.
	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nfail = \"nothing\"\n")
	if r := e.Devcrate("status"); r.Code != 0 || r.Stdout != "missing\n" {
		t.Fatalf("status with repo fail = %q exit %d, want the repo value used", r.Stdout, r.Code)
	}
}

func TestNameCollisionWithAnotherOriginAbortsSetup(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}

	// Another repo with the same name: the VM's workspace has a different origin.
	other := gitBaseURL + "/elsewhere/" + e.Name + ".git"
	if err := sh(e.Dir, "git", "remote", "set-url", "origin", other); err != nil {
		t.Fatal(err)
	}
	r := e.Devcrate("up")
	if r.Code != 1 || !strings.Contains(r.Stderr, "set name in .devcrate.toml") {
		t.Fatalf("up from another origin = exit %d stderr %q, want exit 1 asking to set name", r.Code, r.Stderr)
	}
	if _, code := e.VM("test -f /run/devcrate/ready"); code == 0 {
		t.Error("aborted setup left the Ready marker")
	}
	if out, _ := e.VM("git -C /var/lib/devcrate/workspace remote get-url origin"); strings.Contains(out, "elsewhere") {
		t.Errorf("workspace origin changed to %q", out)
	}
}

func TestUpWithGitURLOutsideARepo(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	// The only provider setting is in the repo's committed .devcrate.toml.
	r := e.DevcrateIn(t.TempDir(), "up", gitBaseURL+"/"+e.Name+".git")
	if r.Code != 0 {
		t.Fatalf("up <git-url> exit %d", r.Code)
	}
	wantLines(t, r.Stdout, "creating "+e.Name+"…", "ready: ssh devcrate-"+e.Name+"  (or: devcrate ssh "+e.Name+")")
	if out, code := e.VM("cat /var/lib/devcrate/workspace/README.md"); code != 0 || out != "fixture "+e.Name+"\n" {
		t.Errorf("workspace README = %q (exit %d), want the cloned repo", out, code)
	}
	if r := e.Devcrate("status"); r.Stdout != "ready\n" {
		t.Errorf("status in the local clone = %q, want ready", r.Stdout)
	}
}

func TestNameIsOptionalOnlyInsideARepo(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	e.UserConfig("[providers.fake]\n")

	if r := e.Devcrate("status"); r.Code != 0 || r.Stdout != "missing\n" {
		t.Errorf("status in the repo = %q exit %d, want missing", r.Stdout, r.Code)
	}
	r := e.DevcrateIn(t.TempDir(), "status")
	if r.Code != 1 || !strings.Contains(r.Stderr, "outside a repo, pass an Environment name") {
		t.Errorf("status outside a repo = exit %d stderr %q, want a missing-name error", r.Code, r.Stderr)
	}
}

func TestBareNameIsResolvedAcrossProviders(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)
	if r := e.Devcrate("up"); r.Code != 0 {
		t.Fatalf("up exit %d", r.Code)
	}
	// fake2 is only in ~/.config/devcrate/providers/, not on PATH. Being a
	// copy of fake, it reports the same Environments.
	e.InstallProvider("fake2")
	e.UserConfig("[providers.fake]\n[providers.fake2]\n")
	elsewhere := t.TempDir()

	r := e.DevcrateIn(elsewhere, "status", e.Name)
	if r.Code != 1 || !strings.Contains(r.Stderr, "fake, fake2") || !strings.Contains(r.Stderr, "--provider") {
		t.Fatalf("status of a name two Providers report = exit %d stderr %q, want an error naming both and --provider", r.Code, r.Stderr)
	}
	if r := e.DevcrateIn(elsewhere, "status", "--provider", "fake2", e.Name); r.Code != 0 || r.Stdout != "ready\n" {
		t.Errorf("status --provider fake2 = %q exit %d, want ready", r.Stdout, r.Code)
	}
	if r := e.Devcrate("status", "--provider", "fake2"); r.Code != 0 || r.Stdout != "ready\n" {
		t.Errorf("status --provider fake2 in the repo = %q exit %d, want ready", r.Stdout, r.Code)
	}

	r = e.DevcrateIn(elsewhere, "list")
	for _, p := range []string{"fake", "fake2"} {
		if !regexp.MustCompile(`(?m)^` + e.Name + ` +` + p + ` +running$`).MatchString(r.Stdout) {
			t.Errorf("list has no row %s %s running:\n%s", e.Name, p, r.Stdout)
		}
	}
}

func TestNameInRepoConfigOverridesDerivedName(t *testing.T) {
	t.Parallel()
	e := NewEnv(t)

	e.WriteFile(".devcrate.toml", "provider = \"fake\"\nname = \"other-name\"\n")
	if r := e.Devcrate("status", "--json"); r.Code != 0 || r.Stdout != `{"name":"other-name","state":"missing"}`+"\n" {
		t.Fatalf("status --json = %q exit %d, want name other-name", r.Stdout, r.Code)
	}

	for _, bad := range []string{"Bad_Name", strings.Repeat("a", 33)} {
		e.WriteFile(".devcrate.toml", "provider = \"fake\"\nname = \""+bad+"\"\n")
		r := e.Devcrate("status")
		if r.Code != 1 || !strings.Contains(r.Stderr, `invalid Environment name "`+bad+`": use only a-z, 0-9 and -, at most 32 characters`) {
			t.Errorf("status with name %q = exit %d stderr %q, want a clear invalid-name error", bad, r.Code, r.Stderr)
		}
	}
}
