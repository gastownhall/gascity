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

// TestDarwinOrphanCandidatesKeepOnlyReparentedRoots (mc-ygved): the
// empty-id scan keeps only roots reparented to launchd, as on Linux.
func TestDarwinOrphanCandidatesKeepOnlyReparentedRoots(t *testing.T) {
	records := map[int]psRecord{
		100: {pid: 100, ppid: 1, command: "agent", env: map[string]string{"GC_SESSION_ID": "gc-orphan"}},
		300: {pid: 300, ppid: 1, command: "bash"},
		102: {pid: 102, ppid: 300, command: "agent", env: map[string]string{"GC_SESSION_ID": "gc-paned"}},
	}
	got := orphanCandidates(scanRecordsBySessionID(records, ""), 0)
	if len(got) != 1 || got[0].SessionID != "gc-orphan" {
		t.Fatalf("orphan candidates %+v, want gc-orphan only", got)
	}
}

// psSnapshot parses ps lines, as psRecords reads them, into records keyed by
// pid.
func psSnapshot(t *testing.T, lines ...string) map[int]psRecord {
	t.Helper()
	records := make(map[int]psRecord, len(lines))
	for _, line := range lines {
		record, ok := parsePSRecord(line)
		if !ok {
			t.Fatalf("parsePSRecord(%q) refused a well-formed line", line)
		}
		records[record.pid] = record
	}
	return records
}

// The relay is told from the tap its companion also runs by argv[1], the
// second word of ps's command column. A one-element argv's second word is
// its first environment entry, which no verb matches.
func TestParsePSRecordKeepsArgvOne(t *testing.T) {
	relay, ok := parsePSRecord("  401   400 /opt/gc/bin/gc-osc7501 relay -- /bin/zsh -c claude GC_SESSION_ID=ga-relayed")
	if !ok || relay.pid != 401 || relay.ppid != 400 || relay.command != "/opt/gc/bin/gc-osc7501" || relay.arg1 != "relay" ||
		relay.env["GC_SESSION_ID"] != "ga-relayed" {
		t.Fatalf("parsePSRecord(relay line) = %+v, %v; want pid 401, ppid 400, the companion path, arg1 relay and the session env", relay, ok)
	}
	program, ok := parsePSRecord("  402   401 claude GC_SESSION_ID=ga-relayed")
	if !ok || program.command != "claude" || program.arg1 != "GC_SESSION_ID=ga-relayed" {
		t.Fatalf("parsePSRecord(one-element argv) = %+v, %v; want command claude and the first environment entry as arg1", program, ok)
	}
	for _, line := range []string{"", "   ", "401 400", "pid 400 claude", "401 ppid claude"} {
		if record, ok := parsePSRecord(line); ok {
			t.Errorf("parsePSRecord(%q) = %+v, accepted a line that is not <pid> <ppid> <command>", line, record)
		}
	}
}

// The relay runs its program in a session and process group of its own, so
// a kill of the relay's group never reaches the program. The Darwin scan
// looks through the relay as the Linux scan does
// (TestScanWithRootLooksThroughProgramStatusRelay): the program is the root,
// attributed to the relay's tmux server exactly as the relay would have been.
func TestScanRecordsBySessionIDLooksThroughProgramStatusRelay(t *testing.T) {
	records := psSnapshot(t,
		"  400     1 tmux: server GC_SESSION_ID=ga-relayed",
		"  401   400 /opt/gc/bin/gc-osc7501 relay -- /bin/zsh -c claude GC_SESSION_ID=ga-relayed",
		"  402   401 claude GC_SESSION_ID=ga-relayed",
		"  403   402 node GC_SESSION_ID=ga-relayed", // the program's own child: inside its envelope
	)

	got := scanRecordsBySessionID(records, "ga-relayed")
	if len(got) != 1 || got[0].PID != 402 {
		t.Fatalf("scanRecordsBySessionID = %+v, want only the relay's program pid 402", got)
	}
	if got[0].PPID != 400 || !got[0].ParentIsProviderInfrastructure {
		t.Fatalf("relayed root PPID=%d ParentIsProviderInfrastructure=%v, want the relay's tmux server 400 and true",
			got[0].PPID, got[0].ParentIsProviderInfrastructure)
	}
	for pid, want := range map[int]bool{400: false, 401: false, 402: true, 403: false} {
		if got := isRecordScanRoot(records, records[pid]); got != want {
			t.Errorf("isRecordScanRoot(%d) = %v, want %v", pid, got, want)
		}
	}
}

// No tap is a root, as on Linux
// (TestScanWithRootNeverReportsProgramStatusTapAsRoot): a tap carries its tmux
// server's environment, which a server founded inside an agent session
// (#5392's topology) takes from that session, so a tap read as a root would be
// that session's to kill.
func TestScanRecordsBySessionIDNeverReportsProgramStatusTapAsRoot(t *testing.T) {
	records := psSnapshot(t,
		"    1     0 /sbin/launchd",
		"  700     1 tmux: server GC_SESSION_ID=ga-founder",
		"  701   700 claude GC_SESSION_ID=ga-founder", // the founding session's own pane program
		"  702   700 /opt/gc/bin/gc-osc7501 tap -t %3 -n rAnd0m -x /usr/local/bin/tmux -S /private/tmp/tmux-501/city GC_SESSION_ID=ga-founder",
		"  703     1 /opt/gc/bin/gc-osc7501 tap -t %4 -n 0ther -x /usr/local/bin/tmux -S /private/tmp/tmux-501/city GC_SESSION_ID=ga-founder",
	)

	got := scanRecordsBySessionID(records, "ga-founder")
	if len(got) != 1 || got[0].PID != 701 {
		t.Fatalf("scanRecordsBySessionID = %+v, want only the founding session's program pid 701", got)
	}
	if orphans := orphanCandidates(scanRecordsBySessionID(records, ""), 0); len(orphans) != 0 {
		t.Fatalf("orphan candidates %+v, want none: an orphaned tap is no session's runtime", orphans)
	}
	for pid, want := range map[int]bool{701: true, 702: false, 703: false} {
		if got := isRecordScanRoot(records, records[pid]); got != want {
			t.Errorf("isRecordScanRoot(%d) = %v, want %v", pid, got, want)
		}
	}
}

// An orphaned relay (its tmux server gone) leaves its program as the orphan
// the sweep must find: the program reads as reparented to launchd, as on
// Linux (TestOrphanCandidateScanLooksThroughOrphanedRelay).
func TestDarwinOrphanCandidatesLookThroughOrphanedRelay(t *testing.T) {
	records := psSnapshot(t,
		"    1     0 /sbin/launchd",
		"  501     1 /opt/gc/bin/gc-osc7501 relay -- /bin/zsh -c claude GC_SESSION_ID=ga-orphan-relay",
		"  502   501 claude GC_SESSION_ID=ga-orphan-relay",
		// A relay still under its tmux server is no orphan.
		"  600     1 tmux: server",
		"  601   600 /opt/gc/bin/gc-osc7501 relay -- /bin/zsh -c claude GC_SESSION_ID=ga-live-relay",
		"  602   601 claude GC_SESSION_ID=ga-live-relay",
	)

	got := orphanCandidates(scanRecordsBySessionID(records, ""), 0)
	if len(got) != 1 || got[0].PID != 502 || got[0].PPID != 1 || got[0].ParentIsProviderInfrastructure {
		t.Fatalf("orphan candidates %+v, want only the orphaned relay's program 502 reading as reparented to launchd", got)
	}
}
