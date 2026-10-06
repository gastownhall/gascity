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

// The drain-ack escalation's kill fence requires a POSITIVE attribution of the
// candidate's parent to provider infrastructure, rather than "this ppid is not
// a subreaper I recognize" (see the linux sibling for why). A platform whose
// scanner never set the field would answer false for the seat's own pane root
// and silently disable the escalation there, so the darwin scanner is pinned to
// report it too.
func TestScanRecordsBySessionIDReportsParentProviderInfrastructure(t *testing.T) {
	records := map[int]psRecord{
		100: {pid: 100, ppid: 1, command: "tmux:", env: map[string]string{"GC_SESSION_ID": "hq-session"}},
		// The seat's own pane root: still owned by the server.
		101: {pid: 101, ppid: 100, command: "claude", env: map[string]string{"GC_SESSION_ID": "hq-session"}},
		// A daemon that merely inherited the seat's environment and was adopted
		// by launchd after its spawner exited.
		102: {pid: 102, ppid: 300, command: "gc", env: map[string]string{"GC_SESSION_ID": "hq-session"}},
		300: {pid: 300, ppid: 1, command: "launchd", env: map[string]string{}},
	}

	got := scanRecordsBySessionID(records, "hq-session")

	parentIsInfra := make(map[int]bool, len(got))
	for _, live := range got {
		parentIsInfra[live.PID] = live.ParentIsProviderInfrastructure
	}
	if len(got) != 2 {
		t.Fatalf("scanRecordsBySessionID = %+v, want the pane root and the inherited-env daemon", got)
	}
	if !parentIsInfra[101] {
		t.Error("the pane root's parent is the tmux server, but the scan did not attribute it to provider infrastructure; " +
			"a kill fence requiring that attribution would refuse the seat's own runtime")
	}
	if parentIsInfra[102] {
		t.Error("attributed a launchd parent to provider infrastructure; that is the inherited-env orphan shape the fence exists to refuse")
	}
}
