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
	fs.Files["/city/.beads/dolt-server.port"] = []byte("28231\n")
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
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
func (e *erroringCleanupClient) Close() error                                            { return nil }

type errBoom string

func (e errBoom) Error() string { return string(e) }
