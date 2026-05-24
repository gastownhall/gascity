package beads_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type hqErrorScenario struct {
	id       string
	name     string
	tested   bool
	asserted bool
	run      func(*testing.T)
}

func TestHQStoreErrorScenarios(t *testing.T) {
	if mode := os.Getenv("HQSTORE_ERROR_HELPER"); mode != "" {
		hqStoreErrorHelper(t, mode)
		return
	}

	for _, scenario := range hqErrorScenarios() {
		scenario := scenario
		t.Run(scenario.id+"_"+scenario.name, func(t *testing.T) {
			if !scenario.tested || !scenario.asserted {
				t.Fatalf("%s checklist incomplete: tested=%v asserted=%v", scenario.id, scenario.tested, scenario.asserted)
			}
			scenario.run(t)
		})
	}
}

func TestHQStoreErrorScenarioChecklist(t *testing.T) {
	seen := make(map[string]hqErrorScenario)
	for _, scenario := range hqErrorScenarios() {
		if scenario.id == "" {
			t.Fatalf("scenario with empty ID: %+v", scenario)
		}
		if _, ok := seen[scenario.id]; ok {
			t.Fatalf("duplicate scenario ID %s", scenario.id)
		}
		seen[scenario.id] = scenario
		if !scenario.tested || !scenario.asserted || scenario.run == nil {
			t.Fatalf("%s is not fully checked: tested=%v asserted=%v run nil=%v",
				scenario.id, scenario.tested, scenario.asserted, scenario.run == nil)
		}
	}
	for i := 1; i <= 25; i++ {
		id := fmt.Sprintf("E%d", i)
		if _, ok := seen[id]; !ok {
			t.Fatalf("missing error scenario %s", id)
		}
	}
}

func hqErrorScenarios() []hqErrorScenario {
	checked := func(id, name string, run func(*testing.T)) hqErrorScenario {
		return hqErrorScenario{id: id, name: name, tested: true, asserted: true, run: run}
	}
	return []hqErrorScenario{
		checked("E1", "sigkill_mid_snapshot", hqScenarioSIGKILLFlushedSnapshot),
		checked("E2", "sigkill_unsnapshotted_write", hqScenarioSIGKILLUnsnapshottedWrite),
		checked("E3", "duplicate_id", hqScenarioDuplicateID),
		checked("E4", "metadata_merge", hqScenarioMetadataMerge),
		checked("E5", "concurrent_same_key", hqScenarioConcurrentSameKey),
		checked("E6", "concurrent_create_delete", hqScenarioConcurrentCreateDelete),
		checked("E7", "snapshot_write_failure", hqScenarioSnapshotFailurePreservesOld),
		checked("E8", "empty_store", hqScenarioEmptyStore),
		checked("E9", "single_record", hqScenarioSingleRecord),
		checked("E10", "max_metadata_keys", hqScenarioMaxMetadataKeys),
		checked("E11", "ttl_exact_boundary", hqScenarioTTLExactBoundary),
		checked("E12", "ttl_read_then_expire", hqScenarioTTLReadThenExpire),
		checked("E13", "clean_restart", hqScenarioCleanRestart),
		checked("E14", "crash_bounded_loss", hqScenarioCrashBoundedLoss),
		checked("E15", "large_batch", hqScenarioLargeBatch),
		checked("E16", "circular_dependency", hqScenarioCircularDependency),
		checked("E17", "blocked_cascade", hqScenarioBlockedCascade),
		checked("E18", "delete_cascades_deps", hqScenarioDeleteCascadesDeps),
		checked("E19", "missing_snapshot", hqScenarioMissingSnapshot),
		checked("E20", "corrupt_snapshot", hqScenarioCorruptSnapshot),
		checked("E21", "concurrent_read_snapshot", hqScenarioConcurrentReadSnapshot),
		checked("E22", "close_already_closed", hqScenarioCloseAlreadyClosed),
		checked("E23", "update_nonexistent", hqScenarioUpdateNonexistent),
		checked("E24", "ephemeral_metadata_batch", hqScenarioEphemeralMetadataBatch),
		checked("E25", "zero_byte_snapshot", hqScenarioZeroByteSnapshot),
	}
}

func hqScenarioSIGKILLFlushedSnapshot(t *testing.T) {
	dir, ids := hqRunKillHelper(t, "flushed")
	recovered := openHQForScenario(t, dir)
	defer hqShutdown(t, recovered)
	got, err := recovered.Get(ids["durable"])
	if err != nil {
		t.Fatalf("Get durable after SIGKILL: %v", err)
	}
	if got.Title != "durable" {
		t.Fatalf("durable title = %q, want durable", got.Title)
	}
}

func hqScenarioSIGKILLUnsnapshottedWrite(t *testing.T) {
	dir, ids := hqRunKillHelper(t, "unsnapped")
	recovered := openHQForScenario(t, dir)
	defer hqShutdown(t, recovered)
	if _, err := recovered.Get(ids["volatile"]); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get volatile after unsnapped SIGKILL = %v, want ErrNotFound", err)
	}
}

func hqScenarioDuplicateID(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{ID: "same", Title: "original"})
	if _, err := store.Create(beads.Bead{ID: created.ID, Title: "dupe"}); err == nil {
		t.Fatal("duplicate Create returned nil error")
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get original: %v", err)
	}
	if got.Title != "original" {
		t.Fatalf("original title = %q, want original", got.Title)
	}
}

func hqScenarioMetadataMerge(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{Title: "metadata", Metadata: map[string]string{"keep": "yes"}})
	if err := store.SetMetadataBatch(created.ID, map[string]string{"add": "yes"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["keep"] != "yes" || got.Metadata["add"] != "yes" {
		t.Fatalf("metadata = %+v, want merged keys", got.Metadata)
	}
}

func hqScenarioConcurrentSameKey(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{Title: "race"})
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.SetMetadataBatch(created.ID, map[string]string{"winner": fmt.Sprint(i)})
		}(i)
	}
	wg.Wait()
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["winner"] == "" {
		t.Fatalf("winner metadata missing after concurrent writers: %+v", got.Metadata)
	}
}

func hqScenarioConcurrentCreateDelete(t *testing.T) {
	store := newHQScenarioStore(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = store.Create(beads.Bead{ID: "racy", Title: "racy"})
		}()
		go func() {
			defer wg.Done()
			_ = store.Delete("racy")
		}()
	}
	wg.Wait()
	items, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) > 1 {
		t.Fatalf("concurrent create/delete left %d records, want at most 1", len(items))
	}
}

func hqScenarioSnapshotFailurePreservesOld(t *testing.T) {
	dir := t.TempDir()
	store := openHQForScenario(t, dir)
	defer hqShutdown(t, store)
	durable := mustHQCreate(t, store, beads.Bead{Title: "old"})
	if err := store.Snapshot(); err != nil {
		t.Fatalf("initial Snapshot: %v", err)
	}
	volatile := mustHQCreate(t, store, beads.Bead{Title: "new"})
	tmpPath := filepath.Join(dir, "snapshot.jsonl.gz.tmp")
	if err := os.Mkdir(tmpPath, 0o755); err != nil {
		t.Fatalf("mkdir temp blocker: %v", err)
	}
	if err := store.Snapshot(); err == nil {
		t.Fatal("Snapshot with temp blocker returned nil error")
	}
	if err := os.Remove(tmpPath); err != nil {
		t.Fatalf("remove temp blocker: %v", err)
	}
	recovered := openHQForScenario(t, dir)
	defer hqShutdown(t, recovered)
	if _, err := recovered.Get(durable.ID); err != nil {
		t.Fatalf("Get durable from old snapshot: %v", err)
	}
	if _, err := recovered.Get(volatile.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get volatile from failed snapshot = %v, want ErrNotFound", err)
	}
}

func hqScenarioEmptyStore(t *testing.T) {
	store := newHQScenarioStore(t)
	if _, err := store.Get("missing"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	if got, err := store.List(beads.ListQuery{AllowScan: true}); err != nil || len(got) != 0 {
		t.Fatalf("List empty = (%d, %v), want (0, nil)", len(got), err)
	}
	if got, err := store.Ready(); err != nil || len(got) != 0 {
		t.Fatalf("Ready empty = (%d, %v), want (0, nil)", len(got), err)
	}
	if got, err := store.DepList("missing", "down"); err != nil || len(got) != 0 {
		t.Fatalf("DepList empty = (%d, %v), want (0, nil)", len(got), err)
	}
}

func hqScenarioSingleRecord(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{Title: "single", Assignee: "rig/agent-01"})
	if _, err := store.Get(created.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, err := store.List(beads.ListQuery{Assignee: "rig/agent-01"}); err != nil || len(got) != 1 {
		t.Fatalf("List single = (%d, %v), want (1, nil)", len(got), err)
	}
	if got, err := store.Ready(beads.ReadyQuery{Assignee: "rig/agent-01"}); err != nil || !hqContainsID(got, created.ID) {
		t.Fatalf("Ready single = (%+v, %v), want %s", got, err, created.ID)
	}
}

func hqScenarioMaxMetadataKeys(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{Title: "metadata"})
	kvs := make(map[string]string, 50)
	for i := range 50 {
		kvs[fmt.Sprintf("k%02d", i)] = fmt.Sprintf("v%02d", i)
	}
	if err := store.SetMetadataBatch(created.ID, kvs); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	got, err := store.ListByMetadata(map[string]string{"k49": "v49"}, 1)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("ListByMetadata returned %+v, want %s", got, created.ID)
	}
}

func hqScenarioTTLExactBoundary(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{
		Title:     "ttl-now",
		Ephemeral: true,
		Metadata:  map[string]string{"expires_at": time.Now().Format(time.RFC3339Nano)},
	})
	if n, err := store.PurgeExpired(); err != nil || n != 1 {
		t.Fatalf("PurgeExpired = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := store.Get(created.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get expired = %v, want ErrNotFound", err)
	}
}

func hqScenarioTTLReadThenExpire(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{
		Title:     "ttl-soon",
		Ephemeral: true,
		Metadata:  map[string]string{"expires_at": time.Now().Add(30 * time.Millisecond).Format(time.RFC3339Nano)},
	})
	if _, err := store.Get(created.ID); err != nil {
		t.Fatalf("initial Get: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := store.PurgeExpired(); err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if _, err := store.Get(created.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get after expiry = %v, want ErrNotFound", err)
	}
}

func hqScenarioCleanRestart(t *testing.T) {
	dir := t.TempDir()
	store := openHQForScenario(t, dir)
	created := mustHQCreate(t, store, beads.Bead{Title: "clean"})
	hqShutdown(t, store)
	recovered := openHQForScenario(t, dir)
	defer hqShutdown(t, recovered)
	if _, err := recovered.Get(created.ID); err != nil {
		t.Fatalf("Get after clean restart: %v", err)
	}
}

func hqScenarioCrashBoundedLoss(t *testing.T) {
	dir, ids := hqRunKillHelper(t, "mixed")
	recovered := openHQForScenario(t, dir)
	defer hqShutdown(t, recovered)
	if _, err := recovered.Get(ids["durable"]); err != nil {
		t.Fatalf("Get durable after crash: %v", err)
	}
	if _, err := recovered.Get(ids["volatile"]); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get volatile after crash = %v, want ErrNotFound", err)
	}
}

func hqScenarioLargeBatch(t *testing.T) {
	store := newHQScenarioStore(t)
	for i := range 10000 {
		assignee := "other"
		if i == 9999 {
			assignee = "needle"
		}
		mustHQCreate(t, store, beads.Bead{Title: fmt.Sprintf("batch-%d", i), Assignee: assignee})
	}
	start := time.Now()
	got, err := store.List(beads.ListQuery{Assignee: "needle"})
	if err != nil {
		t.Fatalf("List large batch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List large batch returned %d, want 1", len(got))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("selective large-batch scan took %s, want <=1s", elapsed)
	}
}

func hqScenarioCircularDependency(t *testing.T) {
	store := newHQScenarioStore(t)
	a := mustHQCreate(t, store, beads.Bead{Title: "A"})
	b := mustHQCreate(t, store, beads.Bead{Title: "B"})
	if err := store.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd A->B: %v", err)
	}
	if err := store.DepAdd(b.ID, a.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd B->A: %v", err)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if hqContainsID(ready, a.ID) || hqContainsID(ready, b.ID) {
		t.Fatalf("Ready included circularly blocked beads: %+v", ready)
	}
}

func hqScenarioBlockedCascade(t *testing.T) {
	store := newHQScenarioStore(t)
	a := mustHQCreate(t, store, beads.Bead{Title: "A"})
	b := mustHQCreate(t, store, beads.Bead{Title: "B"})
	c := mustHQCreate(t, store, beads.Bead{Title: "C"})
	if err := store.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd A->B: %v", err)
	}
	if err := store.DepAdd(b.ID, c.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd B->C: %v", err)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !hqContainsID(ready, c.ID) || hqContainsID(ready, a.ID) || hqContainsID(ready, b.ID) {
		t.Fatalf("Ready cascade = %+v, want C only from chain", ready)
	}
}

func hqScenarioDeleteCascadesDeps(t *testing.T) {
	store := newHQScenarioStore(t)
	a := mustHQCreate(t, store, beads.Bead{Title: "A"})
	b := mustHQCreate(t, store, beads.Bead{Title: "B"})
	if err := store.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if err := store.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if down, err := store.DepList(a.ID, "down"); err != nil || len(down) != 0 {
		t.Fatalf("DepList down after delete = (%+v, %v), want empty", down, err)
	}
	if up, err := store.DepList(b.ID, "up"); err != nil || len(up) != 0 {
		t.Fatalf("DepList up after delete = (%+v, %v), want empty", up, err)
	}
}

func hqScenarioMissingSnapshot(t *testing.T) {
	store := newHQScenarioStore(t)
	if got, err := store.List(beads.ListQuery{AllowScan: true}); err != nil || len(got) != 0 {
		t.Fatalf("fresh store List = (%d, %v), want empty", len(got), err)
	}
}

func hqScenarioCorruptSnapshot(t *testing.T) {
	hqAssertCorruptSnapshotDoesNotPanic(t, []byte("not gzip"))
}

func hqScenarioConcurrentReadSnapshot(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(), beads.WithHQStoreSnapshotInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	defer hqShutdown(t, store)
	for i := range 100 {
		mustHQCreate(t, store, beads.Bead{Title: fmt.Sprintf("item-%d", i), Assignee: "reader"})
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if _, err := store.List(beads.ListQuery{Assignee: "reader"}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for range 10 {
		if err := store.Snapshot(); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("reader error: %v", err)
		}
	}
}

func hqScenarioCloseAlreadyClosed(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{Title: "close twice"})
	if err := store.Close(created.ID); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if err := store.Close(created.ID); err != nil {
		t.Fatalf("Close second: %v", err)
	}
}

func hqScenarioUpdateNonexistent(t *testing.T) {
	store := newHQScenarioStore(t)
	status := "closed"
	if err := store.Update("missing", beads.UpdateOpts{Status: &status}); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Update missing = %v, want ErrNotFound", err)
	}
}

func hqScenarioEphemeralMetadataBatch(t *testing.T) {
	store := newHQScenarioStore(t)
	created := mustHQCreate(t, store, beads.Bead{Title: "wisp", Type: "message", Ephemeral: true})
	if err := store.SetMetadataBatch(created.ID, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("SetMetadataBatch ephemeral: %v", err)
	}
	got, err := store.ListByMetadata(map[string]string{"k": "v"}, 1, beads.WithEphemeral)
	if err != nil {
		t.Fatalf("ListByMetadata ephemeral: %v", err)
	}
	if len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("ListByMetadata ephemeral = %+v, want %s", got, created.ID)
	}
}

func hqScenarioZeroByteSnapshot(t *testing.T) {
	hqAssertCorruptSnapshotDoesNotPanic(t, nil)
}

func hqRunKillHelper(t *testing.T, mode string) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestHQStoreErrorScenarios")
	cmd.Env = append(os.Environ(),
		"HQSTORE_ERROR_HELPER="+mode,
		"HQSTORE_DIR="+dir,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	idsPath := filepath.Join(dir, "ids")
	ids := make(map[string]string)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(idsPath)
		if err == nil {
			for _, line := range splitNonEmptyLines(string(data)) {
				key, value, ok := stringsCut(line, "=")
				if ok {
					ids[key] = value
				}
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(ids) == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("helper did not write ids")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()
	return dir, ids
}

func hqStoreErrorHelper(t *testing.T, mode string) {
	t.Helper()
	dir := os.Getenv("HQSTORE_DIR")
	if dir == "" {
		t.Fatal("HQSTORE_DIR is required")
	}
	store, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("helper OpenHQStore: %v", err)
	}
	ids := make(map[string]string)
	switch mode {
	case "flushed":
		durable := mustHQCreate(t, store, beads.Bead{Title: "durable"})
		if err := store.Snapshot(); err != nil {
			t.Fatalf("helper Snapshot: %v", err)
		}
		ids["durable"] = durable.ID
	case "unsnapped":
		volatile := mustHQCreate(t, store, beads.Bead{Title: "volatile"})
		ids["volatile"] = volatile.ID
	case "mixed":
		durable := mustHQCreate(t, store, beads.Bead{Title: "durable"})
		if err := store.Snapshot(); err != nil {
			t.Fatalf("helper Snapshot: %v", err)
		}
		volatile := mustHQCreate(t, store, beads.Bead{Title: "volatile"})
		ids["durable"] = durable.ID
		ids["volatile"] = volatile.ID
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	if err := writeHQHelperIDs(filepath.Join(dir, "ids"), ids); err != nil {
		t.Fatalf("helper write ids: %v", err)
	}
	select {}
}

func writeHQHelperIDs(path string, ids map[string]string) error {
	var data string
	for k, v := range ids {
		data += k + "=" + v + "\n"
	}
	return os.WriteFile(path, []byte(data), 0o644)
}

func newHQScenarioStore(t *testing.T) *beads.HQStore {
	t.Helper()
	store := openHQForScenario(t, t.TempDir())
	t.Cleanup(func() { hqShutdown(t, store) })
	return store
}

func openHQForScenario(t *testing.T, dir string) *beads.HQStore {
	t.Helper()
	store, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	return store
}

func hqShutdown(t *testing.T, store *beads.HQStore) {
	t.Helper()
	if store == nil {
		return
	}
	if err := store.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func hqAssertCorruptSnapshotDoesNotPanic(t *testing.T, data []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snapshot.jsonl.gz"), data, 0o644); err != nil {
		t.Fatalf("write corrupt snapshot: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("OpenHQStore panicked on corrupt snapshot: %v", r)
		}
	}()
	store, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(0))
	if err == nil {
		hqShutdown(t, store)
		return
	}
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func stringsCut(s, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func FuzzSnapshotRecovery(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("not gzip"))
	f.Add([]byte{0x1f, 0x8b, 0x08, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		hqAssertCorruptSnapshotDoesNotPanic(t, data)
	})
}
