package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// fakeBlockedRepairBd is a scripted bd for one scope: a version, a stored
// marker (bd's config table), and a recompute that reports a fixed count.
type fakeBlockedRepairBd struct {
	mu           sync.Mutex
	version      string
	marker       string
	rows         int
	versionErr   error
	getErr       error
	recomputeErr error
	setErr       error
	calls        []string
}

func (f *fakeBlockedRepairBd) runner() beads.CommandRunner {
	return func(_, _ string, args ...string) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, strings.Join(args, " "))
		switch {
		case len(args) == 1 && args[0] == "version":
			if f.versionErr != nil {
				return nil, f.versionErr
			}
			return []byte("bd version " + f.version + " (deadbeef)\n"), nil
		case len(args) == 4 && args[0] == "config" && args[1] == "get" && args[3] == blockedRepairMarkerKey:
			if f.getErr != nil {
				return nil, f.getErr
			}
			out, _ := json.Marshal(map[string]any{"key": args[3], "schema_version": 1, "value": f.marker})
			return out, nil
		case len(args) == 2 && args[0] == "recompute-blocked" && args[1] == "--json":
			if f.recomputeErr != nil {
				return nil, f.recomputeErr
			}
			rows := f.rows
			f.rows = 0 // idempotent: a second pass finds nothing left to correct
			return []byte(`{"rows_corrected": ` + strconv.Itoa(rows) + `, "schema_version": 1}`), nil
		case len(args) == 4 && args[0] == "config" && args[1] == "set" && args[2] == blockedRepairMarkerKey:
			if f.setErr != nil {
				return nil, f.setErr
			}
			f.marker = args[3]
			return nil, nil
		}
		return nil, errors.New("unexpected bd call: " + strings.Join(args, " "))
	}
}

func (f *fakeBlockedRepairBd) recomputes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "recompute-blocked") {
			n++
		}
	}
	return n
}

func runBlockedRepairForTest(t *testing.T, bds map[string]*fakeBlockedRepairBd) (*events.Fake, string) {
	t.Helper()
	var scopes []blockedRepairScope
	for _, id := range []string{"city", "rig/alpha", "rig/beta"} {
		if _, ok := bds[id]; ok {
			scopes = append(scopes, blockedRepairScope{id: id, root: "/city/" + id})
		}
	}
	rec := events.NewFake()
	var stderr bytes.Buffer
	runBlockedRepair(scopes, blockedRepairDeps{
		openStore: func(scope blockedRepairScope) *beads.BdStore {
			return beads.NewBdStore(scope.root, bds[scope.id].runner())
		},
		openRecorder: func() (events.Recorder, func()) { return rec, func() {} },
	}, &stderr, "gc start")
	return rec, stderr.String()
}

func TestBlockedRepairRunsOnceWhenNoMarkerExists(t *testing.T) {
	bd := &fakeBlockedRepairBd{version: "1.3.2-rc.1", rows: 2}
	rec, out := runBlockedRepairForTest(t, map[string]*fakeBlockedRepairBd{"city": bd})

	if got := bd.recomputes(); got != 1 {
		t.Fatalf("recompute-blocked ran %d times, want 1", got)
	}
	if bd.marker != "1.3.2-rc.1" {
		t.Fatalf("marker = %q, want the running bd version", bd.marker)
	}
	if !strings.Contains(out, "city") || !strings.Contains(out, "2 rows corrected") {
		t.Fatalf("stderr does not report the correction: %q", out)
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.BeadsBlockedRecomputed {
		t.Fatalf("events = %+v, want one %s", rec.Events, events.BeadsBlockedRecomputed)
	}
	var payload events.BlockedRecomputedPayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	want := events.BlockedRecomputedPayload{Scope: "city", RowsCorrected: 2, BDVersion: "1.3.2-rc.1"}
	if payload != want {
		t.Fatalf("payload = %+v, want %+v", payload, want)
	}
	if rec.Events[0].Subject != "city" {
		t.Fatalf("subject = %q, want city", rec.Events[0].Subject)
	}

	// The next start under the same bd is a no-op: two reads, no recompute.
	rec2, out2 := runBlockedRepairForTest(t, map[string]*fakeBlockedRepairBd{"city": bd})
	if got := bd.recomputes(); got != 1 {
		t.Fatalf("second start re-ran the recompute (%d runs total)", got)
	}
	if len(rec2.Events) != 0 || out2 != "" {
		t.Fatalf("second start was not silent: events=%+v stderr=%q", rec2.Events, out2)
	}
}

func TestBlockedRepairRerunsAfterABdVersionChange(t *testing.T) {
	bd := &fakeBlockedRepairBd{version: "1.3.2", marker: "1.3.1", rows: 0}
	rec, out := runBlockedRepairForTest(t, map[string]*fakeBlockedRepairBd{"city": bd})
	if got := bd.recomputes(); got != 1 {
		t.Fatalf("recompute-blocked ran %d times, want 1 after the bd upgrade", got)
	}
	if bd.marker != "1.3.2" {
		t.Fatalf("marker = %q, want 1.3.2", bd.marker)
	}
	if !strings.Contains(out, "0 rows corrected") {
		t.Fatalf("a clean scope should still report the repair ran: %q", out)
	}
	if len(rec.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(rec.Events))
	}
}

func TestBlockedRepairCoversEveryScopeIndependently(t *testing.T) {
	city := &fakeBlockedRepairBd{version: "1.3.2", marker: "1.3.2"}
	alpha := &fakeBlockedRepairBd{version: "1.3.2", rows: 5}
	beta := &fakeBlockedRepairBd{version: "1.3.2", recomputeErr: errors.New("dolt down")}
	rec, out := runBlockedRepairForTest(t, map[string]*fakeBlockedRepairBd{"city": city, "rig/alpha": alpha, "rig/beta": beta})

	if city.recomputes() != 0 {
		t.Fatal("a scope already repaired under this bd was recomputed again")
	}
	if alpha.recomputes() != 1 || alpha.marker != "1.3.2" {
		t.Fatalf("rig/alpha: recomputes=%d marker=%q", alpha.recomputes(), alpha.marker)
	}
	// A failed recompute leaves no marker, so the next start retries it.
	if beta.marker != "" {
		t.Fatalf("rig/beta: marker %q written after a failed recompute", beta.marker)
	}
	if !strings.Contains(out, "rig/beta") || !strings.Contains(out, "dolt down") || !strings.Contains(out, "retry on next start") {
		t.Fatalf("failure not reported with retry hint: %q", out)
	}
	if len(rec.Events) != 1 || rec.Events[0].Subject != "rig/alpha" {
		t.Fatalf("events = %+v, want exactly the rig/alpha repair", rec.Events)
	}
	beta.recomputeErr = nil
	beta.rows = 1
	runBlockedRepairForTest(t, map[string]*fakeBlockedRepairBd{"rig/beta": beta})
	if beta.marker != "1.3.2" {
		t.Fatalf("rig/beta was not retried on the next start: marker %q", beta.marker)
	}
}

func TestBlockedRepairNeverFailsOnProbeErrors(t *testing.T) {
	for name, bd := range map[string]*fakeBlockedRepairBd{
		"version": {versionErr: errors.New("bd missing")},
		"marker":  {version: "1.3.2", getErr: errors.New("config read refused")},
	} {
		t.Run(name, func(t *testing.T) {
			rec, out := runBlockedRepairForTest(t, map[string]*fakeBlockedRepairBd{"city": bd})
			if bd.recomputes() != 0 {
				t.Fatal("recompute ran although the scope's state could not be read")
			}
			if !strings.Contains(out, "warning") || !strings.Contains(out, "retry on next start") {
				t.Fatalf("probe failure not warned: %q", out)
			}
			if len(rec.Events) != 0 {
				t.Fatalf("events = %+v, want none", rec.Events)
			}
		})
	}
}

func TestBlockedRepairMarkerWriteFailureWarnsAndRetries(t *testing.T) {
	bd := &fakeBlockedRepairBd{version: "1.3.2", rows: 3, setErr: errors.New("read-only")}
	rec, out := runBlockedRepairForTest(t, map[string]*fakeBlockedRepairBd{"city": bd})
	if bd.recomputes() != 1 || len(rec.Events) != 1 {
		t.Fatalf("recompute=%d events=%d, want the repair to run and be recorded", bd.recomputes(), len(rec.Events))
	}
	if !strings.Contains(out, "3 rows corrected") || !strings.Contains(out, "read-only") {
		t.Fatalf("stderr = %q, want both the correction and the marker warning", out)
	}
}

func TestBlockedRepairSkipsBdThatPredatesTheCommand(t *testing.T) {
	bd := &fakeBlockedRepairBd{version: "1.0.4"}
	rec, out := runBlockedRepairForTest(t, map[string]*fakeBlockedRepairBd{"city": bd})
	if len(bd.calls) != 1 || bd.calls[0] != "version" {
		t.Fatalf("calls = %v, want only the version probe", bd.calls)
	}
	if out != "" || len(rec.Events) != 0 {
		t.Fatalf("an old bd should be skipped silently: stderr=%q events=%+v", out, rec.Events)
	}
}

func writeBlockedRepairScope(t *testing.T, root, metadata string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".beads", "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBlockedRepairScopesSkipStoresGCDoesNotOwn pins which scopes the upgrade
// repair may write to: gc-owned Dolt-backed bd scopes only.
func TestBlockedRepairScopesSkipStoresGCDoesNotOwn(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_BEADS_BACKEND", "")
	const doltMeta = `{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"%s"}`
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"c\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBlockedRepairScope(t, city, fmt.Sprintf(doltMeta, "hq"))
	owned := filepath.Join(t.TempDir(), "owned")
	writeBlockedRepairScope(t, owned, fmt.Sprintf(doltMeta, "owned"))
	bound := filepath.Join(t.TempDir(), "bound")
	writeBlockedRepairScope(t, bound, `{"backend":"dolt","storage_endpoint":"https://beads.example","storage_database":"ws1"}`)
	lite := filepath.Join(t.TempDir(), "lite")
	writeBlockedRepairScope(t, lite, `{"database":"doltlite","backend":"doltlite","dolt_database":"lite"}`)
	external := filepath.Join(t.TempDir(), "external")
	writeBlockedRepairScope(t, external, fmt.Sprintf(doltMeta, "external"))
	unmaterialized := filepath.Join(t.TempDir(), "unmaterialized")

	cfg := &config.City{Rigs: []config.Rig{
		{Name: "owned", Path: owned},
		{Name: "bound", Path: bound},
		{Name: "lite", Path: lite},
		{Name: "external", Path: external, DoltHost: "db.example.com", DoltPort: "3306"},
		{Name: "unmaterialized", Path: unmaterialized},
		{Name: "pathless"},
	}}
	var got []string
	for _, s := range blockedRepairScopes(city, cfg) {
		got = append(got, s.id)
	}
	if want := []string{"city", "rig/owned"}; !slices.Equal(got, want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}

	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"c\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if blockedRepairScopeEligible(city, cfg, city) {
		t.Fatal("a file-provider city scope was selected for a bd repair")
	}
}

// TestBlockedRepairRunsScopesInParallelWithinTheBound pins the startup-cost
// shape: recomputes overlap, never more than blockedRepairParallelism at once,
// and the run announces how many scopes it is repairing before it starts.
func TestBlockedRepairRunsScopesInParallelWithinTheBound(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	release := make(chan struct{})
	var releaseOnce sync.Once
	runner := func(_, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "version":
			return []byte("bd version 1.3.2 (x)\n"), nil
		case "config":
			if args[1] == "get" {
				return []byte(`{"value":""}`), nil
			}
			return nil, nil
		case "recompute-blocked":
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			reached := peak == blockedRepairParallelism
			mu.Unlock()
			if reached {
				releaseOnce.Do(func() { close(release) })
			}
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
			return []byte(`{"rows_corrected": 1}`), nil
		}
		return nil, errors.New("unexpected bd call")
	}
	var scopes []blockedRepairScope
	for i := 0; i < 2*blockedRepairParallelism; i++ {
		scopes = append(scopes, blockedRepairScope{id: fmt.Sprintf("rig/r%d", i), root: fmt.Sprintf("/r%d", i)})
	}
	rec := events.NewFake()
	var stderr bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBlockedRepair(scopes, blockedRepairDeps{
			openStore:    func(s blockedRepairScope) *beads.BdStore { return beads.NewBdStore(s.root, runner) },
			openRecorder: func() (events.Recorder, func()) { return rec, func() {} },
		}, &stderr, "gc start")
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("recomputes never reached the parallelism bound together")
	}
	if peak != blockedRepairParallelism {
		t.Fatalf("peak concurrent recomputes = %d, want %d", peak, blockedRepairParallelism)
	}
	out := stderr.String()
	if !strings.HasPrefix(out, fmt.Sprintf("gc start: repairing blocked flags in %d scope(s)", len(scopes))) {
		t.Fatalf("progress line missing or not first: %q", out)
	}
	if !strings.Contains(out, fmt.Sprintf("finished for %d of %d scope(s)", len(scopes), len(scopes))) || len(rec.Events) != len(scopes) {
		t.Fatalf("stderr=%q events=%d, want every scope repaired", out, len(rec.Events))
	}
	// Results print in scope order whatever order the recomputes finished in.
	if strings.Index(out, "rig/r0 ") > strings.Index(out, "rig/r7 ") {
		t.Fatalf("results not in scope order: %q", out)
	}
}
