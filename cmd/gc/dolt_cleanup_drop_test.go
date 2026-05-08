package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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

func TestRunDoltCleanup_InvalidStaleIdentifiersCountAsDropErrors(t *testing.T) {
	client := &fakeCleanupDoltClient{
		databases: []string{"testdb_bad;drop"},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
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

	if r.Dropped.Count != 0 {
		t.Errorf("Dropped.Count = %d, want 0", r.Dropped.Count)
	}
	if len(r.Dropped.Skipped) != 1 || r.Dropped.Skipped[0].Reason != DropSkipReasonInvalidIdentifier {
		t.Fatalf("Dropped.Skipped = %+v, want one invalid-identifier skip", r.Dropped.Skipped)
	}
	if r.Summary.ErrorsTotal != 1 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 1; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
	if len(r.Errors) != 1 || r.Errors[0].Stage != "drop" || r.Errors[0].Name != "testdb_bad;drop" || !strings.Contains(r.Errors[0].Error, "invalid database identifier") {
		t.Fatalf("Errors = %+v, want invalid identifier drop error", r.Errors)
	}
	if len(client.dropped) != 0 {
		t.Fatalf("DropDatabase called for invalid identifier: %v", client.dropped)
	}
}

func TestRunDoltCleanup_ForceDropsStaleDatabases(t *testing.T) {
	client := &fakeCleanupDoltClient{
		databases: []string{"hq", "beads", "testdb_abc", "doctest_x"},
	}
	fs := fsys.NewFake()
	fs.Files["/city/.beads/metadata.json"] = []byte(`{"dolt_database":"hq"}`)
	fs.Files["/beads/.beads/metadata.json"] = []byte(`{"dolt_database":"beads"}`)
	rigs := []resolverRig{
		{Name: "hq", Path: "/city", HQ: true},
		{Name: "beads", Path: "/beads"},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              rigs,
		FS:                fs,
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

func TestRunDoltCleanup_ForceDisablesDropAndPurgeWhenRigMetadataUnreadable(t *testing.T) {
	fs := fsys.NewFake()
	fs.Errors["/rigs/foo/.beads/metadata.json"] = os.ErrPermission
	putFakeDirTree(fs, "/rigs/foo/.beads/dolt/.dolt_dropped_databases", map[string]int64{
		"db_a/data.bin": 100,
	})
	client := &fakeCleanupDoltClient{
		databases: []string{"foo", "testdb_registered"},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "foo", Path: "/rigs/foo"}},
		FS:                fs,
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
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if len(client.dropped) != 0 {
		t.Fatalf("dropped = %v, want no forced drops when rig DB identity is unknown", client.dropped)
	}
	if client.purged != 0 {
		t.Fatalf("purged = %d, want no forced purge when rig DB identity is unknown", client.purged)
	}
	if r.Dropped.Count != 0 || len(r.Dropped.Names) != 0 {
		t.Fatalf("Dropped = %+v, want no forced drop results when rig DB identity is unknown", r.Dropped)
	}
	if r.Purge.OK {
		t.Fatalf("Purge.OK = true, want false when forced purge is disabled")
	}
	if r.Summary.ErrorsTotal != 1 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 1; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
	if len(r.Errors) != 1 || r.Errors[0].Stage != "rig" || r.Errors[0].Name != "foo" || !strings.Contains(r.Errors[0].Error, "metadata") {
		t.Fatalf("Errors = %+v, want typed rig metadata error", r.Errors)
	}
}

func TestRunDoltCleanup_ForceDisablesDropAndPurgeWhenRigMetadataCorrupt(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/rigs/foo/.beads/metadata.json"] = []byte(`{"dolt_database":`)
	client := &fakeCleanupDoltClient{
		databases: []string{"testdb_registered"},
	}

	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		Rigs:              []resolverRig{{Name: "foo", Path: "/rigs/foo"}},
		FS:                fs,
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
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if len(client.dropped) != 0 {
		t.Fatalf("dropped = %v, want no forced drops when rig metadata is corrupt", client.dropped)
	}
	if client.purged != 0 {
		t.Fatalf("purged = %d, want no forced purge when rig metadata is corrupt", client.purged)
	}
	if r.Summary.ErrorsTotal != 1 {
		t.Fatalf("Summary.ErrorsTotal = %d, want 1; errors=%+v", r.Summary.ErrorsTotal, r.Errors)
	}
	if len(r.Errors) != 1 || r.Errors[0].Stage != "rig" || r.Errors[0].Name != "foo" || !strings.Contains(r.Errors[0].Error, "metadata") {
		t.Fatalf("Errors = %+v, want typed rig metadata error", r.Errors)
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
	if !equalStringSlice(r.Dropped.Names, wantDropped) {
		t.Errorf("Dropped.Names = %v, want successful drops only %v", r.Dropped.Names, wantDropped)
	}
	if len(r.Dropped.Failed) != 1 || r.Dropped.Failed[0].Name != "testdb_b" {
		t.Errorf("Dropped.Failed = %+v, want one entry for testdb_b", r.Dropped.Failed)
	}
	if !strings.Contains(r.Dropped.Failed[0].Error, "boom") {
		t.Errorf("failure error = %q, want to contain 'boom'", r.Dropped.Failed[0].Error)
	}
}
