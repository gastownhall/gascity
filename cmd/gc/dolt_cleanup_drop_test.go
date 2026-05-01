package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// fakeCleanupDoltClient is an injectable implementation of
// CleanupDoltClient that records calls so tests can assert on the order
// and arguments of operations the cleanup engine performs.
type fakeCleanupDoltClient struct {
	databases []string
	dropped   []string
	purged    int
	dropErr   map[string]error
}

func (f *fakeCleanupDoltClient) ListDatabases(_ context.Context) ([]string, error) {
	out := make([]string, len(f.databases))
	copy(out, f.databases)
	return out, nil
}

func (f *fakeCleanupDoltClient) DropDatabase(_ context.Context, name string) error {
	if err, ok := f.dropErr[name]; ok {
		return err
	}
	f.dropped = append(f.dropped, name)
	// Reflect the drop in the live database listing so subsequent ListDatabases
	// calls see a converged view.
	for i, d := range f.databases {
		if d == name {
			f.databases = append(f.databases[:i], f.databases[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeCleanupDoltClient) PurgeDroppedDatabases(_ context.Context, _ string) error {
	f.purged++
	return nil
}

func (f *fakeCleanupDoltClient) Close() error { return nil }

func TestRunDoltCleanup_DryRunEnumeratesDropCandidatesWithoutDropping(t *testing.T) {
	client := &fakeCleanupDoltClient{
		databases: []string{"hq", "beads", "testdb_abc", "doctest_x", "user_data"},
	}
	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "beads", Path: "/beads"},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fsys.NewFake(),
		JSON:              true,
		Probe:             false,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	if r.Dropped.Count != 2 {
		t.Errorf("Dropped.Count = %d, want 2 (testdb_abc, doctest_x)", r.Dropped.Count)
	}
	if len(client.dropped) != 0 {
		t.Errorf("DropDatabase called %d times in dry-run; want 0", len(client.dropped))
	}
}

func TestRunDoltCleanup_ForceDropsStaleDatabases(t *testing.T) {
	client := &fakeCleanupDoltClient{
		databases: []string{"hq", "beads", "testdb_abc", "doctest_x"},
	}
	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "beads", Path: "/beads"},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fsys.NewFake(),
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if r.Dropped.Count != 2 {
		t.Errorf("Dropped.Count = %d, want 2", r.Dropped.Count)
	}
	wantDropped := []string{"testdb_abc", "doctest_x"}
	if !equalStringSlice(client.dropped, wantDropped) {
		t.Errorf("dropped = %v, want %v", client.dropped, wantDropped)
	}
}

func TestRunDoltCleanup_ForceRecordsDropFailureAndContinues(t *testing.T) {
	client := &fakeCleanupDoltClient{
		databases: []string{"testdb_a", "testdb_b", "testdb_c"},
		dropErr: map[string]error{
			"testdb_b": fmt.Errorf("boom"),
		},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		Force:             true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
		ReapGracePeriod:   1,
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	// Drop failures don't fail the whole run — they're recorded into the
	// report and the operator decides whether to retry. Exit code stays 0
	// when the rest of the run succeeded; per-stage errors are visible
	// via the JSON envelope and human-readable error section.
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	wantDropped := []string{"testdb_a", "testdb_c"}
	if !equalStringSlice(client.dropped, wantDropped) {
		t.Errorf("dropped = %v, want %v", client.dropped, wantDropped)
	}
	if len(r.Dropped.Failed) != 1 || r.Dropped.Failed[0].Name != "testdb_b" {
		t.Errorf("Dropped.Failed = %+v, want one entry for testdb_b", r.Dropped.Failed)
	}
	if !strings.Contains(r.Dropped.Failed[0].Error, "boom") {
		t.Errorf("failure error = %q, want to contain 'boom'", r.Dropped.Failed[0].Error)
	}
}
