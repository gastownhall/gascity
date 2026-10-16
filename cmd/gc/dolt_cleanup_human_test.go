package main

import (
	"bytes"
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestRunDoltCleanup_HumanOutputShowsAllWireframeSections(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	putCanonicalCityConfig(fs)
	putFakeDirTree(fs, "/city/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_old/data": 4096,
	})

	procs := []DoltProcInfo{
		{PID: 1281044, Argv: []string{"dolt", "sql-server", "--config", "/tmp/TestA/config.yaml"}},
	}
	client := &fakeCleanupDoltClient{
		databases: []string{"hq", "testdb_xyz"},
	}
	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		CityPath:          "/city",
		LiveResolve:       fakeLiveResolve(),
		JSON:              false, // human mode
		DoltClient:        client,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()

	for _, want := range []string{
		"Dolt server",
		"28231",
		"DROPPED-DATABASE DIRECTORIES",
		"testdb_xyz",
		"ORPHAN dolt sql-server PROCESSES",
		"1281044",
		"/tmp/TestA/config.yaml",
		"PROTECTED",
		"hq",
		"SUMMARY",
		"Re-run with --force to apply", // dry-run footer
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRunDoltCleanup_HumanOutputForceOmitsDryRunFooter(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)

	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}
	client := &fakeCleanupDoltClient{databases: []string{"hq"}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		JSON:              false,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
		LiveResolve:       fakeLiveResolve(),
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "Re-run with --force") {
		t.Errorf("force-mode output should NOT show dry-run footer:\n%s", out)
	}
}

func TestRunDoltCleanup_HumanOutputContainsNoANSIEscapes(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)

	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}
	client := &fakeCleanupDoltClient{databases: []string{"hq", "testdb_x"}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		JSON:              false,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "\033[") {
		t.Errorf("human output contains ANSI escape sequence (should be plain text):\n%q", out)
	}
}

func TestRunDoltCleanup_HumanOutputShowsErrorsSection(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)

	rigs := []resolverRig{{Name: "hq", Path: "/city", HQ: true}}
	client := &erroringCleanupClient{databases: []string{"hq", "testdb_x"}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
		JSON:              false,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
		LiveResolve:       fakeLiveResolve(),
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ERRORS") {
		t.Errorf("human output missing ERRORS section when drops failed:\n%s", out)
	}
	if !strings.Contains(out, "drop-boom") {
		t.Errorf("ERRORS section missing the actual error message:\n%s", out)
	}
}

func TestRunDoltCleanup_HumanOutputShowsForceBlockersSection(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/rigs/silent/.beads/metadata.json"] = []byte(`{"database":"sqlite"}`)

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs: []resolverRig{
			{Name: "missing", Path: "/rigs/missing"},
			{Name: "silent", Path: "/rigs/silent"},
		},
		FS:                fs,
		JSON:              false,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		LiveResolve:       fakeLiveResolve(),
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"FORCE BLOCKERS (2)",
		"rig-protection",
		"missing",
		"silent",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing force-blocker detail %q:\n%s", want, out)
		}
	}
}

// humanReportSection returns one blank-line-delimited section of the human
// report, from its header line on, so a test can assert on that section
// without matching text another section prints.
func humanReportSection(t *testing.T, out, header string) string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "\n"+header)
	if !ok {
		t.Fatalf("human output missing section %q\n--- output ---\n%s", header, out)
	}
	section, _, _ := strings.Cut(rest, "\n\n")
	return header + strings.TrimSuffix(section, "\n") + "\n"
}

// TestRunDoltCleanup_HumanOutputOnLegacyFallbackPortSaysNothingWasScanned pins
// what an operator reads when the port fell back to the legacy default. No
// database was listed there, so the dropped section must not read as a clean
// "(none)", and the footer must not invite a --force run that will be refused.
func TestRunDoltCleanup_HumanOutputOnLegacyFallbackPortSaysNothingWasScanned(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	client := &fakeCleanupDoltClient{databases: []string{"hq", "testdb_abc"}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:                fs,
		CityPath:          "/city",
		LiveResolve:       fakeLiveResolveMiss(),
		JSON:              false,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, want 0: a dry run reports the blocker without failing; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "(legacy default — fallback)") {
		t.Fatalf("fixture: want a legacy-default fallback run\n--- output ---\n%s", out)
	}

	dropped := humanReportSection(t, out, "DROPPED-DATABASE DIRECTORIES")
	if !strings.Contains(dropped, "  (not scanned: legacy fallback port)\n") {
		t.Errorf("dropped section must say the legacy fallback port was not scanned:\n%s", dropped)
	}
	if strings.Contains(dropped, "(none)") {
		t.Errorf("dropped section reads (none) for a server that was never listed:\n%s", dropped)
	}

	blockers := humanReportSection(t, out, "FORCE BLOCKERS")
	if !strings.Contains(blockers, cleanupErrorKindLegacyFallbackPort) {
		t.Errorf("FORCE BLOCKERS section missing %s:\n%s", cleanupErrorKindLegacyFallbackPort, blockers)
	}

	if !strings.Contains(out, "\n--force is refused on the legacy fallback port; see FORCE BLOCKERS above.\n") {
		t.Errorf("dry-run footer must say --force is refused on the legacy fallback port:\n%s", out)
	}
	if strings.Contains(out, "Re-run with --force") {
		t.Errorf("dry-run footer invites a --force run that is refused on the legacy fallback port:\n%s", out)
	}

	summary := humanReportSection(t, out, "SUMMARY")
	if !strings.Contains(summary, "  Purge:         skipped\n") {
		t.Errorf("dry-run summary must keep the purge as skipped:\n%s", summary)
	}
}

// TestRunDoltCleanup_HumanOutputForceOnLegacyFallbackPortSaysPurgeRefused pins
// the summary of a forced run on the fallback port: the purge was refused
// before it was attempted, which is not the report of a purge that failed.
func TestRunDoltCleanup_HumanOutputForceOnLegacyFallbackPortSaysPurgeRefused(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	client := &fakeCleanupDoltClient{databases: []string{"hq", "testdb_abc"}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "hq", Path: "/city", HQ: true}},
		FS:                fs,
		CityPath:          "/city",
		LiveResolve:       fakeLiveResolveMiss(),
		JSON:              false,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d, want 1: a forced run on the legacy fallback port must fail; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "(legacy default — fallback)") {
		t.Fatalf("fixture: want a legacy-default fallback run\n--- output ---\n%s", out)
	}

	summary := humanReportSection(t, out, "SUMMARY")
	if !strings.Contains(summary, "  Purge:         refused\n") {
		t.Errorf("forced summary must say the purge was refused on the legacy fallback port:\n%s", summary)
	}
	if strings.Contains(summary, "Purge:         failed") {
		t.Errorf("forced summary reports a failed purge for one that was never attempted:\n%s", summary)
	}
}

func TestRunDoltCleanup_HumanOutputCountsPostSIGTERMGoneAsReaped(t *testing.T) {
	discoverCalls := 0

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:      fsys.NewFake(),
		JSON:    false,
		Force:   true,
		HomeDir: "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) {
			discoverCalls++
			switch discoverCalls {
			case 1, 2:
				return []DoltProcInfo{{
					PID:            4444,
					Argv:           []string{"dolt", "sql-server", "--config", "/tmp/TestX/config.yaml"},
					RSSBytes:       4096,
					StartTimeTicks: 10,
				}}, nil
			default:
				return nil, nil
			}
		},
		KillProcess:     func(_ int, _ syscall.Signal) error { return nil },
		ReapGracePeriod: 1,
		LiveResolve:     fakeLiveResolve(),
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Reaped:        1") {
		t.Errorf("human output did not count post-SIGTERM disappearance as reaped:\n%s", out)
	}
}

func TestRunDoltCleanup_HumanOutputShowsDeletedScopeReapReason(t *testing.T) {
	// A bare deleted-cwd server has no --config; the orphan line must still
	// tell the operator why it is being reaped (ga-10wmzh).
	procs := []DoltProcInfo{{
		PID:      6123,
		Argv:     []string{"dolt", "sql-server", "-H", "127.0.0.1", "-P", "33411"},
		CWDState: procPathStateDeleted,
	}}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              false,
		HomeDir:           "/home/u",
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return procs, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"ORPHAN dolt sql-server PROCESSES (1)",
		"6123",
		"(no --config flag)",
		"deleted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

type erroringCleanupClient struct {
	databases []string
}

func (e *erroringCleanupClient) ListDatabases(_ context.Context) ([]string, error) {
	return append([]string{}, e.databases...), nil
}

func (e *erroringCleanupClient) DropDatabase(_ context.Context, _ string) error {
	return errBoom("drop-boom")
}
func (e *erroringCleanupClient) PurgeDroppedDatabases(_ context.Context, _ string) error { return nil }
func (e *erroringCleanupClient) ProbeLiveSessions(_ context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}
func (e *erroringCleanupClient) Close() error { return nil }

type errBoom string

func (e errBoom) Error() string { return string(e) }
