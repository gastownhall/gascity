package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDoltProcRigOwner_MatchesDataDirUnderRigRoot(t *testing.T) {
	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "alpha", Path: "/rig-a"},
	}

	proc := DoltProcInfo{Argv: []string{"dolt", "sql-server", "--data-dir", "/rig-a/.beads/dolt"}}
	owner, ok := doltProcRigOwner(proc, rigs)
	if !ok || owner != "alpha" {
		t.Errorf("doltProcRigOwner = (%q, %v), want (alpha, true)", owner, ok)
	}
}

func TestDoltProcRigOwner_MatchesConfigUnderRigRoot(t *testing.T) {
	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}

	proc := DoltProcInfo{Argv: []string{"dolt", "sql-server", "--config", "/city/.gc/runtime/packs/dolt/config.yaml"}}
	owner, ok := doltProcRigOwner(proc, rigs)
	if !ok || owner != "hq" {
		t.Errorf("doltProcRigOwner = (%q, %v), want (hq, true)", owner, ok)
	}
}

func TestDoltProcRigOwner_IgnoresForeignAndPathlessProcesses(t *testing.T) {
	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "alpha", Path: "/rig-a"},
	}

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"foreign data dir", []string{"dolt", "sql-server", "--data-dir", "/elsewhere/dolt"}},
		{"foreign config", []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"}},
		{"no path flags", []string{"dolt", "sql-server"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if owner, ok := doltProcRigOwner(DoltProcInfo{Argv: tc.argv}, rigs); ok {
				t.Errorf("doltProcRigOwner = (%q, true), want no owner", owner)
			}
		})
	}
}

func TestDoltProcRigOwner_FirstListedRigWinsOnNestedRoots(t *testing.T) {
	// Pathological: nested rig roots both contain the candidate path. The
	// first-listed rig wins; the reaper is still safe because any match
	// protects regardless of attribution.
	rigs := []resolverRig{
		{Name: "outer", Path: "/work"},
		{Name: "inner", Path: "/work/rig-a"},
	}

	proc := DoltProcInfo{Argv: []string{"dolt", "sql-server", "--data-dir", "/work/rig-a/.beads/dolt"}}
	owner, ok := doltProcRigOwner(proc, rigs)
	if !ok || owner != "outer" {
		t.Errorf("doltProcRigOwner = (%q, %v), want (outer, true)", owner, ok)
	}
}

func TestPathUnderRoot(t *testing.T) {
	cases := []struct {
		name string
		path string
		root string
		want bool
	}{
		{"direct child", "/city/.beads/dolt", "/city", true},
		{"equal", "/city", "/city", true},
		{"sibling prefix is not containment", "/cityscape/.beads", "/city", false},
		{"outside", "/elsewhere/dolt", "/city", false},
		{"empty path", "", "/city", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathUnderRoot(tc.path, normalizePathForCompare(tc.root)); got != tc.want {
				t.Errorf("pathUnderRoot(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
			}
		})
	}
}

func TestSplitCmdline_NULSeparatedWithTrailingNUL(t *testing.T) {
	// /proc/<pid>/cmdline format: NUL-separated argv, trailing NUL.
	in := []byte("dolt\x00sql-server\x00--config\x00/tmp/TestFoo/config.yaml\x00")
	got := splitCmdline(in)
	want := []string{"dolt", "sql-server", "--config", "/tmp/TestFoo/config.yaml"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d, got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitCmdline_Empty(t *testing.T) {
	if got := splitCmdline(nil); len(got) != 0 {
		t.Errorf("splitCmdline(nil) = %v, want empty", got)
	}
	if got := splitCmdline([]byte{}); len(got) != 0 {
		t.Errorf("splitCmdline([]) = %v, want empty", got)
	}
}

func TestParseProcStartTimeTicks(t *testing.T) {
	fieldsAfterComm := []string{
		"S", "1", "2", "3", "4", "5", "6", "7", "8", "9",
		"10", "11", "12", "13", "14", "15", "16", "17", "18", "98765",
	}
	line := "123 (dolt sql server) " + strings.Join(fieldsAfterComm, " ")

	if got := parseProcStartTimeTicks([]byte(line)); got != 98765 {
		t.Fatalf("parseProcStartTimeTicks = %d, want 98765", got)
	}
}

func TestLooksLikeDoltSQLServer(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"absolute dolt path", []string{"/usr/local/bin/dolt", "sql-server"}, true},
		{"bare dolt", []string{"dolt", "sql-server", "--config", "x"}, true},
		{"non-dolt", []string{"mysqld", "sql-server"}, false},
		{"dolt without sql-server", []string{"dolt", "version"}, false},
		{"too short", []string{"dolt"}, false},
		{"empty", []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeDoltSQLServer(tc.argv); got != tc.want {
				t.Errorf("looksLikeDoltSQLServer(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestParseDoltPSLine_DoltSQLServer(t *testing.T) {
	// No rss= token: gastownhall/gascity#5201 dropped it from the requested ps
	// fields, since some macOS hosts reject the whole ps invocation when it
	// asks for resource-usage fields on other sessions' processes.
	line := "  78306  Sun May 17 09:31:24 2026 /usr/local/bin/dolt sql-server --config /tmp/TestGcBeadsBdStartUsesRootBeadsDataDir802378814/001/.gc/runtime/packs/dolt/dolt-config.yaml --host 127.0.0.1"
	got, ok := parseDoltPSLine(line, map[int][]int{78306: {3306}})
	if !ok {
		t.Fatal("parseDoltPSLine did not recognize dolt sql-server")
	}
	if got.PID != 78306 {
		t.Fatalf("PID = %d, want 78306", got.PID)
	}
	if got.RSSBytes != 0 {
		t.Fatalf("RSSBytes = %d, want 0 (rss= is no longer requested from ps, gastownhall/gascity#5201)", got.RSSBytes)
	}
	if !reflect.DeepEqual(got.Ports, []int{3306}) {
		t.Fatalf("Ports = %v, want [3306]", got.Ports)
	}
	if got.StartIdentity != "Sun May 17 09:31:24 2026" {
		t.Fatalf("StartIdentity = %q", got.StartIdentity)
	}
	if cfg := extractConfigPath(got.Argv); cfg != "/tmp/TestGcBeadsBdStartUsesRootBeadsDataDir802378814/001/.gc/runtime/packs/dolt/dolt-config.yaml" {
		t.Fatalf("config = %q", cfg)
	}
}

func TestParseDoltPSLine_PreservesSpacedConfigPath(t *testing.T) {
	line := "12345 Sun May 17 09:31:24 2026 dolt sql-server --config /tmp/Test With Space/config.yaml --port 3306"
	got, ok := parseDoltPSLine(line, nil)
	if !ok {
		t.Fatal("parseDoltPSLine did not recognize dolt sql-server")
	}
	if cfg := extractConfigPath(got.Argv); cfg != "/tmp/Test With Space/config.yaml" {
		t.Fatalf("config = %q", cfg)
	}
}

func TestParseDoltPSLine_IgnoresNonDolt(t *testing.T) {
	line := "12345 Sun May 17 09:31:24 2026 mysqld --config /tmp/TestX/config.yaml"
	if got, ok := parseDoltPSLine(line, nil); ok {
		t.Fatalf("parseDoltPSLine = %+v, want ignored", got)
	}
}

// TestPSOutputFormatExcludesRSS pins gastownhall/gascity#5201: some macOS
// hosts require an entitlement to report resource-usage fields (%mem/vsz/
// rss/time) for processes outside the caller's own session, and `ps` exits
// non-zero for the *entire* invocation when it can't — turning a clean,
// zero-orphan scan into a reported dolt-cleanup reap-stage error. RSSBytes
// is cosmetic-only downstream (planOrphanReap classifies purely on
// ConfigPath/DataDir/CWDState, never on RSS), so requesting rss= here is not
// worth trading away the whole scan for.
func TestPSOutputFormatExcludesRSS(t *testing.T) {
	if strings.Contains(psOutputFormat, "rss=") {
		t.Fatalf("psOutputFormat = %q, must not request rss= (gastownhall/gascity#5201)", psOutputFormat)
	}
}

func TestParseListeningPortsByPIDFromLsof(t *testing.T) {
	output := `COMMAND   PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
dolt    78306 dbox   11u  IPv4 0x0000000000000000      0t0  TCP 127.0.0.1:3306 (LISTEN)
dolt    78306 dbox   12u  IPv6 0x0000000000000000      0t0  TCP [::1]:3307 (LISTEN)
dolt    99999 dbox   12u  IPv4 0x0000000000000000      0t0  TCP 127.0.0.1:70000 (LISTEN)
`
	got := parseListeningPortsByPIDFromLsof(output)
	want := map[int][]int{78306: {3306, 3307}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseListeningPortsByPIDFromLsof = %v, want %v", got, want)
	}
}

func TestSameReapProcessIdentity_UsesPSStartIdentityFallback(t *testing.T) {
	target := ReapTarget{PID: 42, StartIdentity: "Sun May 17 09:31:24 2026"}
	same := DoltProcInfo{PID: 42, StartIdentity: "Sun May 17 09:31:24 2026"}
	reused := DoltProcInfo{PID: 42, StartIdentity: "Sun May 17 09:32:00 2026"}

	if !sameReapProcessIdentity(target, same) {
		t.Fatal("sameReapProcessIdentity should accept matching ps start identity")
	}
	if sameReapProcessIdentity(target, reused) {
		t.Fatal("sameReapProcessIdentity should reject mismatched ps start identity")
	}
}

func TestCWDStateFromLink(t *testing.T) {
	// The non-suffix cases never stat, so an arbitrary cwdLink is fine.
	cases := []struct {
		name string
		link string
		want string
	}{
		{"plain live path", "/data/worktrees/live", procPathStateLive},
		{"path with spaces", "/data/worktrees/dir with spaces", procPathStateLive},
		// "(deleted)" must terminate the readlink, not appear mid-string.
		{"deleted mid-string stays live", "/data/worktrees/x (deleted) suffix", procPathStateLive},
		// A genuinely unlinked cwd: the literal "<path> (deleted)" does not
		// exist on disk, so the inode comparison fails closed to deleted.
		{"unlinked inode is deleted", "/data/worktrees/gone (deleted)", procPathStateDeleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cwdStateFromLink(tc.link, "/proc/self/cwd"); got != tc.want {
				t.Errorf("cwdStateFromLink(%q) = %q, want %q", tc.link, got, tc.want)
			}
		})
	}
}

func TestCWDStateFromLink_LiveDirNamedDeletedStaysLive(t *testing.T) {
	// Pathological but real: a live directory whose name literally ends in
	// " (deleted)". The readlink target is identical to the kernel's unlinked
	// marker, so only an inode comparison can tell them apart. When the literal
	// path resolves to the same inode as the procfs cwd link, it must classify
	// live and never be reaped.
	dir := filepath.Join(t.TempDir(), "scope (deleted)")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// cwdLink == the literal path here: both stat the same live inode, the way
	// /proc/<pid>/cwd would resolve for a process whose cwd is this directory.
	if got := cwdStateFromLink(dir, dir); got != procPathStateLive {
		t.Errorf("cwdStateFromLink(%q) = %q, want live (real dir named '... (deleted)')", dir, got)
	}
}

func TestCWDStateFromLink_NonDefinitiveStatErrorIsUnknown(t *testing.T) {
	// A stat failure that is NOT a definitive not-exist (here EACCES from an
	// unsearchable parent, standing in for permission, I/O, or hung-mount
	// errors) must fail closed to unknown/protect, never deleted/reap. The old
	// sameFile collapsed every stat error to "not the same file" and reaped;
	// destructive force-mode cleanup must not act on ambiguous evidence.
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	parent := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	// Restore search permission before t.TempDir's RemoveAll cleanup runs
	// (Cleanup is LIFO, so this registers after — and runs before — it).
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	// Stat of "<parent>/scope (deleted)" fails with EACCES because searching the
	// 0o000 parent is denied; EACCES is not os.IsNotExist, so it must be unknown.
	link := filepath.Join(parent, "scope (deleted)")
	if got := cwdStateFromLink(link, link); got != procPathStateUnknown {
		t.Errorf("cwdStateFromLink(%q) under EACCES = %q, want unknown (fail closed, not deleted)", link, got)
	}
}

func TestDoltProcCWDState_SelfIsLive(t *testing.T) {
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("host has no /proc; cwd state degrades to unknown by design")
	}
	if got := doltProcCWDState(os.Getpid()); got != procPathStateLive {
		t.Errorf("doltProcCWDState(self) = %q, want %q", got, procPathStateLive)
	}
}

func TestDoltProcCWDState_UnknownForBadPID(t *testing.T) {
	// PID 0 has no /proc entry anywhere; the helper must degrade to unknown
	// (the classification's protect-leaning default), never guess.
	if got := doltProcCWDState(0); got != procPathStateUnknown {
		t.Errorf("doltProcCWDState(0) = %q, want unknown", got)
	}
}

func TestDoltConfigPathState(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "dolt-config.yaml")
	if err := os.WriteFile(existing, []byte("listener:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "removed", "dolt-config.yaml")

	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"existing absolute config", []string{"dolt", "sql-server", "--config", existing}, procPathStateLive},
		{"missing absolute config", []string{"dolt", "sql-server", "--config", missing}, procPathStateDeleted},
		{"relative config is unknown", []string{"dolt", "sql-server", "--config", "dolt-config.yaml"}, procPathStateUnknown},
		{"no config flag is unknown", []string{"dolt", "sql-server", "-H", "127.0.0.1"}, procPathStateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doltConfigPathState(tc.argv); got != tc.want {
				t.Errorf("doltConfigPathState(%v) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}
