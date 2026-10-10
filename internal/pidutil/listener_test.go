package pidutil

import (
	"reflect"
	"strings"
	"testing"
)

// procNetTCPFixture is a /proc/net/tcp table as the kernel prints it: a header,
// two listeners (127.0.0.1:3307 and 0.0.0.0:22), an ESTABLISHED connection
// whose local port is also 3307, a TIME_WAIT row with inode 0, and a truncated
// row.
const procNetTCPFixture = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0CEB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242001 1 0000000000000000 100 0 0 10 0
   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 17 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0CEB 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 4242002 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:0CEB 0100007F:D432 06 00000000:00000000 03:00000F9E 00000000     0        0 0 3 0000000000000000
   4: 0100007F:0CEB 00000000:0000 0A
`

// procNetTCP6Fixture is a /proc/net/tcp6 table with a [::]:3307 listener and a
// row whose port is not hex.
const procNetTCP6Fixture = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0CEB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242003 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:ZZZZ 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4242004 1 0000000000000000 100 0 0 10 0
`

type listenRow struct {
	Port  int
	Inode string
}

func parseListenFixture(t *testing.T, table string) []listenRow {
	t.Helper()
	var rows []listenRow
	if err := parseProcNetTCPListeners(strings.NewReader(table), func(port int, inode string) {
		rows = append(rows, listenRow{Port: port, Inode: inode})
	}); err != nil {
		t.Fatalf("parseProcNetTCPListeners: %v", err)
	}
	return rows
}

func TestParseProcNetTCPListenersIPv4(t *testing.T) {
	got := parseListenFixture(t, procNetTCPFixture)
	want := []listenRow{{Port: 3307, Inode: "4242001"}, {Port: 22, Inode: "17"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listeners = %+v, want %+v (only LISTEN rows with an inode)", got, want)
	}
}

func TestParseProcNetTCPListenersIPv6(t *testing.T) {
	got := parseListenFixture(t, procNetTCP6Fixture)
	want := []listenRow{{Port: 3307, Inode: "4242003"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listeners = %+v, want %+v", got, want)
	}
}

func TestParseProcNetTCPListenersEmpty(t *testing.T) {
	for _, table := range []string{"", "  sl  local_address rem_address   st\n"} {
		if got := parseListenFixture(t, table); len(got) != 0 {
			t.Fatalf("listeners for %q = %+v, want none", table, got)
		}
	}
}
