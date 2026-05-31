package beads_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestHQStoreProductionPatterns(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(), beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	t.Run("P1 mail send", func(t *testing.T) {
		msg, err := store.Create(beads.Bead{
			Title:     "mail",
			Type:      "message",
			Assignee:  "rig/agent-01",
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("Create mail: %v", err)
		}
		if err := store.SetMetadata(msg.ID, "gc.mail.kind", "handoff"); err != nil {
			t.Fatalf("SetMetadata mail: %v", err)
		}
	})

	t.Run("P2 mail poll", func(t *testing.T) {
		got, err := store.List(beads.ListQuery{
			Type:      "message",
			Status:    "open",
			Assignee:  "rig/agent-01",
			TierMode:  beads.TierWisps,
			AllowScan: true,
		})
		if err != nil {
			t.Fatalf("List mail: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("List mail returned no messages")
		}
	})

	t.Run("P3 mail read", func(t *testing.T) {
		msgs, err := store.ListByAssignee("rig/agent-01", "open", 1)
		if err != nil {
			t.Fatalf("ListByAssignee: %v", err)
		}
		if len(msgs) != 0 {
			t.Fatalf("main-tier ListByAssignee returned wisps: %+v", msgs)
		}
		wisps, err := store.ListByMetadata(map[string]string{"gc.mail.kind": "handoff"}, 1, beads.WithEphemeral)
		if err != nil {
			t.Fatalf("ListByMetadata wisps: %v", err)
		}
		if len(wisps) != 1 {
			t.Fatalf("ListByMetadata wisps returned %d, want 1", len(wisps))
		}
		if _, err := store.Get(wisps[0].ID); err != nil {
			t.Fatalf("Get mail: %v", err)
		}
		if err := store.Update(wisps[0].ID, beads.UpdateOpts{Labels: []string{"read"}}); err != nil {
			t.Fatalf("Update read label: %v", err)
		}
	})

	t.Run("P4 mail archive", func(t *testing.T) {
		wisps, err := store.ListByLabel("read", 1, beads.WithEphemeral)
		if err != nil {
			t.Fatalf("ListByLabel read wisps: %v", err)
		}
		if len(wisps) != 1 {
			t.Fatalf("ListByLabel read wisps returned %d, want 1", len(wisps))
		}
		if err := store.Close(wisps[0].ID); err != nil {
			t.Fatalf("Close mail: %v", err)
		}
	})

	session := mustHQCreate(t, store, beads.Bead{
		Title: "session",
		Type:  "session",
		Metadata: map[string]string{
			"gc.session_state": "creating",
		},
	})
	t.Run("P5 session create", func(t *testing.T) {
		got, err := store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get session: %v", err)
		}
		if got.Type != "session" {
			t.Fatalf("session type = %q, want session", got.Type)
		}
	})

	t.Run("P6 session state transition", func(t *testing.T) {
		if err := store.SetMetadataBatch(session.ID, map[string]string{
			"gc.session_state":  "running",
			"gc.session_pid":    "12345",
			"gc.session_pane":   "%1",
			"gc.last_heartbeat": time.Now().Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatalf("SetMetadataBatch session: %v", err)
		}
	})

	t.Run("P7 session close drain", func(t *testing.T) {
		if err := store.Close(session.ID); err != nil {
			t.Fatalf("Close session: %v", err)
		}
		if err := store.SetMetadataBatch(session.ID, map[string]string{"gc.drain_reason": "idle"}); err != nil {
			t.Fatalf("SetMetadataBatch closed session: %v", err)
		}
	})

	root := mustHQCreate(t, store, beads.Bead{Title: "mol", Type: "molecule"})
	step := mustHQCreate(t, store, beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
	t.Run("P8 molecule instantiation", func(t *testing.T) {
		children, err := store.Children(root.ID)
		if err != nil {
			t.Fatalf("Children: %v", err)
		}
		if len(children) != 1 || children[0].ID != step.ID {
			t.Fatalf("Children = %+v, want step %s", children, step.ID)
		}
		if err := store.SetMetadata(root.ID, "molecule_id", root.ID); err != nil {
			t.Fatalf("SetMetadata molecule: %v", err)
		}
	})

	t.Run("P9 molecule step advance", func(t *testing.T) {
		if _, err := store.Get(step.ID); err != nil {
			t.Fatalf("Get step: %v", err)
		}
		status := "in_progress"
		if err := store.Update(step.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update step: %v", err)
		}
		if err := store.SetMetadata(step.ID, "gc.step", "green"); err != nil {
			t.Fatalf("SetMetadata step: %v", err)
		}
	})

	t.Run("P10 work dispatch sling", func(t *testing.T) {
		convoy := mustHQCreate(t, store, beads.Bead{Title: "convoy", Type: "convoy"})
		if err := store.SetMetadataBatch(convoy.ID, map[string]string{
			"gc.routed_to": "rig/agent-02",
			"workflow_id":  "wf-1",
		}); err != nil {
			t.Fatalf("SetMetadataBatch convoy: %v", err)
		}
		if _, err := store.Get(convoy.ID); err != nil {
			t.Fatalf("Get convoy: %v", err)
		}
		status := "in_progress"
		if err := store.Update(convoy.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update convoy: %v", err)
		}
	})

	t.Run("P11 ready query", func(t *testing.T) {
		work := mustHQCreate(t, store, beads.Bead{Title: "ready work", Assignee: "rig/agent-02"})
		ready, err := store.Ready(beads.ReadyQuery{Assignee: "rig/agent-02"})
		if err != nil {
			t.Fatalf("Ready: %v", err)
		}
		if !hqContainsID(ready, work.ID) {
			t.Fatalf("Ready did not include %s: %+v", work.ID, ready)
		}
	})

	t.Run("P12 order tracking wisp lifecycle", func(t *testing.T) {
		order := mustHQCreate(t, store, beads.Bead{
			Title:     "order",
			Type:      "order-tracking",
			Ephemeral: true,
			Metadata: map[string]string{
				"expires_at": time.Now().Add(-time.Second).Format(time.RFC3339Nano),
			},
		})
		if err := store.Close(order.ID); err != nil {
			t.Fatalf("Close order tracking: %v", err)
		}
		purged, err := store.PurgeExpired()
		if err != nil {
			t.Fatalf("PurgeExpired: %v", err)
		}
		if purged == 0 {
			t.Fatal("PurgeExpired purged 0 order-tracking wisps")
		}
	})
}

func TestHQStoreCoverageMatrix(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(0), beads.WithHQStoreClosedTaskRetention(time.Hour))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	hqExerciseAllCounters(t, dir, store)
	if err := store.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	var out bytes.Buffer
	zeros := hqWriteCoverageMatrix(&out, store.Counters())
	t.Log("\n" + out.String())
	if len(zeros) > 0 {
		t.Fatalf("HQStore coverage matrix has zero rows: %s", strings.Join(zeros, ", "))
	}
}

func TestHQStoreOptionAndBranchCoverage(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(),
		beads.WithHQStoreIDPrefix("custom"),
		beads.WithHQStoreMailRetentionTTL(2*time.Hour),
		beads.WithHQStoreTTLInterval(10*time.Millisecond),
		beads.WithHQStoreSnapshotInterval(0),
	)
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	if got := store.MailRetentionTTL(); got != 2*time.Hour {
		t.Fatalf("MailRetentionTTL() = %v, want 2h", got)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	parent := mustHQCreate(t, store, beads.Bead{Title: "parent"})
	target := mustHQCreate(t, store, beads.Bead{Title: "target"})
	needs := mustHQCreate(t, store, beads.Bead{
		Title: "needs",
		Needs: []string{"waits-for:" + target.ID, parent.ID},
	})
	if !strings.HasPrefix(needs.ID, "custom-") {
		t.Fatalf("generated ID = %q, want custom-*", needs.ID)
	}
	deps, err := store.DepList(needs.ID, "down")
	if err != nil {
		t.Fatalf("DepList needs: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("needs deps = %+v, want 2", deps)
	}

	title := "updated"
	status := "in_progress"
	desc := "description"
	priority := 1
	assignee := "rig/agent-03"
	typ := "feature"
	if err := store.Update(needs.ID, beads.UpdateOpts{
		Title:        &title,
		Status:       &status,
		Description:  &desc,
		Priority:     &priority,
		ParentID:     &parent.ID,
		Assignee:     &assignee,
		Type:         &typ,
		Labels:       []string{"keep", "drop"},
		RemoveLabels: []string{"drop"},
		Metadata:     map[string]string{"branch": "covered"},
	}); err != nil {
		t.Fatalf("Update all fields: %v", err)
	}
	got, err := store.Get(needs.ID)
	if err != nil {
		t.Fatalf("Get updated: %v", err)
	}
	if got.Title != title || got.Status != status || got.Description != desc ||
		got.Priority == nil || *got.Priority != priority || got.ParentID != parent.ID ||
		got.Assignee != assignee || got.Type != typ || got.Metadata["branch"] != "covered" ||
		!slicesEqual(got.Labels, []string{"keep"}) {
		t.Fatalf("updated bead mismatch: %+v", got)
	}

	if err := store.Close(needs.ID); err != nil {
		t.Fatalf("Close needs: %v", err)
	}
	if err := store.Reopen(needs.ID); err != nil {
		t.Fatalf("Reopen needs: %v", err)
	}
	reopened, err := store.Get(needs.ID)
	if err != nil {
		t.Fatalf("Get reopened: %v", err)
	}
	if _, ok := reopened.Metadata["gc.hqstore.closed_at"]; ok {
		t.Fatalf("closed_at metadata survived reopen: %+v", reopened.Metadata)
	}

	if err := store.DepAdd(needs.ID, target.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd blocks: %v", err)
	}
	if err := store.DepAdd(needs.ID, target.ID, "conditional-blocks"); err != nil {
		t.Fatalf("DepAdd replacement: %v", err)
	}
	if err := store.DepAdd(needs.ID, target.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd parent-child: %v", err)
	}
	deps, err = store.DepList(needs.ID, "down")
	if err != nil {
		t.Fatalf("DepList after replacements: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("deps after replacement and parent-child = %+v, want 3", deps)
	}

	wisp := mustHQCreate(t, store, beads.Bead{
		Title:     "wisp",
		Assignee:  assignee,
		Ephemeral: true,
	})
	both, err := store.List(beads.ListQuery{Assignee: assignee, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List TierBoth: %v", err)
	}
	if !hqContainsID(both, needs.ID) || !hqContainsID(both, wisp.ID) {
		t.Fatalf("TierBoth results = %+v, want main %s and wisp %s", both, needs.ID, wisp.ID)
	}

	expiring := mustHQCreate(t, store, beads.Bead{
		Title:     "background ttl",
		Ephemeral: true,
		Metadata:  map[string]string{"gc.expires_at": time.Now().Add(-time.Second).Format(time.RFC3339Nano)},
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.Get(expiring.ID); errors.Is(err, beads.ErrNotFound) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background TTL did not purge %s", expiring.ID)
}

func TestHQStoreBackgroundSnapshotErrorIsObservable(t *testing.T) {
	dir := t.TempDir()
	restoreSnapshotDir := makeHQSnapshotDirReadOnly(t, dir)
	store, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	mustHQCreate(t, store, beads.Bead{Title: "snap-error"})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := store.LastSnapshotErr(); err != nil {
			restoreSnapshotDir()
			if shutdownErr := store.Shutdown(); shutdownErr != nil {
				t.Fatalf("Shutdown: %v", shutdownErr)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	restoreSnapshotDir()
	_ = store.Shutdown()
	t.Fatal("background snapshot error was not recorded")
}

func hqExerciseAllCounters(t *testing.T, dir string, store *beads.HQStore) {
	t.Helper()
	if err := store.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	main := mustHQCreate(t, store, beads.Bead{
		ID:       "explicit-main",
		Title:    "main",
		Assignee: "rig/agent-01",
		Labels:   []string{"alpha"},
		Metadata: map[string]string{"scope": "matrix"},
	})
	if _, err := store.Create(beads.Bead{ID: main.ID, Title: "dupe"}); err == nil {
		t.Fatal("duplicate Create returned nil error")
	}
	if _, err := store.Get(main.ID); err != nil {
		t.Fatalf("Get main: %v", err)
	}
	if _, err := store.Get("missing"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}
	status := "in_progress"
	if err := store.Update(main.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update main: %v", err)
	}
	if err := store.Update("missing", beads.UpdateOpts{Status: &status}); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Update missing error = %v, want ErrNotFound", err)
	}
	if err := store.SetMetadata(main.ID, "one", "1"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := store.SetMetadataBatch(main.ID, map[string]string{"two": "2"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if _, err := store.List(beads.ListQuery{Assignee: "rig/agent-01"}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := store.ListOpen("in_progress"); err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if _, err := store.ListByLabel("alpha", 10); err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if _, err := store.ListByAssignee("rig/agent-01", "in_progress", 10); err != nil {
		t.Fatalf("ListByAssignee: %v", err)
	}
	if _, err := store.ListByMetadata(map[string]string{"scope": "matrix"}, 10); err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	child := mustHQCreate(t, store, beads.Bead{Title: "child", ParentID: main.ID})
	if _, err := store.Children(main.ID); err != nil {
		t.Fatalf("Children: %v", err)
	}
	if err := store.DepAdd(child.ID, main.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if _, err := store.DepList(child.ID, "down"); err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if err := store.DepRemove(child.ID, main.ID); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}
	readyTask := mustHQCreate(t, store, beads.Bead{Title: "ready", Assignee: "rig/agent-01"})
	if ready, err := store.Ready(beads.ReadyQuery{Assignee: "rig/agent-01"}); err != nil {
		t.Fatalf("Ready: %v", err)
	} else if !hqContainsID(ready, readyTask.ID) {
		t.Fatalf("Ready missing %s: %+v", readyTask.ID, ready)
	}
	if err := store.Close(main.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close("missing"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Close missing error = %v, want ErrNotFound", err)
	}
	if err := store.Reopen(main.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	closeA := mustHQCreate(t, store, beads.Bead{Title: "close all A"})
	closeB := mustHQCreate(t, store, beads.Bead{Title: "close all B"})
	if n, err := store.CloseAll([]string{closeA.ID, closeB.ID}, map[string]string{"closed_by": "matrix"}); err != nil || n != 2 {
		t.Fatalf("CloseAll = (%d, %v), want (2, nil)", n, err)
	}
	deleteMe := mustHQCreate(t, store, beads.Bead{Title: "delete"})
	if err := store.Delete(deleteMe.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete("missing"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Delete missing error = %v, want ErrNotFound", err)
	}
	expired := mustHQCreate(t, store, beads.Bead{
		Title:     "expired",
		Ephemeral: true,
		Metadata: map[string]string{
			"expires_at": time.Now().Add(-time.Second).Format(time.RFC3339Nano),
		},
	})
	if purged, err := store.PurgeExpired(); err != nil || purged == 0 {
		t.Fatalf("PurgeExpired after %s = (%d, %v), want at least one purge", expired.ID, purged, err)
	}
	if err := store.Tx("matrix", func(tx beads.Tx) error {
		txStatus := "closed"
		return tx.Update(readyTask.ID, beads.UpdateOpts{Status: &txStatus})
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if err := store.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	restoreSnapshotDir := makeHQSnapshotDirReadOnly(t, dir)
	if err := store.Snapshot(); err == nil {
		t.Fatal("Snapshot with read-only snapshot dir returned nil error")
	}
	restoreSnapshotDir()
}

type hqCounterRow struct {
	name  string
	calls int64
}

func hqCounterRows(c *beads.EntryCounters) []hqCounterRow {
	return []hqCounterRow{
		{"Create", c.Create.Load()},
		{"Get", c.Get.Load()},
		{"Update", c.Update.Load()},
		{"Close", c.Close.Load()},
		{"Reopen", c.Reopen.Load()},
		{"CloseAll", c.CloseAll.Load()},
		{"List", c.List.Load()},
		{"ListOpen", c.ListOpen.Load()},
		{"Ready", c.Ready.Load()},
		{"Children", c.Children.Load()},
		{"ListByLabel", c.ListByLabel.Load()},
		{"ListByAssignee", c.ListByAssignee.Load()},
		{"ListByMetadata", c.ListByMetadata.Load()},
		{"SetMetadata", c.SetMetadata.Load()},
		{"SetMetadataBatch", c.SetMetadataBatch.Load()},
		{"Delete", c.Delete.Load()},
		{"DepAdd", c.DepAdd.Load()},
		{"DepRemove", c.DepRemove.Load()},
		{"DepList", c.DepList.Load()},
		{"PurgeExpired", c.PurgeExpired.Load()},
		{"Ping", c.Ping.Load()},
		{"Tx", c.Tx.Load()},
		{"Snapshot", c.Snapshot.Load()},
		{"Shutdown", c.Shutdown.Load()},
		{"DuplicateCreate", c.DuplicateCreate.Load()},
		{"GetNotFound", c.GetNotFound.Load()},
		{"UpdateNotFound", c.UpdateNotFound.Load()},
		{"CloseNotFound", c.CloseNotFound.Load()},
		{"DeleteNotFound", c.DeleteNotFound.Load()},
		{"SnapshotWriteErr", c.SnapshotWriteErr.Load()},
		{"PurgeExpiredN", c.PurgeExpiredN.Load()},
	}
}

func hqWriteCoverageMatrix(out *bytes.Buffer, c *beads.EntryCounters) []string {
	var zeros []string
	fmt.Fprintln(out, "=== HQStore Coverage Matrix ===")
	fmt.Fprintln(out, "Counter             | Calls")
	fmt.Fprintln(out, "--------------------|------")
	for _, row := range hqCounterRows(c) {
		fmt.Fprintf(out, "%-19s | %5d\n", row.name, row.calls)
		if row.calls == 0 {
			zeros = append(zeros, row.name)
		}
	}
	if len(zeros) == 0 {
		fmt.Fprintln(out, "[ZERO COUNTERS]     | -- NONE --")
	}
	return zeros
}

func mustHQCreate(t *testing.T, store *beads.HQStore, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create(%q): %v", b.Title, err)
	}
	return created
}

func hqContainsID(beads []beads.Bead, id string) bool {
	for _, bead := range beads {
		if bead.ID == id {
			return true
		}
	}
	return false
}

func slicesEqual(a, b []string) bool {
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
