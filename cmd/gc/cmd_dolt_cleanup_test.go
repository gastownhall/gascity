package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCleanupReportJSONShape(t *testing.T) {
	r := CleanupReport{
		Schema: "gc.dolt.cleanup.v1",
		Port: CleanupPortReport{
			Resolved: 28231,
			Source:   "/city/.beads/dolt-server.port",
			Fallback: false,
		},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)

	wantKeys := []string{
		`"schema":"gc.dolt.cleanup.v1"`,
		`"port":{`,
		`"rigs_protected":[]`,
		`"dropped":{`,
		`"purge":{`,
		`"reaped":{`,
		`"summary":{`,
		`"errors":[]`,
	}
	for _, key := range wantKeys {
		if !strings.Contains(got, key) {
			t.Errorf("JSON missing %q\nfull JSON:\n%s", key, got)
		}
	}
}

func TestRunDoltCleanup_JSONOutputsResolvedPort(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Flag:     "",
		CityPort: 0,
		Rigs:     rigs,
		FS:       fs,
		JSON:     true,
		Probe:    false, // skip TCP probe in unit tests
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runDoltCleanup exit=%d, stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal stdout: %v\nstdout: %s", err, stdout.String())
	}
	if r.Schema != "gc.dolt.cleanup.v1" {
		t.Errorf("Schema = %q", r.Schema)
	}
	if r.Port.Resolved != 28231 {
		t.Errorf("Port.Resolved = %d, want 28231", r.Port.Resolved)
	}
	if r.Port.Fallback {
		t.Errorf("Port.Fallback = true, want false")
	}
}

func TestRunDoltCleanup_HumanOutputShowsPortAndFallbackWarning(t *testing.T) {
	fs := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:    fs,
		JSON:  false,
		Probe: false,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "3307") {
		t.Errorf("stdout missing legacy port 3307: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "fallback") && !strings.Contains(strings.ToLower(out), "legacy default") {
		t.Errorf("stdout missing fallback indicator: %s", out)
	}
}

func TestRunDoltCleanup_FlagOverridesEverything(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Flag:     "9999",
		CityPort: 4242,
		Rigs:     []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:       fs,
		JSON:     true,
		Probe:    false,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Port.Resolved != 9999 {
		t.Errorf("Port.Resolved = %d, want 9999", r.Port.Resolved)
	}
	if r.Port.Source != "--port flag" {
		t.Errorf("Port.Source = %q", r.Port.Source)
	}
}

func TestRunDoltCleanup_RigsProtectedFromRegistry(t *testing.T) {
	// Wireframe-6 schema requires rigs_protected to enumerate registered rigs.
	// One entry per registered rig (HQ + non-HQ); each rig's DB name equals
	// its rig name in this codebase (`gascity`, `beads`, etc.). Order is
	// HQ-first to match the resolver's port-resolution preference.
	fs := fsys.NewFake()
	rigs := []resolverRig{
		{Name: "gascity", Path: "/city", HQ: true},
		{Name: "beads", Path: "/beads", HQ: false},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:  rigs,
		FS:    fs,
		JSON:  true,
		Probe: false,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := []CleanupRigProtection{
		{Rig: "gascity", DB: "gascity"},
		{Rig: "beads", DB: "beads"},
	}
	if len(r.RigsProtected) != len(want) {
		t.Fatalf("RigsProtected len = %d, want %d (got %v)", len(r.RigsProtected), len(want), r.RigsProtected)
	}
	for i, w := range want {
		if r.RigsProtected[i] != w {
			t.Errorf("RigsProtected[%d] = %+v, want %+v", i, r.RigsProtected[i], w)
		}
	}
}

func TestRunDoltCleanup_DryRunReportsReapPlanWithoutKilling(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	procs := []DoltProcInfo{
		{PID: 1138290, Ports: []int{28231}, Argv: []string{"dolt", "sql-server"}},
		{PID: 1281044, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestA/config.yaml"}},
		{PID: 1319499, Ports: []int{33400}, Argv: []string{"dolt", "sql-server", "--config", "/tmp/be-s9d-bench-dolt/config.yaml"}},
	}
	killed := []int{}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:    []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:      fs,
		JSON:    true,
		Probe:   false,
		HomeDir: "/home/u",
		// Force not set → dry-run.
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess: func(pid int, _ syscall.Signal) error {
			killed = append(killed, pid)
			return nil
		},
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	if r.Reaped.Count != 1 {
		t.Errorf("Reaped.Count = %d, want 1 (one orphan, dry-run)", r.Reaped.Count)
	}
	wantProtected := []int{1138290, 1319499}
	if !equalIntSlice(r.Reaped.ProtectedPIDs, wantProtected) {
		t.Errorf("ProtectedPIDs = %v, want %v", r.Reaped.ProtectedPIDs, wantProtected)
	}
	if len(killed) != 0 {
		t.Errorf("KillProcess called %d times in dry-run; want 0 (dry-run is non-destructive)", len(killed))
	}
}

func TestRunDoltCleanup_ForceKillsOrphans(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")

	procs := []DoltProcInfo{
		{PID: 1138290, Ports: []int{28231}, Argv: []string{"dolt", "sql-server"}},
		{PID: 1281044, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestA/config.yaml"}},
		{PID: 1281099, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestB/config.yaml"}},
	}
	var termed []int

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:                fs,
		JSON:              true,
		Force:             true,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess: func(pid int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				termed = append(termed, pid)
			}
			return syscall.ESRCH // pretend the process is already gone after TERM
		},
		ReapGracePeriod: 1, // tiny so the test doesn't sleep meaningfully
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Reaped.Count != 2 {
		t.Errorf("Reaped.Count = %d, want 2", r.Reaped.Count)
	}
	wantTermed := []int{1281044, 1281099}
	if !equalIntSlice(termed, wantTermed) {
		t.Errorf("SIGTERM-ed PIDs = %v, want %v", termed, wantTermed)
	}
}

func TestRunDoltCleanup_ForceCountsSuccessfulKill(t *testing.T) {
	procs := []DoltProcInfo{
		{PID: 4444, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"}},
	}
	var signals []syscall.Signal

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		Force:             true,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Reaped.Count != len(procs) {
		t.Errorf("Reaped.Count = %d, want %d", r.Reaped.Count, len(procs))
	}
	if r.Summary.ErrorsTotal != 0 {
		t.Errorf("Summary.ErrorsTotal = %d, want 0", r.Summary.ErrorsTotal)
	}
	if len(r.Errors) != 0 || len(r.Reaped.Errors) != 0 {
		t.Errorf("errors = %#v, reap errors = %#v; want none", r.Errors, r.Reaped.Errors)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Errorf("signals = %v, want [SIGTERM SIGKILL]", signals)
	}
}

func TestRunDoltCleanup_ForceRecordsKillError(t *testing.T) {
	procs := []DoltProcInfo{
		{PID: 4444, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"}},
	}
	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		Force:             true,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
		KillProcess: func(_ int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				return nil
			}
			return syscall.EPERM
		},
		ReapGracePeriod: 1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(r.Reaped.Errors) == 0 {
		t.Errorf("Reaped.Errors empty; want non-zero kill error")
	}
	if r.Reaped.Count != 0 {
		t.Errorf("Reaped.Count = %d, want 0 because SIGKILL failed", r.Reaped.Count)
	}
	if r.Summary.ErrorsTotal != 1 {
		t.Errorf("Summary.ErrorsTotal = %d, want 1", r.Summary.ErrorsTotal)
	}
	if len(r.Errors) != 1 {
		t.Fatalf("len(Errors) = %d, want 1: %#v", len(r.Errors), r.Errors)
	}
	if r.Errors[0].Stage != "reap" || r.Errors[0].Name != "pid 4444" || !strings.Contains(r.Errors[0].Error, "SIGKILL") {
		t.Errorf("Errors[0] = %#v, want top-level reap SIGKILL error for pid 4444", r.Errors[0])
	}
}

func TestRunDoltCleanup_RigsProtectedReadsDoltDatabaseFromMetadata(t *testing.T) {
	// When a rig's metadata.json sets dolt_database, the protection entry MUST
	// use that value as DB (not the rig name) so the drop step doesn't
	// accidentally target a rig DB whose operator-chosen name differs from
	// the rig's registered name. Falls back to rig.Name when metadata is
	// missing or doesn't specify dolt_database.
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	fs.Files["/rigs/foo/.beads/metadata.json"] = []byte(`{"dolt_database":"foo_db"}`)
	fs.Files["/rigs/bar/.beads/metadata.json"] = []byte(`{"database":"sqlite"}`) // no dolt_database
	// /rigs/missing has no metadata.json at all.

	rigs := []resolverRig{
		{Name: "city", Path: "/city", HQ: true},
		{Name: "foo", Path: "/rigs/foo"},
		{Name: "bar", Path: "/rigs/bar"},
		{Name: "missing", Path: "/rigs/missing"},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:  rigs,
		FS:    fs,
		JSON:  true,
		Probe: false,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runDoltCleanup exit=%d, stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	want := []CleanupRigProtection{
		{Rig: "city", DB: "hq"},         // from metadata
		{Rig: "foo", DB: "foo_db"},      // from metadata
		{Rig: "bar", DB: "bar"},         // metadata present but no dolt_database — fall back to rig.Name
		{Rig: "missing", DB: "missing"}, // no metadata — fall back to rig.Name
	}
	if len(r.RigsProtected) != len(want) {
		t.Fatalf("RigsProtected len = %d, want %d (got %+v)", len(r.RigsProtected), len(want), r.RigsProtected)
	}
	for i, w := range want {
		if r.RigsProtected[i] != w {
			t.Errorf("RigsProtected[%d] = %+v, want %+v", i, r.RigsProtected[i], w)
		}
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
