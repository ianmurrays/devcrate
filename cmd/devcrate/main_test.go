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
