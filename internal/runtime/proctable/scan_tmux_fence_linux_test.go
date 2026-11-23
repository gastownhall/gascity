//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// tmuxFenceFixture builds a fake /proc holding one host-sharing box and a tmux
// server an agent of that box founded, as gc sees them:
//
//	10 holder (the pack's process; no session)
//	└─ 11 sh            GC_SESSION_ID=sid-box   the box's program: a root
//	   ├─ 12 sleep      GC_SESSION_ID=sid-box   inside the program's envelope
//	   └─ 13 tmux: client GC_SESSION_ID=sid-box an attach run from the box
//	20 tmux: server     GC_SESSION_ID=sid-box   founded in the box, adopted by init
//	└─ 21 bash          GC_SESSION_ID=sid-box   a pane: a root for the tmux runtime
//	   └─ 22 vim        GC_SESSION_ID=sid-box
//	      └─ 23 agent   GC_SESSION_ID=sid-other a root of another session, in the pane
//	30 sleep            GC_SESSION_ID=sid-box   a leftover adopted by init
func tmuxFenceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFakeBootTime(t, root, time.Unix(1700000000, 0))
	box := map[string]string{"GC_SESSION_ID": "sid-box", "GC_CITY_PATH": "/city"}
	buildFakeProcUnder(t, root, 10, 1, "holder", nil)
	buildFakeProcUnder(t, root, 11, 10, "sh", box)
	buildFakeProcUnder(t, root, 12, 11, "sleep", box)
	buildFakeProcUnder(t, root, 13, 11, "tmux: client", box)
	buildFakeProcUnder(t, root, 20, 1, "tmux: server", box)
	buildFakeProcUnder(t, root, 21, 20, "bash", box)
	buildFakeProcUnder(t, root, 22, 21, "vim", box)
	buildFakeProcUnder(t, root, 23, 22, "agent", map[string]string{"GC_SESSION_ID": "sid-other", "GC_CITY_PATH": "/city"})
	buildFakeProcUnder(t, root, 30, 1, "sleep", box)
	return root
}

// Kills a host-sharing scan that reports a process under a tmux server as a
// session root: a tmux server an agent founds in its box carries the
// session's GC_SESSION_ID, so its pane reads as the session's root, and a
// start's orphan kill would end the pane's process group. The scan outside
// tmux reports the box's program and its adopted leftover, and nothing in
// the tmux server's subtree, however deep, nor the server or a client.
func TestScanBySessionIDOutsideTmuxFencesTmuxSubtrees(t *testing.T) {
	root := tmuxFenceFixture(t)
	SetScanRootForTesting(t, root)
	savedDetect := detectSubreaper
	detectSubreaper = func(int) int { return 0 }
	t.Cleanup(func() { detectSubreaper = savedDetect })

	for _, tc := range []struct {
		id   string
		want []int
	}{
		{"sid-box", []int{11, 30}},
		{"sid-other", []int{}},
		// A regression guard only: the sweep's scan reads orphan candidates,
		// so no process under the server is one even unfenced.
		{"", []int{30}},
	} {
		found, err := ScanBySessionIDOutsideTmux(tc.id)
		if err != nil {
			t.Fatalf("ScanBySessionIDOutsideTmux(%q): %v", tc.id, err)
		}
		if got := rootPIDs(found); !slices.Equal(got, tc.want) {
			t.Errorf("ScanBySessionIDOutsideTmux(%q) roots = %v, want %v: nothing under a tmux server is a host-sharing session's root", tc.id, got, tc.want)
		}
	}
}

// Pins the tmux runtime's scan unchanged by the fence: the process a tmux
// server runs in a pane carrying the server's GC_SESSION_ID is that
// session's root, with its parent attributed to provider infrastructure, so
// the tmux runtime's tracking, orphan kill and drain-ack escalation still see
// their panes.
func TestScanBySessionIDStillReportsTmuxPanes(t *testing.T) {
	root := tmuxFenceFixture(t)
	SetScanRootForTesting(t, root)
	found, err := ScanBySessionID("sid-box")
	if err != nil {
		t.Fatalf("ScanBySessionID: %v", err)
	}
	if got := rootPIDs(found); !slices.Equal(got, []int{11, 21, 30}) {
		t.Fatalf("ScanBySessionID roots = %v, want [11 21 30]: the pane is the tmux runtime's root", got)
	}
	if !found[1].ParentIsProviderInfrastructure {
		t.Fatalf("the pane root %+v lost its tmux parent attribution", found[1])
	}
	other, err := ScanBySessionID("sid-other")
	if err != nil || !slices.Equal(rootPIDs(other), []int{23}) {
		t.Fatalf("ScanBySessionID(sid-other) = %+v, %v; want the root inside the pane", other, err)
	}
}

// Kills a fence that reads an undecidable ancestry as "no tmux": a root whose
// ancestor vanished mid-walk, or whose parent chain loops, may sit under a
// tmux server, so it is left out with an entry error (that process alone,
// never a whole-scan failure), while a decidable root is still reported.
func TestScanBySessionIDOutsideTmuxLeavesOutUndecidableAncestry(t *testing.T) {
	root := t.TempDir()
	writeFakeBootTime(t, root, time.Unix(1700000000, 0))
	env := map[string]string{"GC_SESSION_ID": "sid-box"}
	buildFakeProcUnder(t, root, 41, 40, "agent", env) // its parent 40 is gone
	buildFakeProcUnder(t, root, 50, 51, "agent", env) // 50 and 51 parent each other
	buildFakeProcUnder(t, root, 51, 50, "loop", nil)
	buildFakeProcUnder(t, root, 60, 1, "agent", env)
	if err := os.WriteFile(filepath.Join(root, "52"), nil, 0o644); err != nil { // a non-process entry is skipped
		t.Fatal(err)
	}
	SetScanRootForTesting(t, root)

	found, err := ScanBySessionIDOutsideTmux("sid-box")
	if got := rootPIDs(found); !slices.Equal(got, []int{60}) {
		t.Fatalf("roots = %v, want only [60]: an undecidable ancestry is no host-sharing root", got)
	}
	if pids := entryErrorPIDs(err); !slices.Equal(pids, []int{41, 50}) || HasWholeScanFailure(err) {
		t.Fatalf("err = %v (entry pids %v), want entry errors for 41 and 50 alone", err, pids)
	}
}

// Kills a fence broader than the hazard: only a tmux that carries a
// GC_SESSION_ID was founded inside a session, so only its panes read as that
// session's roots. A box whose holder runs under a pack's own multiplexer,
// started without session identity, and an agent of a gc that runs in a
// user's tmux pane are roots like any other, whatever the tmux is named.
func TestScanBySessionIDOutsideTmuxReportsRootsUnderASessionlessTmux(t *testing.T) {
	root := t.TempDir()
	writeFakeBootTime(t, root, time.Unix(1700000000, 0))
	buildFakeProcUnder(t, root, 100, 1, "tmux: server", map[string]string{"PATH": "/usr/bin"})
	buildFakeProcUnder(t, root, 101, 100, "holder", nil)
	buildFakeProcUnder(t, root, 102, 101, "sh", map[string]string{"GC_SESSION_ID": "sid-box"})
	buildFakeProcUnder(t, root, 110, 1, "tmux3.4:", map[string]string{"HOME": "/home/user"})
	buildFakeProcUnder(t, root, 111, 110, "bash", nil)
	buildFakeProcUnder(t, root, 112, 111, "gc", nil)
	buildFakeProcUnder(t, root, 113, 112, "agent", map[string]string{"GC_SESSION_ID": "sid-acp"})
	SetScanRootForTesting(t, root)
	for _, tc := range []struct {
		id   string
		want []int
	}{
		{"sid-box", []int{102}},
		{"sid-acp", []int{113}},
	} {
		found, err := ScanBySessionIDOutsideTmux(tc.id)
		if err != nil {
			t.Fatalf("ScanBySessionIDOutsideTmux(%q): %v", tc.id, err)
		}
		if got := rootPIDs(found); !slices.Equal(got, tc.want) {
			t.Errorf("ScanBySessionIDOutsideTmux(%q) = %v, want %v: a tmux that carries no session's id hosts no session's pane", tc.id, got, tc.want)
		}
	}
}

// Kills a fence that reads a tmux whose environment it cannot read as one
// without a session: another user's tmux server may carry a session's
// GC_SESSION_ID, so a root under it is left out with an entry error naming
// that root, never a whole-scan failure.
func TestScanBySessionIDOutsideTmuxFailsClosedOnAnUnreadableTmuxEnvironment(t *testing.T) {
	root := t.TempDir()
	writeFakeBootTime(t, root, time.Unix(1700000000, 0))
	box := map[string]string{"GC_SESSION_ID": "sid-box"}
	buildFakeProcUnder(t, root, 20, 1, "tmux: server", box)
	// Another user's server: the scan never reads its environment, and the
	// fence cannot. A directory stands in for the unreadable file, which a
	// test running as root would read anyway.
	writeFakeProcessUID(t, filepath.Join(root, "20"), os.Geteuid()+1)
	environ := filepath.Join(root, "20", "environ")
	if err := os.Remove(environ); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(environ, 0o755); err != nil {
		t.Fatal(err)
	}
	buildFakeProcUnder(t, root, 21, 20, "bash", nil)
	buildFakeProcUnder(t, root, 22, 21, "agent", box)
	buildFakeProcUnder(t, root, 30, 1, "agent", box)
	SetScanRootForTesting(t, root)

	found, err := ScanBySessionIDOutsideTmux("sid-box")
	if got := rootPIDs(found); !slices.Equal(got, []int{30}) {
		t.Fatalf("roots = %v, want only [30]: a root under a tmux of unreadable environment is undecided", got)
	}
	if pids := entryErrorPIDs(err); !slices.Equal(pids, []int{22}) || HasWholeScanFailure(err) {
		t.Fatalf("err = %v (entry pids %v), want an entry error for 22 alone", err, pids)
	}
}

// A root that exits between the scan and the fence is left out without an
// error, as the scan skips a process that dies under it; the drain-ack
// escalation scans just after a stop, while a seat's processes exit. A root
// whose ancestor vanishes mid-walk still gets an entry error: it may have sat
// under a session's tmux.
func TestFenceTmuxSubtreesSkipsARootThatExited(t *testing.T) {
	root := t.TempDir()
	buildFakeProcUnder(t, root, 41, 40, "agent", map[string]string{"GC_SESSION_ID": "sid-box"})
	buildFakeProcUnder(t, root, 60, 1, "agent", map[string]string{"GC_SESSION_ID": "sid-box"})
	kept, err := fenceTmuxSubtrees(root, []runtime.LiveRuntime{{PID: 41}, {PID: 60}, {PID: 99}})
	if got := rootPIDs(kept); !slices.Equal(got, []int{60}) {
		t.Fatalf("kept = %v, want [60]", got)
	}
	if pids := entryErrorPIDs(err); !slices.Equal(pids, []int{41}) {
		t.Fatalf("err = %v (entry pids %v), want an entry error for 41 alone: root 99 exited, ancestor 40 of 41 vanished", err, pids)
	}
}

// Kills a fence that recognizes tmux by the name "tmux" alone: tmux names
// its processes after argv[0] ("<name>: server", "<name>: client"), so a
// tmux run as tmux3 founds a "tmux3: server". The fenced scan reports
// neither such a server, which carries the session's id and reads as a root
// adopted by init, nor any process in its panes.
func TestScanBySessionIDOutsideTmuxFencesARenamedTmux(t *testing.T) {
	root := t.TempDir()
	writeFakeBootTime(t, root, time.Unix(1700000000, 0))
	box := map[string]string{"GC_SESSION_ID": "sid-box"}
	buildFakeProcUnder(t, root, 70, 1, "tmux3: server", box)
	buildFakeProcUnder(t, root, 71, 70, "bash", box)
	buildFakeProcUnder(t, root, 72, 1, "sleep", box)
	buildFakeProcUnder(t, root, 73, 72, "tmux3: client", box)
	buildFakeProcUnder(t, root, 74, 73, "agent", map[string]string{"GC_SESSION_ID": "sid-other"})
	SetScanRootForTesting(t, root)
	if all, err := ScanBySessionID("sid-box"); err != nil || !slices.Contains(rootPIDs(all), 70) {
		t.Fatalf("ScanBySessionID = %v, %v; want the renamed server among its roots, or the case proves nothing", rootPIDs(all), err)
	}
	for _, tc := range []struct {
		id   string
		want []int
	}{
		{"sid-box", []int{72}},
		{"sid-other", []int{}},
	} {
		found, err := ScanBySessionIDOutsideTmux(tc.id)
		if err != nil {
			t.Fatalf("ScanBySessionIDOutsideTmux(%q): %v", tc.id, err)
		}
		if got := rootPIDs(found); !slices.Equal(got, tc.want) {
			t.Errorf("ScanBySessionIDOutsideTmux(%q) = %v, want %v: a renamed tmux server, its panes and its clients' children are fenced", tc.id, got, tc.want)
		}
	}
}

// On Linux tmux titles itself "<name>: server (<socket>)" and, when that
// overruns comm's 15 bytes, cuts it at its last space, so a tmux run under
// a name of 7 or more characters (tmux3.4) has the comm "<name>:" for both
// its server and its clients. The fence recognizes that form too.
func TestScanBySessionIDOutsideTmuxFencesARenamedTmuxWithATruncatedTitle(t *testing.T) {
	root := t.TempDir()
	writeFakeBootTime(t, root, time.Unix(1700000000, 0))
	box := map[string]string{"GC_SESSION_ID": "sid-box"}
	buildFakeProcUnder(t, root, 70, 1, "tmux3.4:", box)
	buildFakeProcUnder(t, root, 71, 70, "bash", box)
	buildFakeProcUnder(t, root, 72, 1, "sleep", box)
	buildFakeProcUnder(t, root, 73, 72, "tmux3.4:", box)
	buildFakeProcUnder(t, root, 74, 73, "agent", map[string]string{"GC_SESSION_ID": "sid-other"})
	SetScanRootForTesting(t, root)
	if all, err := ScanBySessionID("sid-box"); err != nil || !slices.Contains(rootPIDs(all), 70) {
		t.Fatalf("ScanBySessionID = %v, %v; want the renamed server among its roots, or the case proves nothing", rootPIDs(all), err)
	}
	for _, tc := range []struct {
		id   string
		want []int
	}{
		{"sid-box", []int{72}},
		{"sid-other", []int{}},
	} {
		found, err := ScanBySessionIDOutsideTmux(tc.id)
		if err != nil {
			t.Fatalf("ScanBySessionIDOutsideTmux(%q): %v", tc.id, err)
		}
		if got := rootPIDs(found); !slices.Equal(got, tc.want) {
			t.Errorf("ScanBySessionIDOutsideTmux(%q) = %v, want %v: a tmux whose title was cut to \"<name>:\", its panes and its clients' children are fenced", tc.id, got, tc.want)
		}
	}
}
