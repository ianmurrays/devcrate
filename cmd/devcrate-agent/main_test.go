package main

import (
	"strings"
	"testing"
	"time"
)

func TestCountEstablishedCountsOnlyConnectionsToSSH(t *testing.T) {
	// /proc/net/tcp: a listener on :22, two sessions to :22, an outgoing
	// connection from an ephemeral port to a remote :22, and a TIME_WAIT on :22.
	const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0200A8C0:0016 0100A8C0:C350 01 00000000:00000000 02:000AFC3E 00000000     0        0 2 4 0000000000000000 20 4 30 10 -1
   2: 0200A8C0:0016 0100A8C0:C351 01 00000000:00000000 02:000AFC3E 00000000     0        0 3 4 0000000000000000 20 4 30 10 -1
   3: 0200A8C0:C352 0300A8C0:0016 01 00000000:00000000 00:00000000 00000000     0        0 4 1 0000000000000000 20 4 30 10 -1
   4: 0200A8C0:0016 0100A8C0:C353 06 00000000:00000000 03:00000F9A 00000000     0        0 0 3 0000000000000000
`
	if n := countEstablished(strings.NewReader(tcp), 22); n != 2 {
		t.Errorf("countEstablished = %d, want 2", n)
	}
}

func TestIdleClockStartsWhenSetupExits(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := idleClock{since: t0}
	if d := c.observe(t0.Add(10*time.Second), true, time.Time{}); d != 0 {
		t.Errorf("busy: idle %s, want 0", d)
	}
	// Setup exited at +15 s, between polls.
	if d := c.observe(t0.Add(20*time.Second), false, t0.Add(15*time.Second)); d != 5*time.Second {
		t.Errorf("idle since setup exited = %s, want 5s", d)
	}
	if d := c.observe(t0.Add(80*time.Second), false, t0.Add(15*time.Second)); d != 65*time.Second {
		t.Errorf("idle a minute later = %s, want 65s", d)
	}
}

func TestIdleClockStartsWhenKeepAliveExpires(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := idleClock{since: t0}
	until := t0.Add(time.Minute) // a Keep-alive held until +60 s
	if d := c.observe(t0.Add(30*time.Second), false, until); d > 0 {
		t.Errorf("during the Keep-alive: idle %s, want none", d)
	}
	if d := c.observe(t0.Add(70*time.Second), false, until); d != 10*time.Second {
		t.Errorf("idle 10 s after the Keep-alive expired = %s, want 10s", d)
	}
}

func TestKeepAliveDeadlineRoundTrips(t *testing.T) {
	path := t.TempDir() + "/run/keep-alive"
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := keepAliveUntil(path); !got.IsZero() {
		t.Errorf("no Keep-alive: deadline %s, want zero", got)
	}
	until, err := holdFor(path, now, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(2 * time.Hour); !until.Equal(want) || !keepAliveUntil(path).Equal(want) {
		t.Errorf("deadline = %s, read back %s, want %s", until, keepAliveUntil(path), want)
	}
	if _, err := holdFor(path, now, 0); err == nil {
		t.Error("a zero Keep-alive was accepted")
	}
}
