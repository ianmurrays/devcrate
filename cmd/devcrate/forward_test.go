package main

import (
	"reflect"
	"testing"
)

func TestForwardPortsMergesConfigAndFlags(t *testing.T) {
	cfg := []byte(`{"configuration": {
		"forwardPorts": [3000, "db:5432", 8080],
		"portsAttributes": {"3000": {"label": "web"}, "db:5432": {"label": "database"}, "9000": {"label": "extra"}}
	}}`)
	got, err := forwardPorts(cfg, []string{"3001:3000", "9000", "db:6379", "7000:db:6380"})
	if err != nil {
		t.Fatal(err)
	}
	want := []forwardPort{
		{target: "3000", local: 3001, label: "web"},
		{target: "db:5432", local: 5432, label: "database"},
		{target: "8080", local: 8080},
		{target: "9000", local: 9000, label: "extra"},
		{target: "db:6379", local: 6379},
		{target: "db:6380", local: 7000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forwardPorts =\n%+v\nwant\n%+v", got, want)
	}
}

func TestDetectedPortsFollowOnAutoForward(t *testing.T) {
	cfg, err := parsePortsConfig([]byte(`{"configuration": {
		"portsAttributes": {
			"3000": {"label": "web"},
			"5432": {"onAutoForward": "ignore"},
			"9000-9010": {"onAutoForward": "ignore", "label": "range"},
			"db:6000": {"onAutoForward": "ignore"},
			"^node": {"onAutoForward": "ignore"}
		}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	for port, want := range map[int]struct {
		label string
		ok    bool
	}{
		3000: {"web", true}, 5432: {"", false}, 9000: {"", false}, 9010: {"", false},
		9011: {"", true}, 6000: {"", true}, 8080: {"", true},
	} {
		if label, ok := cfg.autoForward(port); label != want.label || ok != want.ok {
			t.Errorf("autoForward(%d) = %q, %v; want %q, %v", port, label, ok, want.label, want.ok)
		}
	}
}

func TestOtherPortsAttributesIgnoreForwardsOnlyListedPorts(t *testing.T) {
	cfg, err := parsePortsConfig([]byte(`{"configuration": {
		"portsAttributes": {"3000": {"label": "web"}, "4000-4001": {}},
		"otherPortsAttributes": {"onAutoForward": "ignore"}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	for port, want := range map[int]bool{3000: true, 4001: true, 3001: false} {
		if _, ok := cfg.autoForward(port); ok != want {
			t.Errorf("autoForward(%d) = %v, want %v", port, ok, want)
		}
	}
	if _, ok := (portsConfig{}).autoForward(3000); !ok {
		t.Error("without a devcontainer.json every detected port is forwarded")
	}
}

func TestForwardPortsWithoutDevcontainer(t *testing.T) {
	got, err := forwardPorts(nil, []string{"22"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []forwardPort{{target: "22", local: 22}}; !reflect.DeepEqual(got, want) {
		t.Errorf("forwardPorts = %+v, want %+v", got, want)
	}
}

func TestForwardPortsRejectsBadSpecs(t *testing.T) {
	for _, spec := range []string{"", "x", "0", "70000", "3000:", "abc:3000:1", "3000:70000"} {
		if _, err := forwardPorts(nil, []string{spec}); err == nil {
			t.Errorf("-p %q: no error", spec)
		}
	}
}

func TestForwardPortLine(t *testing.T) {
	for p, want := range map[forwardPort]string{
		{target: "3000", local: 3000, label: "web"}: "3000 (web) -> localhost:3000",
		{target: "22", local: 2222}:                 "22 -> localhost:2222",
		{target: "db:5432", local: 5432}:            "db:5432 -> localhost:5432",
	} {
		if got := p.String(); got != want {
			t.Errorf("%+v = %q, want %q", p, got, want)
		}
	}
}
