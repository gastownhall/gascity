package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadRigDoltPorts_ReadsAllRigs(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")
	fs.Files["/rig-a/.beads/dolt-server.port"] = []byte("28232\n")
	fs.Files["/rig-b/.beads/dolt-server.port"] = []byte("28233\n")

	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "alpha", Path: "/rig-a"},
		{Name: "beta", Path: "/rig-b"},
	}

	got := loadRigDoltPorts(rigs, fs)
	want := map[int]string{
		28231: "hq",
		28232: "alpha",
		28233: "beta",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadRigDoltPorts = %v, want %v", got, want)
	}
}

func TestLoadRigDoltPorts_SkipsMissingAndMalformed(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/rig-a/.beads/dolt-server.port"] = []byte("28232\n")
	fs.Files["/rig-b/.beads/dolt-server.port"] = []byte("not-a-port\n")
	fs.Files["/rig-c/.beads/dolt-server.port"] = []byte("\n")
	// /rig-d has no port file at all.

	rigs := []resolverRig{
		{Name: "alpha", Path: "/rig-a"},
		{Name: "beta", Path: "/rig-b"},
		{Name: "gamma", Path: "/rig-c"},
		{Name: "delta", Path: "/rig-d"},
	}

	got := loadRigDoltPorts(rigs, fs)
	want := map[int]string{
		28232: "alpha",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadRigDoltPorts = %v, want %v", got, want)
	}
}

func TestLoadRigDoltPorts_DuplicatePortsLastWins(t *testing.T) {
	// Pathological: two rigs claim the same port. Last write wins so the
	// reaper still protects on port match (it just attributes to the
	// later-listed rig). Acceptable behavior; documented in the function.
	fs := fsys.NewFake()
	fs.Files["/rig-a/.beads/dolt-server.port"] = []byte("28232\n")
	fs.Files["/rig-b/.beads/dolt-server.port"] = []byte("28232\n")

	rigs := []resolverRig{
		{Name: "alpha", Path: "/rig-a"},
		{Name: "beta", Path: "/rig-b"},
	}

	got := loadRigDoltPorts(rigs, fs)
	if got[28232] == "" {
		t.Errorf("expected port 28232 to be in map, got %v", got)
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
	line := "  78306  65392 Sun May 17 09:31:24 2026 /usr/local/bin/dolt sql-server --config /tmp/TestGcBeadsBdStartUsesRootBeadsDataDir802378814/001/.gc/runtime/packs/dolt/dolt-config.yaml --host 127.0.0.1"
	got, ok := parseDoltPSLine(line, map[int][]int{78306: {3306}})
	if !ok {
		t.Fatal("parseDoltPSLine did not recognize dolt sql-server")
	}
	if got.PID != 78306 {
		t.Fatalf("PID = %d, want 78306", got.PID)
	}
	if got.RSSBytes != 65392*1024 {
		t.Fatalf("RSSBytes = %d, want %d", got.RSSBytes, int64(65392*1024))
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
	line := "12345 1024 Sun May 17 09:31:24 2026 dolt sql-server --config /tmp/Test With Space/config.yaml --port 3306"
	got, ok := parseDoltPSLine(line, nil)
	if !ok {
		t.Fatal("parseDoltPSLine did not recognize dolt sql-server")
	}
	if cfg := extractConfigPath(got.Argv); cfg != "/tmp/Test With Space/config.yaml" {
		t.Fatalf("config = %q", cfg)
	}
}

func TestParseDoltPSLine_IgnoresNonDolt(t *testing.T) {
	line := "12345 1024 Sun May 17 09:31:24 2026 mysqld --config /tmp/TestX/config.yaml"
	if got, ok := parseDoltPSLine(line, nil); ok {
		t.Fatalf("parseDoltPSLine = %+v, want ignored", got)
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
	cases := []struct {
		link string
		want string
	}{
		{"/data/worktrees/live", procPathStateLive},
		{"/data/worktrees/gone (deleted)", procPathStateDeleted},
		{"/data/worktrees/dir with spaces", procPathStateLive},
		// "(deleted)" must be a readlink suffix, not an arbitrary substring.
		{"/data/worktrees/x (deleted) suffix", procPathStateLive},
	}
	for _, tc := range cases {
		if got := cwdStateFromLink(tc.link); got != tc.want {
			t.Errorf("cwdStateFromLink(%q) = %q, want %q", tc.link, got, tc.want)
		}
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
