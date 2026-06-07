package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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

	// Slice 2 (ga-9h05hk): live-session probe fields. Defaults to
	// empty/nil so existing tests retain their semantics — an
	// uninitialized fake reports no live sessions and no probe error.
	liveSessions    map[string]int
	liveSessionsErr error
	probeCalls      int
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

func (f *fakeCleanupDoltClient) ProbeLiveSessions(_ context.Context) (map[string]int, error) {
	f.probeCalls++
	if f.liveSessionsErr != nil {
		return nil, f.liveSessionsErr
	}
	out := map[string]int{}
	for k, v := range f.liveSessions {
		out[k] = v
	}
	return out, nil
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

// blockingProbeCleanupDoltClient overrides ProbeLiveSessions to block on
// the caller's context until cancellation, returning ctx.Err(). Used by
// TestProbeLiveSessions_TimesOut to exercise the FAIL-CLOSED wiring
// without sleeping for the full cleanupLiveSessionProbeTimeout in CI.
type blockingProbeCleanupDoltClient struct {
	fakeCleanupDoltClient
}

func (b *blockingProbeCleanupDoltClient) ProbeLiveSessions(ctx context.Context) (map[string]int, error) {
	b.probeCalls++
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestProbeLiveSessions_HealthyServer(t *testing.T) {
	// Direct unit on applyLiveSessionsToPlan: pure function asserts.
	inputPlan := DoltDropPlan{
		ToDrop: []string{"testdb_a", "testdb_busy", "doctest_y", "testdb_c"},
	}
	sessions := map[string]int{"testdb_busy": 3, "doctest_y": 1}

	result := applyLiveSessionsToPlan(inputPlan, sessions)

	for _, name := range result.ToDrop {
		if name == "testdb_busy" || name == "doctest_y" {
			t.Errorf("ToDrop contains live-session DB %q; want it removed", name)
		}
	}
	wantSkipped := []DoltDropSkip{
		{Name: "testdb_busy", Reason: "live-session"},
		{Name: "doctest_y", Reason: "live-session"},
	}
	if len(result.Skipped) != len(wantSkipped) {
		t.Fatalf("Skipped len = %d, want %d; got=%+v", len(result.Skipped), len(wantSkipped), result.Skipped)
	}
	for i, want := range wantSkipped {
		if result.Skipped[i] != want {
			t.Errorf("Skipped[%d] = %+v, want %+v", i, result.Skipped[i], want)
		}
	}
	if DropSkipReasonLiveSession != "live-session" {
		t.Errorf("DropSkipReasonLiveSession = %q, want %q", DropSkipReasonLiveSession, "live-session")
	}

	// Drive runDoltCleanup to verify the fake's ProbeLiveSessions is
	// called exactly once.
	client := &fakeCleanupDoltClient{
		databases:    []string{"hq", "testdb_a", "testdb_busy", "doctest_y", "testdb_c"},
		liveSessions: sessions,
	}
	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	if code := runDoltCleanup(opts, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d, stderr=%q", code, stderr.String())
	}
	if client.probeCalls != 1 {
		t.Errorf("probeCalls = %d, want 1", client.probeCalls)
	}
}

func TestProbeLiveSessions_TimesOut(t *testing.T) {
	// Shrink the probe deadline so the test does not sleep for the full
	// 2 s production value. The wiring is what we are exercising — the
	// value is pinned by TestProbeLiveSessions_FailClosed.
	oldTimeout := cleanupLiveSessionProbeTimeout
	cleanupLiveSessionProbeTimeout = 50 * time.Millisecond
	defer func() { cleanupLiveSessionProbeTimeout = oldTimeout }()

	client := &blockingProbeCleanupDoltClient{
		fakeCleanupDoltClient: fakeCleanupDoltClient{
			databases: []string{"testdb_a", "testdb_b"},
		},
	}
	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}

	start := time.Now()
	runDoltCleanup(opts, &stdout, &stderr)
	elapsed := time.Since(start)
	// Allow 10x the probe timeout as wall-clock budget. Under parallel test
	// load the scheduler may delay goroutine wakeups past the theoretical
	// minimum; 10x gives headroom without letting a truly hung probe pass.
	wallBudget := cleanupLiveSessionProbeTimeout * 10
	if elapsed >= wallBudget {
		t.Errorf("wall time = %v, want < %v (10x probe timeout)", elapsed, wallBudget)
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if len(r.ForceBlockers) != 1 {
		t.Fatalf("ForceBlockers = %+v, want exactly one entry", r.ForceBlockers)
	}
	if r.ForceBlockers[0].Kind != "live-session-probe-failed" {
		t.Errorf("ForceBlockers[0].Kind = %q, want %q", r.ForceBlockers[0].Kind, "live-session-probe-failed")
	}
	if r.ForceBlockers[0].Name != "" {
		t.Errorf("ForceBlockers[0].Name = %q, want \"\"", r.ForceBlockers[0].Name)
	}
	if !strings.Contains(r.ForceBlockers[0].Error, "context deadline exceeded") {
		t.Errorf("ForceBlockers[0].Error = %q, want contains %q", r.ForceBlockers[0].Error, "context deadline exceeded")
	}
	if len(client.dropped) != 0 {
		t.Errorf("DropDatabase called %d times, want 0", len(client.dropped))
	}
}

func TestProbeLiveSessions_FailClosed(t *testing.T) {
	// Pin the production deadline value here. Other tests may override
	// cleanupLiveSessionProbeTimeout, but the live constant must be 2 s.
	if cleanupLiveSessionProbeTimeout != 2*time.Second {
		t.Errorf("cleanupLiveSessionProbeTimeout = %v, want %v", cleanupLiveSessionProbeTimeout, 2*time.Second)
	}

	client := &fakeCleanupDoltClient{
		databases:       []string{"testdb_a", "testdb_b", "doctest_c"},
		liveSessionsErr: errors.New("connection reset by peer"),
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
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (force + probe failure); stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if len(r.ForceBlockers) != 1 {
		t.Fatalf("ForceBlockers = %+v, want exactly one entry", r.ForceBlockers)
	}
	if r.ForceBlockers[0].Kind != "live-session-probe-failed" {
		t.Errorf("ForceBlockers[0].Kind = %q, want %q", r.ForceBlockers[0].Kind, "live-session-probe-failed")
	}
	if r.ForceBlockers[0].Error != "connection reset by peer" {
		t.Errorf("ForceBlockers[0].Error = %q, want %q", r.ForceBlockers[0].Error, "connection reset by peer")
	}

	probeErrors := 0
	for _, e := range r.Errors {
		if e.Kind == "live-session-probe-failed" {
			probeErrors++
			if e.Stage != "drop" {
				t.Errorf("Errors[].Stage = %q, want %q", e.Stage, "drop")
			}
			if e.Name != "" {
				t.Errorf("Errors[].Name = %q, want \"\"", e.Name)
			}
			if e.Error != "connection reset by peer" {
				t.Errorf("Errors[].Error = %q, want %q", e.Error, "connection reset by peer")
			}
		}
	}
	if probeErrors != 1 {
		t.Errorf("Errors with kind=live-session-probe-failed: %d, want 1; errors=%+v", probeErrors, r.Errors)
	}

	if len(client.dropped) != 0 {
		t.Errorf("DropDatabase called %d times, want 0; got %v", len(client.dropped), client.dropped)
	}
	if client.purged != 0 {
		t.Errorf("PurgeDroppedDatabases called %d times, want 0", client.purged)
	}
}

func TestProbeLiveSessions_DryRunSurvivesFailure(t *testing.T) {
	client := &fakeCleanupDoltClient{
		databases:       []string{"testdb_a", "testdb_b", "doctest_c"},
		liveSessionsErr: errors.New("connection reset by peer"),
	}
	var stdout, stderr bytes.Buffer
	opts := cleanupOptions{
		FS:                fsys.NewFake(),
		JSON:              true,
		Force:             false,
		DoltClient:        client,
		DiscoverProcesses: func() ([]DoltProcInfo, error) { return nil, nil },
	}
	code := runDoltCleanup(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (dry-run + probe failure); stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}
	if len(r.ForceBlockers) != 1 {
		t.Fatalf("ForceBlockers = %+v, want exactly one entry", r.ForceBlockers)
	}
	if r.ForceBlockers[0].Kind != "live-session-probe-failed" {
		t.Errorf("ForceBlockers[0].Kind = %q, want %q", r.ForceBlockers[0].Kind, "live-session-probe-failed")
	}
	for _, e := range r.Errors {
		if e.Kind == "live-session-probe-failed" {
			t.Errorf("dry-run promoted probe failure into Errors: %+v", e)
		}
	}
	wantDropped := []string{"testdb_a", "testdb_b", "doctest_c"}
	if !equalStringSlice(r.Dropped.Names, wantDropped) {
		t.Errorf("Dropped.Names = %v, want %v (would-be plan visible)", r.Dropped.Names, wantDropped)
	}
	if len(client.dropped) != 0 {
		t.Errorf("DropDatabase called %d times, want 0 in dry-run", len(client.dropped))
	}
}

func TestProbeLiveSessions_RemovesFromToDrop(t *testing.T) {
	client := &fakeCleanupDoltClient{
		databases:    []string{"hq", "beads", "testdb_a", "testdb_b", "doctest_c"},
		liveSessions: map[string]int{"testdb_a": 1, "doctest_c": 2, "beads": 5},
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
	if code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", code, stderr.String())
	}

	var r CleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("Unmarshal: %v\nstdout: %s", err, stdout.String())
	}

	wantDropped := []string{"testdb_b"}
	if !equalStringSlice(r.Dropped.Names, wantDropped) {
		t.Errorf("Dropped.Names = %v, want %v", r.Dropped.Names, wantDropped)
	}

	wantSkipped := []DoltDropSkip{
		{Name: "testdb_a", Reason: "live-session"},
		{Name: "doctest_c", Reason: "live-session"},
	}
	if len(r.Dropped.Skipped) != len(wantSkipped) {
		t.Fatalf("Dropped.Skipped len = %d, want %d; got=%+v", len(r.Dropped.Skipped), len(wantSkipped), r.Dropped.Skipped)
	}
	for i, want := range wantSkipped {
		if r.Dropped.Skipped[i] != want {
			t.Errorf("Dropped.Skipped[%d] = %+v, want %+v", i, r.Dropped.Skipped[i], want)
		}
	}
	for _, s := range r.Dropped.Skipped {
		if s.Name == "beads" {
			t.Errorf("Skipped contains 'beads' which was never in ToDrop: %+v", s)
		}
	}

	if !equalStringSlice(client.dropped, []string{"testdb_b"}) {
		t.Errorf("DropDatabase called with %v, want [testdb_b]", client.dropped)
	}
}
