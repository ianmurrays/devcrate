package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestListeningKeepsLoopbackAndWildcardListenersBelowTheEphemeralRange(t *testing.T) {
	// LISTEN (0A) on 127.0.0.1:3000, 0.0.0.0:8080, 172.17.0.2:5000 (specific
	// IP), 0.0.0.0:80 (< 1024), 0.0.0.0:40000 (ephemeral), plus an
	// ESTABLISHED connection from 127.0.0.1:4000.
	const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0
   2: 020011AC:1388 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 100 0 0 10 0
   3: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4 1 0000000000000000 100 0 0 10 0
   4: 00000000:9C40 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 5 1 0000000000000000 100 0 0 10 0
   5: 0100007F:0FA0 0100007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 6 1 0000000000000000 100 0 0 10 0
`
	// LISTEN on [::1]:5173, [::]:8080 (dup of the v4 one), [fe80::1]:6000.
	const tcp6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1435 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 7 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8 1 0000000000000000 100 0 0 10 0
   2: 000080FE000000000000000001000000:1770 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 9 1 0000000000000000 100 0 0 10 0
`
	got := listening(32768, strings.NewReader(tcp), strings.NewReader(tcp6))
	if want := []int{3000, 5173, 8080}; !reflect.DeepEqual(got, want) {
		t.Errorf("listening = %v, want %v", got, want)
	}
}

func TestEphemeralStart(t *testing.T) {
	if got := ephemeralStart("32768\t60999\n"); got != 32768 {
		t.Errorf("ephemeralStart = %d", got)
	}
	if got := ephemeralStart(""); got != 32768 {
		t.Errorf("ephemeralStart of nothing = %d, want Linux's default 32768", got)
	}
}
