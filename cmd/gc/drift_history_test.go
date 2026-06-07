package main

// Unit-test coverage for drift_history.go (added in ga-xbgq, commit
// ee37578d). The integration test
// TestStartDrift_RestartLoopGuard_RefusesFourthInWindow in
// test/integration/start_drift_test.go pins the happy path: four
// cycles in 60s and the fourth is refused. These unit tests cover the
// four edge cases the integration suite cannot easily exercise:
//
//  1. corrupt-JSON path — loadDriftRestartHistory returns nil so a
//     torn file does not block startup;
//  2. missing-file path — loadDriftRestartHistory returns nil so a
//     fresh install does not block startup;
//  3. prune-then-rewrite when budget exhausted —
//     recordDriftRestartAttempt persists the pruned history on the
//     refuse path so stale entries do not accumulate forever;
//  4. atomic-rename invariant — saveDriftRestartHistory writes via
//     temp-file-then-rename and leaves no .tmp residue, so concurrent
//     `gc start` invocations cannot observe a torn file.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fixedNow is the canonical "current time" used across the test
// table. Using a fixed instant keeps assertions stable and avoids
// flakes from clock skew between time.Now() calls inside the helpers
// and the test's own time.Now().
var fixedNow = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

// TestLoadDriftRestartHistory_EdgeCases covers the two non-error
// "treat as empty" paths the bead calls out: missing file (fresh
// install) and corrupt JSON (torn write).
func TestLoadDriftRestartHistory_EdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, path string)
		want    int
	}{
		{
			name:    "missing file returns empty",
			prepare: func(_ *testing.T, _ string) {},
			want:    0,
		},
		{
			name: "corrupt JSON returns empty",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
					t.Fatalf("seeding corrupt file: %v", err)
				}
			},
			want: 0,
		},
		{
			name: "empty file returns empty",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
					t.Fatalf("seeding empty file: %v", err)
				}
			},
			want: 0,
		},
		{
			name: "wrong schema (object) returns empty",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(`{"attempts":[]}`), 0o600); err != nil {
					t.Fatalf("seeding wrong-schema file: %v", err)
				}
			},
			want: 0,
		},
		{
			name: "valid empty array returns empty",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
					t.Fatalf("seeding empty array: %v", err)
				}
			},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "drift-restart-history.json")
			tc.prepare(t, path)
			got := loadDriftRestartHistory(path)
			if len(got) != tc.want {
				t.Fatalf("loadDriftRestartHistory len = %d, want %d (got=%v)", len(got), tc.want, got)
			}
		})
	}
}

// TestLoadDriftRestartHistory_RoundtripsValidJSON pins the load/save
// pair: timestamps written by saveDriftRestartHistory must come back
// in the same order (ascending by time) and with nanosecond
// precision. A regression that drops sub-second precision (e.g.
// converting to time.Unix(s, 0)) or that loses ordering would surface
// here.
func TestLoadDriftRestartHistory_RoundtripsValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drift-restart-history.json")

	// Deliberately unordered so the helper has to sort.
	in := []time.Time{
		fixedNow.Add(-2 * time.Second),
		fixedNow.Add(-30 * time.Second),
		fixedNow.Add(-100 * time.Millisecond),
	}
	if err := saveDriftRestartHistory(path, in); err != nil {
		t.Fatalf("saveDriftRestartHistory: %v", err)
	}

	got := loadDriftRestartHistory(path)
	if len(got) != len(in) {
		t.Fatalf("loaded %d entries, want %d", len(got), len(in))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Before(got[i-1]) {
			t.Errorf("loaded history not sorted ascending at index %d: %s before %s", i, got[i], got[i-1])
		}
	}
	// Nanosecond precision: every input timestamp must appear in the
	// output exactly once.
	want := map[int64]int{}
	for _, ts := range in {
		want[ts.UnixNano()]++
	}
	for _, ts := range got {
		want[ts.UnixNano()]--
	}
	for ns, count := range want {
		if count != 0 {
			t.Errorf("timestamp %d UnixNano: balance %+d (saved vs loaded)", ns, count)
		}
	}
}

// TestSaveDriftRestartHistory_AtomicRename pins the temp-file-then-
// rename invariant. After a successful save:
//
//   - no .tmp residue remains in the parent directory;
//   - the target file is the renamed temp file, not a partial write.
//
// The rename guarantee is the load-bearing property: a concurrent
// reader either sees the pre-write or post-write state, never
// a torn JSON payload.
func TestSaveDriftRestartHistory_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drift-restart-history.json")
	if err := saveDriftRestartHistory(path, []time.Time{fixedNow}); err != nil {
		t.Fatalf("saveDriftRestartHistory: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	var residual []string
	for _, e := range entries {
		if e.Name() == "drift-restart-history.json" {
			continue
		}
		residual = append(residual, e.Name())
	}
	if len(residual) != 0 {
		t.Errorf("temp-file residue after save: %v (expected only drift-restart-history.json)", residual)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	var stamps []int64
	if err := json.Unmarshal(data, &stamps); err != nil {
		t.Fatalf("written file not valid JSON int64 array: %v\nraw: %s", err, string(data))
	}
	if len(stamps) != 1 || stamps[0] != fixedNow.UnixNano() {
		t.Errorf("written stamps = %v, want [%d]", stamps, fixedNow.UnixNano())
	}
}

// TestSaveDriftRestartHistory_ConcurrentWritesLeaveValidFile pins the
// "concurrent gc start invocations" scenario from the bead. Multiple
// goroutines call saveDriftRestartHistory simultaneously, each with a
// different attempt set. After they all finish:
//
//   - the file is parseable JSON (never torn);
//   - the file's contents match one of the written sets exactly;
//   - no .tmp residue remains.
//
// We do not assert WHICH set wins — POSIX rename ordering under
// concurrent writers is implementation-defined. Only the
// non-torn invariant matters.
func TestSaveDriftRestartHistory_ConcurrentWritesLeaveValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drift-restart-history.json")

	const writers = 8
	const writes = 32

	// Each writer uses a distinct stamp value so we can identify which
	// writer's payload finally lands.
	wantSets := make(map[string]bool, writers)
	for w := 0; w < writers; w++ {
		stamps := []int64{int64(w + 1)}
		key := fmt.Sprintf("%v", stamps)
		wantSets[key] = true
	}

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		w := w
		go func() {
			defer wg.Done()
			payload := []time.Time{time.Unix(0, int64(w+1))}
			for i := 0; i < writes; i++ {
				if err := saveDriftRestartHistory(path, payload); err != nil {
					t.Errorf("writer %d save: %v", w, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading final file: %v", err)
	}
	var stamps []int64
	if err := json.Unmarshal(data, &stamps); err != nil {
		t.Fatalf("final file torn: %v\nraw: %s", err, string(data))
	}
	if key := fmt.Sprintf("%v", stamps); !wantSets[key] {
		t.Errorf("final file = %v, not any of the writer payloads", stamps)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "drift-restart-history.json" {
			continue
		}
		t.Errorf("unexpected residue after concurrent writes: %s", e.Name())
	}
}

// TestSaveDriftRestartHistory_CreatesParentDirectory pins the
// MkdirAll(0o700) at the top of saveDriftRestartHistory. A fresh
// GC_HOME may not have the parent directory yet — the save call must
// create it rather than fail with ENOENT.
func TestSaveDriftRestartHistory_CreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested", "deeper")
	path := filepath.Join(nested, "drift-restart-history.json")
	if err := saveDriftRestartHistory(path, []time.Time{fixedNow}); err != nil {
		t.Fatalf("saveDriftRestartHistory into missing parent: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created at %s: %v", path, err)
	}
	info, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("parent not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("parent at %s is not a directory", nested)
	}
}

// TestPruneRestartHistory_Cases is a table-driven check of the
// pruning predicate: an entry is kept iff its timestamp is strictly
// after now-window. Entries exactly at the cutoff are dropped (the
// guard's window is half-open: (now-window, now]). The returned
// slice is freshly allocated so the caller can persist it without
// aliasing the input.
func TestPruneRestartHistory_Cases(t *testing.T) {
	window := 60 * time.Second
	cases := []struct {
		name string
		in   []time.Time
		want []time.Time
	}{
		{
			name: "empty input",
			in:   nil,
			want: nil,
		},
		{
			name: "all entries inside window",
			in: []time.Time{
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-10 * time.Second),
				fixedNow,
			},
			want: []time.Time{
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-10 * time.Second),
				fixedNow,
			},
		},
		{
			name: "all entries outside window",
			in: []time.Time{
				fixedNow.Add(-120 * time.Second),
				fixedNow.Add(-90 * time.Second),
			},
			want: nil,
		},
		{
			name: "entry exactly at cutoff is dropped",
			in: []time.Time{
				fixedNow.Add(-60 * time.Second), // cutoff = now - window: After(cutoff) is false
				fixedNow.Add(-59 * time.Second), // strictly inside: kept
			},
			want: []time.Time{
				fixedNow.Add(-59 * time.Second),
			},
		},
		{
			name: "mixed inside and outside",
			in: []time.Time{
				fixedNow.Add(-120 * time.Second),
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-90 * time.Second),
				fixedNow.Add(-5 * time.Second),
			},
			want: []time.Time{
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-5 * time.Second),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pruneRestartHistory(tc.in, fixedNow, window)
			if len(got) != len(tc.want) {
				t.Fatalf("pruned len = %d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if !got[i].Equal(tc.want[i]) {
					t.Errorf("pruned[%d] = %s, want %s", i, got[i], tc.want[i])
				}
			}
			// Freshly allocated: the returned slice's underlying
			// array must not alias the input.
			if len(got) > 0 && len(tc.in) > 0 && &got[0] == &tc.in[0] {
				t.Errorf("pruned aliases input slice (same backing array)")
			}
		})
	}
}

// TestRecordDriftRestartAttempt_Cases is the central table covering
// the loop-guard's decision matrix. Each case seeds an on-disk
// history (or leaves it absent / corrupt), invokes
// recordDriftRestartAttempt with maxAttempts=3, window=60s, and
// asserts the boolean return AND the persisted file state. The
// `wantPersisted` slice describes what must remain in the file after
// the call — this is the load-bearing prune-then-rewrite assertion
// for the over-budget refuse path.
func TestRecordDriftRestartAttempt_Cases(t *testing.T) {
	const maxAttempts = 3
	const window = 60 * time.Second

	cases := []struct {
		name           string
		seedAttempts   []time.Time // nil = no file written
		seedRaw        string      // non-empty = write this literal instead (overrides seedAttempts)
		wantReturn     bool
		wantPersisted  []time.Time
		wantFileExists bool
	}{
		{
			name:           "first attempt on empty store appends and returns true",
			seedAttempts:   nil,
			wantReturn:     true,
			wantPersisted:  []time.Time{fixedNow},
			wantFileExists: true,
		},
		{
			name: "second attempt within window appends and returns true",
			seedAttempts: []time.Time{
				fixedNow.Add(-10 * time.Second),
			},
			wantReturn: true,
			wantPersisted: []time.Time{
				fixedNow.Add(-10 * time.Second),
				fixedNow,
			},
			wantFileExists: true,
		},
		{
			name: "third attempt within window appends and returns true (still under maxAttempts)",
			seedAttempts: []time.Time{
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-10 * time.Second),
			},
			wantReturn: true,
			wantPersisted: []time.Time{
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-10 * time.Second),
				fixedNow,
			},
			wantFileExists: true,
		},
		{
			name: "fourth attempt within window is refused but pruned history is rewritten",
			seedAttempts: []time.Time{
				fixedNow.Add(-50 * time.Second),
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-10 * time.Second),
			},
			wantReturn: false,
			wantPersisted: []time.Time{
				fixedNow.Add(-50 * time.Second),
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-10 * time.Second),
			},
			wantFileExists: true,
		},
		{
			name: "stale entries pruned then attempt allowed (under budget post-prune)",
			seedAttempts: []time.Time{
				fixedNow.Add(-120 * time.Second), // stale
				fixedNow.Add(-90 * time.Second),  // stale
				fixedNow.Add(-10 * time.Second),  // fresh
			},
			wantReturn: true,
			wantPersisted: []time.Time{
				fixedNow.Add(-10 * time.Second),
				fixedNow,
			},
			wantFileExists: true,
		},
		{
			name: "stale-pruned-then-refused: pruning drops one but 3 fresh remain → refuse",
			seedAttempts: []time.Time{
				fixedNow.Add(-120 * time.Second), // stale, drops
				fixedNow.Add(-50 * time.Second),  // fresh
				fixedNow.Add(-30 * time.Second),  // fresh
				fixedNow.Add(-5 * time.Second),   // fresh
			},
			wantReturn: false,
			// Pruned file: stale entry is gone; the three fresh
			// entries remain. The refuse path does NOT append now.
			wantPersisted: []time.Time{
				fixedNow.Add(-50 * time.Second),
				fixedNow.Add(-30 * time.Second),
				fixedNow.Add(-5 * time.Second),
			},
			wantFileExists: true,
		},
		{
			name:           "corrupt file treated as empty: first attempt recorded",
			seedRaw:        "{not json",
			wantReturn:     true,
			wantPersisted:  []time.Time{fixedNow},
			wantFileExists: true,
		},
		{
			name: "all-stale history collapses to just now (full prune-then-append)",
			seedAttempts: []time.Time{
				fixedNow.Add(-300 * time.Second),
				fixedNow.Add(-200 * time.Second),
				fixedNow.Add(-100 * time.Second),
			},
			wantReturn:     true,
			wantPersisted:  []time.Time{fixedNow},
			wantFileExists: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "drift-restart-history.json")
			switch {
			case tc.seedRaw != "":
				if err := os.WriteFile(path, []byte(tc.seedRaw), 0o600); err != nil {
					t.Fatalf("seeding raw file: %v", err)
				}
			case tc.seedAttempts != nil:
				if err := saveDriftRestartHistory(path, tc.seedAttempts); err != nil {
					t.Fatalf("seeding via saveDriftRestartHistory: %v", err)
				}
			}

			got := recordDriftRestartAttempt(path, maxAttempts, window, fixedNow)
			if got != tc.wantReturn {
				t.Errorf("recordDriftRestartAttempt = %v, want %v", got, tc.wantReturn)
			}

			if _, err := os.Stat(path); err != nil {
				if tc.wantFileExists {
					t.Fatalf("expected file to exist after call: %v", err)
				}
				return
			}
			persisted := loadDriftRestartHistory(path)
			if len(persisted) != len(tc.wantPersisted) {
				t.Fatalf("persisted len = %d, want %d\n  got:  %v\n  want: %v",
					len(persisted), len(tc.wantPersisted), persisted, tc.wantPersisted)
			}
			for i := range tc.wantPersisted {
				if !persisted[i].Equal(tc.wantPersisted[i]) {
					t.Errorf("persisted[%d] = %s, want %s", i, persisted[i], tc.wantPersisted[i])
				}
			}
		})
	}
}

// TestRecordDriftRestartAttempt_RefusedPathPrunesStaleEntries is the
// most surgical version of the "prune-then-rewrite when budget
// exhausted" assertion from the bead. It seeds a file with stale
// entries plus enough fresh entries to be over budget, calls record,
// and verifies that (a) the call refuses (false), and (b) the file
// no longer contains the stale entry. Without the
// `saveDriftRestartHistory(path, pruned)` call on the refuse arm,
// stale entries would never age out and the file would grow forever.
func TestRecordDriftRestartAttempt_RefusedPathPrunesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drift-restart-history.json")

	seed := []time.Time{
		fixedNow.Add(-10 * time.Minute), // stale, must be pruned
		fixedNow.Add(-50 * time.Second), // fresh
		fixedNow.Add(-30 * time.Second), // fresh
		fixedNow.Add(-10 * time.Second), // fresh
	}
	if err := saveDriftRestartHistory(path, seed); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	got := recordDriftRestartAttempt(path, 3, 60*time.Second, fixedNow)
	if got {
		t.Fatalf("recordDriftRestartAttempt = true, want false (budget exhausted)")
	}

	persisted := loadDriftRestartHistory(path)
	for _, ts := range persisted {
		if ts.Equal(fixedNow.Add(-10 * time.Minute)) {
			t.Errorf("stale entry %s still in persisted history after refuse — prune-then-rewrite invariant broken", ts)
		}
	}
	// Sanity: the three fresh entries must remain.
	if len(persisted) != 3 {
		t.Errorf("persisted = %d entries, want 3 (the fresh ones)", len(persisted))
	}
}

// TestDriftRestartHistoryPath_UsesSupervisorDefaultHome pins the
// integration between drift_history.go and the supervisor's home
// resolver: changing GC_HOME at runtime must redirect the history
// file. Integration tests run with isolated GC_HOME and rely on this
// to avoid clobbering each other's history files.
func TestDriftRestartHistoryPath_UsesSupervisorDefaultHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)
	got := driftRestartHistoryPath()
	want := filepath.Join(dir, "drift-restart-history.json")
	if got != want {
		t.Errorf("driftRestartHistoryPath = %q, want %q", got, want)
	}
}
