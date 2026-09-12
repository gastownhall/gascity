//go:build darwin

package proctable

import "testing"

// A tmux server founded by a session's first new-session call inherits that
// session's GC_SESSION_ID and reparents to launchd. The parent-envelope test
// alone therefore reports it as an agent root, and the orphan sweep kills the
// one server every agent in the city shares (gastownhall/gascity#5392).
//
// psRecord.command is the first whitespace token of ps's command column, so a
// real "tmux: server" proctitle reaches these tests as "tmux:"; both spellings
// are infrastructure, and fixtures below should stay honest about that.
func TestScanRecordsBySessionIDNeverReportsInfrastructureAsRoot(t *testing.T) {
	records := map[int]psRecord{
		100: {pid: 100, ppid: 1, command: "tmux: server", env: map[string]string{"GC_SESSION_ID": "hq-session"}},
		101: {pid: 101, ppid: 100, command: "claude", env: map[string]string{"GC_SESSION_ID": "hq-session"}},
	}
	got := scanRecordsBySessionID(records, "hq-session")
	if len(got) != 1 || got[0].PID != 101 {
		t.Fatalf("scanRecordsBySessionID = %+v, want only the agent pid 101", got)
	}
}

func TestIsRecordScanRootRefusesInfrastructure(t *testing.T) {
	records := map[int]psRecord{
		100: {pid: 100, ppid: 1, command: "tmux: server", env: map[string]string{"GC_SESSION_ID": "hq-session"}},
		101: {pid: 101, ppid: 100, command: "claude", env: map[string]string{"GC_SESSION_ID": "hq-session"}},
	}
	if isRecordScanRoot(records, records[100]) {
		t.Fatal("the tmux server was classified as an agent root")
	}
	if !isRecordScanRoot(records, records[101]) {
		t.Fatal("the agent under an infrastructure parent must remain a root")
	}
}

// Infrastructure is matched by exact name: a tmux-* wrapper running as an
// agent root is reported, where a substring test on "tmux" would hide it — and
// a root the scan hides is a runtime the orphan sweep can never reap.
func TestScanRecordsBySessionIDReportsTmuxWrapperAsRoot(t *testing.T) {
	records := map[int]psRecord{
		200: {pid: 200, ppid: 1, command: "/usr/local/bin/tmux-wrapper", env: map[string]string{"GC_SESSION_ID": "hq-session"}},
	}
	got := scanRecordsBySessionID(records, "hq-session")
	if len(got) != 1 || got[0].PID != 200 {
		t.Fatalf("scanRecordsBySessionID = %+v, want the tmux-wrapper agent pid 200 reported as a root", got)
	}
	if !isRecordScanRoot(records, records[200]) {
		t.Fatal("isRecordScanRoot refused the tmux-wrapper agent as a root")
	}
}
