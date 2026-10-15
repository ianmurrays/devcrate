package main

import (
	"reflect"
	"testing"
)

func TestParseFlagsRepeatsOnlyPorts(t *testing.T) {
	allowed := []string{"--provider=", "-p=", "--no-detect"}
	flags, rest, err := parseFlags([]string{"--provider", "a", "-p", "3000", "web", "--provider=b", "-p", "3001:3000,4000"}, allowed)
	if err != nil {
		t.Fatal(err)
	}
	// A single-valued flag given twice takes its last value; -p keeps every
	// value whole, so a comma is not a separator.
	want := map[string]string{"--provider": "b", "-p": "3000\n3001:3000,4000"}
	if !reflect.DeepEqual(flags, want) || !reflect.DeepEqual(rest, []string{"web"}) {
		t.Errorf("parseFlags = %q %q, want %q [web]", flags, rest, want)
	}
}

func TestForwardRejectsCommaSeparatedPorts(t *testing.T) {
	if _, err := forwardPorts(nil, []string{"3000,3001"}); err == nil {
		t.Error("-p 3000,3001 was accepted, want an error")
	}
}

func TestAgentSock(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/default.sock")
	off := false
	for _, tc := range []struct {
		name string
		cfg  config
		want string
	}{
		{"default agent", config{}, "/tmp/default.sock"},
		{"ssh_agent wins", config{SSHAgent: "/tmp/1p.sock"}, "/tmp/1p.sock"},
		{"tilde expands", config{SSHAgent: "~/Library/Group Containers/agent.sock"}, "/home/me/Library/Group Containers/agent.sock"},
		{"bare tilde", config{SSHAgent: "~"}, "/home/me"},
		{"forward_agent off", config{SSHAgent: "/tmp/1p.sock", ForwardAgent: &off}, ""},
	} {
		if got := tc.cfg.agentSock(); got != tc.want {
			t.Errorf("%s: agentSock() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
