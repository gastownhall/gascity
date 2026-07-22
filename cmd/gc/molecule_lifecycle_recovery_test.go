package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestPrepareMoleculeLifecycleIntentPersistsMarkerLast(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "durable lifecycle", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &lifecycleWriteOrderStore{Store: base}
	requestedAt := time.Date(2026, 7, 16, 10, 11, 12, 13, time.UTC)

	before, intent, err := prepareMoleculeLifecycleIntent(store, root.ID, moleculeAutocloseReason, "controller", requestedAt)
	if err != nil {
		t.Fatalf("prepareMoleculeLifecycleIntent: %v", err)
	}
	if before.ID != root.ID || before.Status != "open" {
		t.Fatalf("before = %+v, want live open root %q", before, root.ID)
	}
	if intent.IntentID == "" || intent.FromStatus != "open" || intent.Actor != "controller" || !intent.RequestedAt.Equal(requestedAt) {
		t.Fatalf("intent = %+v, want populated v1 intent", intent)
	}
	if got, want := store.calls(), []string{"batch:close_reason,intent", "set:pending=v1"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}

	durable, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if durable.Metadata["close_reason"] != moleculeAutocloseReason {
		t.Fatalf("close_reason = %q, want %q", durable.Metadata["close_reason"], moleculeAutocloseReason)
	}
	if durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("pending marker = %q, want v1", durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey])
	}
	decoded, err := decodeMoleculeLifecycleIntent(durable.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil {
		t.Fatalf("decode durable intent: %v", err)
	}
	if decoded.IntentID != intent.IntentID {
		t.Fatalf("durable intent_id = %q, want %q", decoded.IntentID, intent.IntentID)
	}
}

func TestPrepareMoleculeLifecycleIntentAbortsBeforeMarkerOnPartialBatchFailure(t *testing.T) {
	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "partial prepare", Type: "molecule"})
	store := &lifecyclePartialPrepareStore{Store: base}

	if _, _, err := prepareMoleculeLifecycleIntent(store, root.ID, moleculeAutocloseReason, "controller", time.Now().UTC()); err == nil {
		t.Fatal("prepareMoleculeLifecycleIntent error = nil, want partial batch failure")
	}
	durable, _ := base.Get(root.ID)
	if got := durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]; got != "" {
		t.Fatalf("pending marker = %q after failed batch, want absent", got)
	}
	if durable.Status != "open" {
		t.Fatalf("status = %q, want open", durable.Status)
	}
}

func TestPrepareMoleculeLifecycleIntentDoesNotOverwritePendingOpenRow(t *testing.T) {
	base := beads.NewMemStore()
	root, existing := seedPendingOpenMolecule(t, base, moleculeAutocloseReason, time.Now().UTC())

	before, _, err := prepareMoleculeLifecycleIntent(base, root.ID, moleculeSourceAutocloseReason, "replacement", time.Now().UTC())
	if !errors.Is(err, errMoleculeLifecyclePending) {
		t.Fatalf("prepare error = %v, want errMoleculeLifecyclePending", err)
	}
	if before.ID != root.ID || before.Status != "open" {
		t.Fatalf("before = %+v, want pending open row", before)
	}
	durable, _ := base.Get(root.ID)
	got, err := decodeMoleculeLifecycleIntent(durable.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil {
		t.Fatalf("decode retained intent: %v", err)
	}
	if got.IntentID != existing.IntentID || durable.Metadata["close_reason"] != moleculeAutocloseReason {
		t.Fatalf("pending state overwritten: intent=%+v metadata=%v", got, durable.Metadata)
	}
}

func TestDecodeMoleculeLifecycleIntentStrictValidation(t *testing.T) {
	valid := `{"version":"v1","intent_id":"0123456789abcdef0123456789abcdef","from_status":"open","actor":"controller","requested_at":"2026-07-16T10:11:12Z","close_reason":"` + moleculeAutocloseReason + `"}`
	if _, err := decodeMoleculeLifecycleIntent(valid); err != nil {
		t.Fatalf("valid intent rejected: %v", err)
	}
	replaceField := func(old, replacement string) string {
		return strings.Replace(valid, old, replacement, 1)
	}

	tests := map[string]string{
		"unknown version":     replaceField(`"version":"v1"`, `"version":"v2"`),
		"unknown field":       valid[:len(valid)-1] + `,"surprise":true}`,
		"short intent id":     replaceField(`0123456789abcdef0123456789abcdef`, `0123`),
		"uppercase intent id": replaceField(`0123456789abcdef0123456789abcdef`, `0123456789ABCDEF0123456789ABCDEF`),
		"closed from status":  replaceField(`"from_status":"open"`, `"from_status":"closed"`),
		"blank actor":         replaceField(`"actor":"controller"`, `"actor":" "`),
		"non UTC timestamp":   replaceField(`2026-07-16T10:11:12Z`, `2026-07-16T12:11:12+02:00`),
		"unknown reason":      replaceField(moleculeAutocloseReason, `manual close`),
		"trailing document":   valid + `{}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeMoleculeLifecycleIntent(raw); err == nil {
				t.Fatalf("decodeMoleculeLifecycleIntent(%s) error = nil", raw)
			}
		})
	}
}

func TestRecoverMoleculeLifecycleIntentsPublishesOrderedEventsAndClearsIntent(t *testing.T) {
	store := beads.NewMemStore()
	requestedAt := time.Date(2026, 7, 16, 11, 12, 13, 0, time.UTC)
	root, intent := seedPendingClosedMolecule(t, store, false, moleculeAutocloseReason, requestedAt)
	rec := &lifecycleRecorder{}

	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("recoverMoleculeLifecycleIntents retry = true, want complete")
	}
	events := rec.snapshot()
	if got := lifecycleEventTypes(events); fmt.Sprint(got) != fmt.Sprint([]string{eventsPkgBeadClosed, eventsPkgMoleculeResolved}) {
		t.Fatalf("event types = %v, want bead.closed then molecule.resolved", got)
	}
	assertRecoveredLifecyclePayloads(t, events, root, intent)

	durable, _ := store.Get(root.ID)
	if got := durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]; got != "" {
		t.Fatalf("pending marker after recovery = %q, want cleared", got)
	}
	if got := durable.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey]; got != "" {
		t.Fatalf("intent after recovery = %q, want cleared", got)
	}
	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("second recovery retry = true, want no-op complete")
	}
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("events after idempotent second recovery = %d, want 2", got)
	}
}

func TestRecoverMoleculeLifecycleIntentsRetainsOwnershipUntilDurablePublication(t *testing.T) {
	store := beads.NewMemStore()
	root, intent := seedPendingClosedMolecule(t, store, false, moleculeSourceAutocloseReason, time.Now().UTC())
	spec, err := store.Create(beads.Bead{
		Title: "generated workflow spec",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create spec: %v", err)
	}
	rec := &failOnceDurableLifecycleRecorder{failures: 1}

	if retry := recoverMoleculeLifecycleIntents(store, rec); !retry {
		t.Fatal("first recovery retry = false, want retry after durable publication failure")
	}
	afterFailure, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root after publication failure: %v", err)
	}
	retainedIntent, err := decodeMoleculeLifecycleIntent(afterFailure.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil {
		t.Fatalf("decode retained intent: %v", err)
	}
	if afterFailure.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 || retainedIntent.IntentID != intent.IntentID {
		t.Fatalf("publication ownership after failure = marker:%q intent:%q, want v1/%q", afterFailure.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey], retainedIntent.IntentID, intent.IntentID)
	}
	afterFailedSpec, err := store.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get spec after publication failure: %v", err)
	}
	if afterFailedSpec.Status != "open" {
		t.Fatalf("spec status after publication failure = %q, want open", afterFailedSpec.Status)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("events after failed durable publication = %d, want zero", got)
	}

	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("second recovery retry = true, want durable publication complete")
	}
	got := rec.snapshot()
	if gotTypes := lifecycleEventTypes(got); fmt.Sprint(gotTypes) != fmt.Sprint([]string{events.BeadClosed, events.MoleculeResolved}) {
		t.Fatalf("event types = %v, want bead.closed then molecule.resolved", gotTypes)
	}
	afterSuccess, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root after publication success: %v", err)
	}
	if marker, rawIntent := afterSuccess.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey], afterSuccess.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey]; marker != "" || rawIntent != "" {
		t.Fatalf("publication ownership after success = marker:%q intent:%q, want cleared", marker, rawIntent)
	}
	afterSuccessfulSpec, err := store.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get spec after publication success: %v", err)
	}
	if afterSuccessfulSpec.Status != "closed" || afterSuccessfulSpec.Metadata["close_reason"] != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("spec after publication success = %+v, want closed sidecar", afterSuccessfulSpec)
	}
}

func TestRecoverMoleculeLifecycleIntentsRetriesPartialDurableBatchAtLeastOnce(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := seedPendingClosedMolecule(t, store, false, moleculeAutocloseReason, time.Now().UTC())
	rec := &partialFailOnceDurableLifecycleRecorder{}

	if retry := recoverMoleculeLifecycleIntents(store, rec); !retry {
		t.Fatal("first recovery retry = false, want retry after partial durable publication")
	}
	if got := lifecycleEventTypes(rec.snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{events.BeadClosed}) {
		t.Fatalf("events after partial publication = %v, want first bead.closed only", got)
	}
	retained, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get retained root: %v", err)
	}
	if retained.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 || retained.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] == "" {
		t.Fatalf("partial publication ownership = marker:%q intent:%q, want retained", retained.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey], retained.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	}

	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("second recovery retry = true, want durable publication complete")
	}
	if got := lifecycleEventTypes(rec.snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{
		events.BeadClosed, events.BeadClosed, events.MoleculeResolved,
	}) {
		t.Fatalf("events after retry = %v, want at-least-once closed then ordered lifecycle pair", got)
	}
	cleared, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get cleared root: %v", err)
	}
	if cleared.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" || cleared.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
		t.Fatalf("publication ownership after retry = marker:%q intent:%q, want cleared", cleared.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey], cleared.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	}
}

func TestRecoverMoleculeLifecycleIntentsRepairsTornFileRecorderTail(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := seedPendingClosedMolecule(t, store, false, moleculeAutocloseReason, time.Now().UTC())
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := events.NewFileRecorder(path, t.Output())
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup
	if err := rec.RecordDurably(events.Event{Type: events.BeadCreated, Subject: "seed"}); err != nil {
		t.Fatalf("seed RecordDurably: %v", err)
	}
	torn, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open torn writer: %v", err)
	}
	if _, err := torn.WriteString(`{"seq":2,"type":"bead.closed"`); err != nil {
		_ = torn.Close()
		t.Fatalf("append torn record: %v", err)
	}
	if err := torn.Close(); err != nil {
		t.Fatalf("close torn writer: %v", err)
	}

	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("recovery retry = true, want repaired durable lifecycle publication")
	}
	got, err := events.ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if gotTypes := lifecycleEventTypes(got); fmt.Sprint(gotTypes) != fmt.Sprint([]string{
		events.BeadCreated, events.BeadClosed, events.MoleculeResolved,
	}) {
		t.Fatalf("events = %v, want seed then readable bead.closed and molecule.resolved", gotTypes)
	}
	if got[0].Seq+1 != got[1].Seq || got[1].Seq+1 != got[2].Seq {
		t.Fatalf("event seqs = [%d %d %d], want consecutive", got[0].Seq, got[1].Seq, got[2].Seq)
	}
	cleared, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get cleared root: %v", err)
	}
	if cleared.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" || cleared.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
		t.Fatalf("publication ownership after repaired retry = marker:%q intent:%q, want cleared", cleared.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey], cleared.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	}
}

func TestRecoverMoleculeLifecycleIntentsRejectsBestEffortDiscard(t *testing.T) {
	store := beads.NewMemStore()
	root, intent := seedPendingClosedMolecule(t, store, false, moleculeSourceAutocloseReason, time.Now().UTC())
	spec, err := store.Create(beads.Bead{
		Title: "generated workflow spec",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create spec: %v", err)
	}

	if retry := recoverMoleculeLifecycleIntents(store, events.Discard); !retry {
		t.Fatal("recovery retry = false, want retry for best-effort discard recorder")
	}
	retained, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get retained root: %v", err)
	}
	retainedIntent, err := decodeMoleculeLifecycleIntent(retained.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil {
		t.Fatalf("decode retained intent: %v", err)
	}
	if retained.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 || retainedIntent.IntentID != intent.IntentID {
		t.Fatalf("discarded publication ownership = marker:%q intent:%q, want v1/%q", retained.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey], retainedIntent.IntentID, intent.IntentID)
	}
	retainedSpec, err := store.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get retained spec: %v", err)
	}
	if retainedSpec.Status != "open" {
		t.Fatalf("spec status after discarded publication = %q, want open", retainedSpec.Status)
	}
}

func TestRecoverMoleculeLifecycleIntentsUsesLiveAuthority(t *testing.T) {
	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "stale cached root", Type: "molecule"})
	store := &lifecycleExplicitHandlesStore{Store: base, stale: root}
	requestedAt := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	intent, err := newMoleculeLifecycleIntent("open", "controller", moleculeAutocloseReason, requestedAt)
	if err != nil {
		t.Fatalf("new intent: %v", err)
	}
	raw, _ := marshalMoleculeLifecycleIntent(intent)
	if err := base.SetMetadataBatch(root.ID, map[string]string{
		"close_reason": moleculeAutocloseReason,
		beadmeta.MoleculeLifecycleIntentMetadataKey:  raw,
		beadmeta.MoleculeLifecyclePendingMetadataKey: moleculeLifecycleVersionV1,
	}); err != nil {
		t.Fatalf("seed pending metadata: %v", err)
	}
	if err := base.Close(root.ID); err != nil {
		t.Fatalf("out-of-band close: %v", err)
	}
	if cached, err := store.Handles().Cached.Get(root.ID); err != nil || cached.Status != "open" {
		t.Fatalf("cached setup row = %+v, err=%v, want stale open", cached, err)
	}
	rec := &lifecycleRecorder{}

	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("recovery retry = true, want live recovery complete")
	}
	if got := lifecycleEventTypes(rec.snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{eventsPkgBeadClosed, eventsPkgMoleculeResolved}) {
		t.Fatalf("event types = %v, want recovered lifecycle", got)
	}
}

func TestRecoverMoleculeLifecycleIntentsScansBothTiers(t *testing.T) {
	store := beads.NewMemStore()
	seedPendingClosedMolecule(t, store, false, moleculeAutocloseReason, time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC))
	seedPendingClosedMolecule(t, store, true, moleculeSourceAutocloseReason, time.Date(2026, 7, 16, 13, 1, 0, 0, time.UTC))
	rec := &lifecycleRecorder{}

	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("recovery retry = true, want complete")
	}
	if got := lifecycleEventTypes(rec.snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{
		eventsPkgBeadClosed, eventsPkgMoleculeResolved, eventsPkgBeadClosed, eventsPkgMoleculeResolved,
	}) {
		t.Fatalf("event types = %v, want two ordered lifecycle pairs", got)
	}
}

func TestRecoverMoleculeLifecycleIntentsRetainsUntrustedCandidates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(beads.Store, string)
	}{
		{name: "unknown intent version", mutate: func(store beads.Store, id string) {
			_ = store.SetMetadata(id, beadmeta.MoleculeLifecycleIntentMetadataKey, `{"version":"v2","intent_id":"0123456789abcdef0123456789abcdef"}`)
		}},
		{name: "malformed intent", mutate: func(store beads.Store, id string) {
			_ = store.SetMetadata(id, beadmeta.MoleculeLifecycleIntentMetadataKey, `{`)
		}},
		{name: "reason mismatch", mutate: func(store beads.Store, id string) {
			_ = store.SetMetadata(id, "close_reason", moleculeSourceAutocloseReason)
		}},
		{name: "open row", mutate: func(store beads.Store, id string) {
			_ = store.Reopen(id)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			root, _ := seedPendingClosedMolecule(t, store, false, moleculeAutocloseReason, time.Now().UTC())
			tc.mutate(store, root.ID)
			rec := &lifecycleRecorder{}
			_ = recoverMoleculeLifecycleIntents(store, rec)
			if got := len(rec.snapshot()); got != 0 {
				t.Fatalf("events = %d, want zero for untrusted candidate", got)
			}
			durable, _ := store.Get(root.ID)
			if got := durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]; got != moleculeLifecycleVersionV1 {
				t.Fatalf("pending marker = %q, want retained v1", got)
			}
		})
	}
}

func TestRecoverMoleculeLifecycleIntentsReadAndClearFailures(t *testing.T) {
	t.Run("live get failure retains without fabrication", func(t *testing.T) {
		base := beads.NewMemStore()
		root, _ := seedPendingClosedMolecule(t, base, false, moleculeAutocloseReason, time.Now().UTC())
		store := &lifecycleFaultStore{Store: base, failGetID: root.ID}
		rec := &lifecycleRecorder{}
		if retry := recoverMoleculeLifecycleIntents(store, rec); !retry {
			t.Fatal("retry = false, want true after live Get failure")
		}
		if got := len(rec.snapshot()); got != 0 {
			t.Fatalf("events = %d, want zero", got)
		}
		durable, _ := base.Get(root.ID)
		if durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
			t.Fatal("pending marker was not retained")
		}
	})

	t.Run("marker clear failure is retryable", func(t *testing.T) {
		base := beads.NewMemStore()
		seedPendingClosedMolecule(t, base, false, moleculeAutocloseReason, time.Now().UTC())
		store := &lifecycleFaultStore{Store: base, failSetKey: beadmeta.MoleculeLifecyclePendingMetadataKey}
		rec := &lifecycleRecorder{}
		if retry := recoverMoleculeLifecycleIntents(store, rec); !retry {
			t.Fatal("retry = false, want true after marker clear failure")
		}
		if got := len(rec.snapshot()); got != 2 {
			t.Fatalf("events = %d, want lifecycle pair before retryable clear failure", got)
		}
	})

	t.Run("intent clear failure is harmless", func(t *testing.T) {
		base := beads.NewMemStore()
		root, _ := seedPendingClosedMolecule(t, base, false, moleculeAutocloseReason, time.Now().UTC())
		store := &lifecycleFaultStore{Store: base, failSetKey: beadmeta.MoleculeLifecycleIntentMetadataKey}
		rec := &lifecycleRecorder{}
		if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
			t.Fatal("retry = true, want false after harmless intent cleanup failure")
		}
		durable, _ := base.Get(root.ID)
		if durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" {
			t.Fatal("marker not cleared before intent cleanup")
		}
		if durable.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] == "" {
			t.Fatal("intent unexpectedly cleared despite injected failure")
		}
	})
}

func TestRecoverMoleculeLifecycleIntentsProcessesRowsFromPartialList(t *testing.T) {
	base := beads.NewMemStore()
	seedPendingClosedMolecule(t, base, false, moleculeAutocloseReason, time.Now().UTC())
	store := &lifecyclePartialListStore{Store: base}
	rec := &lifecycleRecorder{}

	if retry := recoverMoleculeLifecycleIntents(store, rec); !retry {
		t.Fatal("retry = false, want true for partial List error")
	}
	if got := lifecycleEventTypes(rec.snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{events.BeadClosed, events.MoleculeResolved}) {
		t.Fatalf("events = %v, want validated partial row processed", got)
	}
}

func TestRecoverMoleculeLifecycleDiscoveryMintsUnmarkedRootFromPartialList(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "eligible molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := base.Create(beads.Bead{Title: "terminal child", Type: "step", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := base.Close(child.ID); err != nil {
		t.Fatalf("Close child: %v", err)
	}
	store := &lifecycleDiscoveryPartialListStore{Store: base}
	rec := &lifecycleRecorder{}

	// The partial discovery list error requests a retry; the unmarked, never-
	// published eligible root is still minted (the outage-gap heal).
	if retry := recoverMoleculeLifecycleIntents(store, rec); !retry {
		t.Fatal("retry = false, want true for partial discovery List error")
	}
	after, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root after recovery: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed from a minted unmarked eligible root", after.Status)
	}
	if got := lifecycleEventTypes(rec.snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{events.BeadClosed, events.MoleculeResolved}) {
		t.Fatalf("events = %v, want one minted lifecycle pair", got)
	}
	if after.Metadata[beadmeta.MoleculeLifecycleCompletedMetadataKey] == "" {
		t.Fatal("completed marker not set after minting; a later reopen would be re-minted")
	}
}

func TestRecoverEligibleMoleculeLifecycleSkipsCompletedRootButMintsUnmarked(t *testing.T) {
	seedEligibleMolecule := func(t *testing.T, store beads.Store, metadata map[string]string) beads.Bead {
		t.Helper()
		root, err := store.Create(beads.Bead{Title: "eligible molecule", Type: "molecule", Metadata: metadata})
		if err != nil {
			t.Fatalf("Create root: %v", err)
		}
		child, err := store.Create(beads.Bead{Title: "terminal child", Type: "step", ParentID: root.ID})
		if err != nil {
			t.Fatalf("Create child: %v", err)
		}
		if err := store.Close(child.ID); err != nil {
			t.Fatalf("Close child: %v", err)
		}
		return root
	}

	t.Run("completed marker without prepared marker is skipped", func(t *testing.T) {
		store := beads.NewMemStore()
		// A root published once and reopened by an operator carries only the
		// completed marker — exactly what a real publication leaves behind.
		root := seedEligibleMolecule(t, store, map[string]string{
			beadmeta.MoleculeLifecycleCompletedMetadataKey: "0123456789abcdef0123456789abcdef",
		})
		rec := &lifecycleRecorder{}
		if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
			t.Fatal("recovery retry = true, want complete")
		}
		after, err := store.Get(root.ID)
		if err != nil {
			t.Fatalf("Get root: %v", err)
		}
		if after.Status != "open" {
			t.Fatalf("root status = %q, want open; a completed-marker (reopened) root must not be re-minted", after.Status)
		}
		if got := len(rec.snapshot()); got != 0 {
			t.Fatalf("events = %d, want zero for a completed-marker root", got)
		}
	})

	t.Run("truly unmarked eligible root is minted", func(t *testing.T) {
		store := beads.NewMemStore()
		root := seedEligibleMolecule(t, store, nil)
		rec := &lifecycleRecorder{}
		if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
			t.Fatal("recovery retry = true, want complete")
		}
		after, err := store.Get(root.ID)
		if err != nil {
			t.Fatalf("Get root: %v", err)
		}
		if after.Status != "closed" {
			t.Fatalf("root status = %q, want closed; a never-published eligible root heals the outage gap", after.Status)
		}
		if got := lifecycleEventTypes(rec.snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{events.BeadClosed, events.MoleculeResolved}) {
			t.Fatalf("events = %v, want one minted lifecycle pair", got)
		}
		if after.Metadata[beadmeta.MoleculeLifecycleCompletedMetadataKey] == "" {
			t.Fatal("completed marker not set after minting")
		}
	})
}

func TestRecoverEligibleMoleculeLifecycleDoesNotReCloseReopenedRoot(t *testing.T) {
	store := beads.NewMemStore()
	requestedAt := time.Date(2026, 7, 16, 14, 0, 0, 0, time.UTC)
	root, _ := seedPendingClosedMolecule(t, store, false, moleculeAutocloseReason, requestedAt)
	child, err := store.Create(beads.Bead{Title: "terminal child", Type: "step", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("Close child: %v", err)
	}
	rec := &lifecycleRecorder{}

	// First pass publishes the prepared intent, clears its marker, and stamps the
	// completed marker — the durable record of publication reality produces.
	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("initial recovery retry = true, want prepared intent published")
	}
	published := len(rec.snapshot())
	if published != 2 {
		t.Fatalf("published events = %d, want the ordered lifecycle pair", published)
	}
	afterPublish, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get published root: %v", err)
	}
	if afterPublish.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" {
		t.Fatalf("pending marker after publication = %q, want cleared", afterPublish.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey])
	}
	if afterPublish.Metadata[beadmeta.MoleculeLifecycleCompletedMetadataKey] == "" {
		t.Fatal("completed marker not stamped by publication; reopen would be re-minted")
	}

	// An operator reopens the auto-closed root — a documented recovery workflow.
	// Its subtree is still terminal, so a level-triggered scan would re-close it,
	// but the completed marker (no prepared marker) makes recovery skip it.
	if err := store.Reopen(root.ID); err != nil {
		t.Fatalf("Reopen root: %v", err)
	}
	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("post-reopen recovery retry = true, want complete")
	}
	afterReopen, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get reopened root: %v", err)
	}
	if afterReopen.Status != "open" {
		t.Fatalf("reopened root status = %q, want open; recovery must not re-close an operator-reopened root", afterReopen.Status)
	}
	if got := len(rec.snapshot()); got != published {
		t.Fatalf("events after reopen = %d, want unchanged %d", got, published)
	}
}

func TestMoleculeLifecycleEventsCarryOccurrenceTimestamp(t *testing.T) {
	intent, err := newMoleculeLifecycleIntent("open", "controller", moleculeAutocloseReason, time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("newMoleculeLifecycleIntent: %v", err)
	}

	t.Run("prefers closed row UpdatedAt", func(t *testing.T) {
		updatedAt := time.Date(2026, 7, 16, 11, 30, 0, 0, time.UTC)
		closed := beads.Bead{ID: "gc-1", Status: "closed", UpdatedAt: updatedAt, Metadata: map[string]string{"close_reason": moleculeAutocloseReason}}
		closedEvent, resolvedEvent, err := moleculeLifecycleEvents(closed, intent)
		if err != nil {
			t.Fatalf("moleculeLifecycleEvents: %v", err)
		}
		if !closedEvent.Ts.Equal(updatedAt) || closedEvent.Ts.Location() != time.UTC {
			t.Errorf("bead.closed Ts = %s, want close-commit UpdatedAt %s in UTC", closedEvent.Ts, updatedAt)
		}
		if !resolvedEvent.Ts.Equal(updatedAt) || resolvedEvent.Ts.Location() != time.UTC {
			t.Errorf("molecule.resolved Ts = %s, want close-commit UpdatedAt %s in UTC", resolvedEvent.Ts, updatedAt)
		}
	})

	t.Run("falls back to intent RequestedAt when UpdatedAt is zero", func(t *testing.T) {
		closed := beads.Bead{ID: "gc-1", Status: "closed", Metadata: map[string]string{"close_reason": moleculeAutocloseReason}}
		closedEvent, resolvedEvent, err := moleculeLifecycleEvents(closed, intent)
		if err != nil {
			t.Fatalf("moleculeLifecycleEvents: %v", err)
		}
		if !closedEvent.Ts.Equal(intent.RequestedAt) {
			t.Errorf("bead.closed Ts = %s, want intent RequestedAt %s", closedEvent.Ts, intent.RequestedAt)
		}
		if !resolvedEvent.Ts.Equal(intent.RequestedAt) {
			t.Errorf("molecule.resolved Ts = %s, want intent RequestedAt %s", resolvedEvent.Ts, intent.RequestedAt)
		}
		if closedEvent.Ts.IsZero() {
			t.Error("bead.closed Ts is zero; a bare recorder would back-fill publish-time")
		}
	})
}

func TestLiveSubtreeTerminalExcludingRootUsesIndexedParentQueries(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "molecule root", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "terminal child", Type: "step", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	grandchild, err := store.Create(beads.Bead{Title: "terminal grandchild", Type: "step", ParentID: child.ID})
	if err != nil {
		t.Fatalf("Create grandchild: %v", err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("Close child: %v", err)
	}
	if err := store.Close(grandchild.ID); err != nil {
		t.Fatalf("Close grandchild: %v", err)
	}
	tx := &indexedParentLifecycleReadTransaction{Store: store, id: root.ID}

	terminal, descendants, err := liveSubtreeTerminalExcludingRoot(tx, root.ID)
	if err != nil {
		t.Fatalf("liveSubtreeTerminalExcludingRoot: %v", err)
	}
	if !terminal || descendants != 2 {
		t.Fatalf("subtree result = terminal:%t descendants:%d, want true/2", terminal, descendants)
	}
	if got, want := fmt.Sprint(tx.parentQueries), fmt.Sprint([]string{root.ID, child.ID, grandchild.ID}); got != want {
		t.Fatalf("indexed parent queries = %v, want %v", tx.parentQueries, []string{root.ID, child.ID, grandchild.ID})
	}
}

func TestRecoverMoleculeLifecycleIntentsClosesGeneratedSpecSidecars(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := seedPendingClosedMolecule(t, store, false, moleculeSourceAutocloseReason, time.Now().UTC())
	spec, err := store.Create(beads.Bead{
		Title: "generated workflow spec",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create spec: %v", err)
	}

	if retry := recoverMoleculeLifecycleIntents(store, &lifecycleRecorder{}); retry {
		t.Fatal("retry = true, want complete")
	}
	after, err := store.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get spec: %v", err)
	}
	if after.Status != "closed" || after.Metadata["close_reason"] != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("spec after recovery = %+v, want closed after root lifecycle", after)
	}
}

func TestRecoverMoleculeLifecycleIntentsResumesEligibleOpenIntent(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "recoverable legacy molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	step, err := base.Create(beads.Bead{Title: "terminal step", Type: "step", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}
	if err := base.Close(step.ID); err != nil {
		t.Fatalf("Close step: %v", err)
	}
	store := &moleculeAutocloseFailOnceLegacyCloseStore{Store: base, failID: root.ID, failures: 1}
	rec := events.NewFake()
	var stdout bytes.Buffer
	if retry := doMoleculeAutocloseWith(store, "", rec, step.ID, &stdout).Wait(); !retry {
		t.Fatal("initial autoclose retry = false after injected pre-commit close failure")
	}

	pending, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get pending root: %v", err)
	}
	if pending.Status != "open" || pending.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("pending root = %+v, want open v1 lifecycle intent", pending)
	}
	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		current, _ := base.Get(root.ID)
		t.Fatalf("recovery retry = true, want eligible prepared close recovered; root=%+v events=%+v", current, rec.Events)
	}

	after, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get recovered root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("recovered root status = %q, want closed", after.Status)
	}
	if after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
		after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
		t.Fatalf("recovered lifecycle metadata = %#v, want cleared", after.Metadata)
	}
	if got := lifecycleEventTypes(rec.Events); fmt.Sprint(got) != fmt.Sprint([]string{events.BeadClosed, events.MoleculeResolved}) {
		t.Fatalf("recovered lifecycle events = %v, want one ordered pair", got)
	}
	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("second recovery retry = true, want idempotent complete")
	}
	if got := len(rec.Events); got != 2 {
		t.Fatalf("events after idempotent recovery = %d, want 2: %+v", got, rec.Events)
	}
}

func TestRecoverMoleculeLifecycleIntentsResumesPreparedSourceIntentAcrossAtomicCapabilityChange(t *testing.T) {
	base := beads.NewMemStore()
	source, err := base.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := base.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}
	root, err := base.Create(beads.Bead{
		Title: "stepless graph workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	spec, err := base.Create(beads.Bead{
		Title: "generated step spec",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create spec: %v", err)
	}
	capabilityless := &moleculeAutocloseNoTransitionStore{Store: base}
	_, prepared, err := prepareMoleculeLifecycleIntent(
		capabilityless,
		root.ID,
		moleculeSourceAutocloseReason,
		"source-controller",
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("prepare source intent: %v", err)
	}
	rec := &lifecycleRecorder{}

	// Recovery must resume the exact prepared fallback intent even though the
	// store now exposes atomic close support. Probing the new capability would
	// strand or duplicate the durable intent's publication ownership.
	if retry := recoverMoleculeLifecycleIntents(base, rec); retry {
		t.Fatal("source recovery retry = true, want complete")
	}
	after, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if after.Status != "closed" || after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
		after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
		t.Fatalf("recovered source root = %+v, want closed with lifecycle metadata cleared", after)
	}
	closedSpec, err := base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get spec: %v", err)
	}
	if closedSpec.Status != "closed" {
		t.Fatalf("generated spec status = %q, want closed after recovered root lifecycle", closedSpec.Status)
	}
	if prepared.IntentID == "" {
		t.Fatal("prepared source intent id is empty")
	}
	if got := lifecycleEventTypes(rec.snapshot()); fmt.Sprint(got) != fmt.Sprint([]string{events.BeadClosed, events.MoleculeResolved}) {
		t.Fatalf("source lifecycle events = %v, want one ordered pair", got)
	}
	if retry := recoverMoleculeLifecycleIntents(base, rec); retry {
		t.Fatal("second source recovery retry = true, want idempotent complete")
	}
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("events after second source recovery = %d, want 2", got)
	}
}

func TestRecoverMoleculeLifecycleIntentsRetainsIneligibleOpenIntents(t *testing.T) {
	tests := []struct {
		name         string
		reason       string
		rootType     string
		workflowRoot bool
		childStatus  string
		sourceStatus string
		statusAfter  string
	}{
		{name: "molecule has no descendants", reason: moleculeAutocloseReason, rootType: "molecule"},
		{name: "molecule descendant is open", reason: moleculeAutocloseReason, rootType: "molecule", childStatus: "open"},
		{name: "ordinary reason has wrong root type", reason: moleculeAutocloseReason, rootType: "task", childStatus: "closed"},
		{name: "source workflow descendant is open", reason: moleculeSourceAutocloseReason, rootType: "task", workflowRoot: true, childStatus: "open", sourceStatus: "closed"},
		{name: "source bead was reopened", reason: moleculeSourceAutocloseReason, rootType: "task", workflowRoot: true, sourceStatus: "open"},
		{name: "root status differs from intent", reason: moleculeAutocloseReason, rootType: "molecule", childStatus: "closed", statusAfter: "in_progress"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := beads.NewMemStore()
			metadata := map[string]string{}
			if tc.workflowRoot {
				source, err := base.Create(beads.Bead{Title: "source", Type: "task"})
				if err != nil {
					t.Fatalf("Create source: %v", err)
				}
				if tc.sourceStatus == "closed" {
					if err := base.Close(source.ID); err != nil {
						t.Fatalf("Close source: %v", err)
					}
				}
				metadata[beadmeta.KindMetadataKey] = beadmeta.KindWorkflow
				metadata[beadmeta.FormulaContractMetadataKey] = beadmeta.FormulaContractGraphV2
				metadata[beadmeta.SourceBeadIDMetadataKey] = source.ID
			}
			root, err := base.Create(beads.Bead{Title: "pending root", Type: tc.rootType, Metadata: metadata})
			if err != nil {
				t.Fatalf("Create root: %v", err)
			}
			if tc.childStatus != "" {
				child, err := base.Create(beads.Bead{Title: "child", Type: "step", ParentID: root.ID})
				if err != nil {
					t.Fatalf("Create child: %v", err)
				}
				if tc.childStatus == "closed" {
					if err := base.Close(child.ID); err != nil {
						t.Fatalf("Close child: %v", err)
					}
				}
			}
			_, intent, err := prepareMoleculeLifecycleIntent(base, root.ID, tc.reason, "controller", time.Now().UTC())
			if err != nil {
				t.Fatalf("prepare intent: %v", err)
			}
			if tc.statusAfter != "" {
				status := tc.statusAfter
				if err := base.Update(root.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("Update root status: %v", err)
				}
			}
			rec := &lifecycleRecorder{}
			if retry := recoverMoleculeLifecycleIntents(base, rec); retry {
				t.Fatal("ineligible recovery retry = true, want retained without hot retry")
			}
			after, err := base.Get(root.ID)
			if err != nil {
				t.Fatalf("Get retained root: %v", err)
			}
			wantStatus := "open"
			if tc.statusAfter != "" {
				wantStatus = tc.statusAfter
			}
			if after.Status != wantStatus {
				t.Fatalf("retained root status = %q, want %q", after.Status, wantStatus)
			}
			retained, err := decodeMoleculeLifecycleIntent(after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
			if err != nil || retained.IntentID != intent.IntentID ||
				after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
				t.Fatalf("retained intent = %+v err=%v metadata=%v, want original v1 owner", retained, err, after.Metadata)
			}
			if got := len(rec.snapshot()); got != 0 {
				t.Fatalf("events for ineligible root = %d, want zero", got)
			}
		})
	}
}

func TestMoleculeLifecycleSidecarFailuresRequestRetry(t *testing.T) {
	t.Run("direct sidecar completion", func(t *testing.T) {
		store := &lifecycleSidecarListErrorStore{
			Store:   beads.NewMemStore(),
			listErr: errors.New("injected sidecar list failure"),
		}
		if retry := closeSpecSidecarsForRootCompletion(store, "root-1").Wait(); !retry {
			t.Fatal("sidecar completion retry = false, want true after list failure")
		}
	})

	t.Run("durable lifecycle publication", func(t *testing.T) {
		base := beads.NewMemStore()
		root, intent := seedPendingClosedMolecule(t, base, false, moleculeAutocloseReason, time.Now().UTC())
		store := &lifecycleSidecarListErrorStore{
			Store:   base,
			listErr: errors.New("injected sidecar list failure"),
		}
		if retry := publishPendingMoleculeLifecycle(store, &lifecycleRecorder{}, root.ID, intent.IntentID, nil).Wait(); !retry {
			t.Fatal("durable publication retry = false, want true after sidecar list failure")
		}
	})
}

func TestRecoverMoleculeLifecycleIntentsRepairsSidecarsAfterMarkerCleanupFailure(t *testing.T) {
	base := beads.NewMemStore()
	root, intent := seedPendingClosedMolecule(t, base, false, moleculeSourceAutocloseReason, time.Now().UTC())
	if err := base.SetMetadataBatch(root.ID, map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
		beadmeta.SourceBeadIDMetadataKey: "source-1",
	}); err != nil {
		t.Fatalf("mark workflow root: %v", err)
	}
	spec, err := base.Create(beads.Bead{
		Title: "generated workflow spec",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create spec: %v", err)
	}
	store := &lifecycleFailOnceSidecarListStore{Store: base, failures: 1}
	rec := &lifecycleRecorder{}
	if retry := publishPendingMoleculeLifecycle(store, rec, root.ID, intent.IntentID, nil).Wait(); !retry {
		t.Fatal("initial publication retry = false after injected sidecar failure")
	}
	failed, err := base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get spec after failure: %v", err)
	}
	if failed.Status != "open" {
		t.Fatalf("spec status after injected failure = %q, want open", failed.Status)
	}
	publishedRoot, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root after publication: %v", err)
	}
	if publishedRoot.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" {
		t.Fatalf("root marker after publication = %q, want cleared", publishedRoot.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey])
	}

	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("repair recovery retry = true, want complete")
	}
	repaired, err := base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get repaired spec: %v", err)
	}
	if repaired.Status != "closed" || repaired.Metadata["close_reason"] != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("repaired spec = %+v, want closed with sidecar reason", repaired)
	}
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("root lifecycle events after repair = %d, want original pair only", got)
	}
}

func TestRecoverMoleculeLifecycleIntentsWaitsForGlobalSidecarRepairDelivery(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{
		Title: "closed workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	if err := base.Close(root.ID); err != nil {
		t.Fatalf("Close root: %v", err)
	}
	spec, err := base.Create(beads.Bead{
		Title: "generated spec residue",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create spec: %v", err)
	}
	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	defer func() {
		select {
		case <-releaseObserver:
		default:
			close(releaseObserver)
		}
	}()
	var blockOnce sync.Once
	cache := beads.NewCachingStoreForTest(base, func(eventType, id string, _ json.RawMessage) {
		if eventType == events.BeadClosed && id == spec.ID {
			blockOnce.Do(func() {
				close(observerEntered)
				<-releaseObserver
			})
		}
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	completion := recoverPendingMoleculeLifecycles(cache, &lifecycleRecorder{})
	waitDone := make(chan bool, 1)
	go func() { waitDone <- completion.Wait() }()
	select {
	case <-observerEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("global sidecar repair observer did not enter")
	}
	select {
	case <-waitDone:
		t.Fatal("recovery completed before repaired sidecar delivery drained")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseObserver)
	select {
	case retry := <-waitDone:
		if retry {
			t.Fatal("global sidecar repair retry = true, want complete")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("recovery did not complete after repaired sidecar delivery")
	}
}

func TestPublishPendingMoleculeLifecycleDoesNotPublishOrClearReplacementIntent(t *testing.T) {
	store := beads.NewMemStore()
	root, ownerB := seedPendingClosedMolecule(t, store, false, moleculeAutocloseReason, time.Now().UTC())
	ownerA, err := newMoleculeLifecycleIntent("open", "other-writer", moleculeAutocloseReason, time.Now().UTC())
	if err != nil {
		t.Fatalf("new owner A intent: %v", err)
	}
	rec := &lifecycleRecorder{}

	if retry := publishPendingMoleculeLifecycle(store, rec, root.ID, ownerA.IntentID, nil).Wait(); retry {
		t.Fatal("retry = true for superseded owner, want retained periodic handling")
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("events = %d, want zero for superseded owner", got)
	}
	durable, _ := store.Get(root.ID)
	retained, err := decodeMoleculeLifecycleIntent(durable.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil {
		t.Fatalf("decode replacement intent: %v", err)
	}
	if retained.IntentID != ownerB.IntentID || durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("replacement intent changed: retained=%+v metadata=%v", retained, durable.Metadata)
	}
}

func TestPublishPendingMoleculeLifecycleCleanupDoesNotEraseRacingReplacementIntent(t *testing.T) {
	base := beads.NewMemStore()
	root, ownerA := seedPendingClosedMolecule(t, base, false, moleculeAutocloseReason, time.Now().UTC())
	store := &lifecycleReplacementRaceStore{
		Store:                  base,
		ownershipReadEntered:   make(chan struct{}),
		releaseOwnershipRead:   make(chan struct{}),
		replacementAttempted:   make(chan struct{}),
		replacementEnteredLock: make(chan struct{}),
	}
	defer func() {
		select {
		case <-store.releaseOwnershipRead:
		default:
			close(store.releaseOwnershipRead)
		}
	}()

	publicationDone := make(chan bool, 1)
	go func() {
		publicationDone <- publishPendingMoleculeLifecycle(store, &lifecycleRecorder{}, root.ID, ownerA.IntentID, nil).Wait()
	}()
	select {
	case <-store.ownershipReadEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("owner A cleanup did not reach its marker-cleared ownership read")
	}
	if err := base.Reopen(root.ID); err != nil {
		t.Fatalf("Reopen for replacement: %v", err)
	}

	type prepareResult struct {
		intent moleculeLifecycleIntent
		err    error
	}
	replacementDone := make(chan prepareResult, 1)
	go func() {
		_, intent, err := prepareMoleculeLifecycleIntent(
			store,
			root.ID,
			moleculeSourceAutocloseReason,
			"replacement-controller",
			time.Now().UTC(),
		)
		replacementDone <- prepareResult{intent: intent, err: err}
	}()

	var replacement prepareResult
	select {
	case replacement = <-replacementDone:
		// Without the lifecycle critical section, owner B can install its new
		// intent while owner A is paused on a now-stale ownership snapshot.
		close(store.releaseOwnershipRead)
	case <-store.replacementAttempted:
		select {
		case <-store.replacementEnteredLock:
			t.Fatal("replacement entered the lifecycle transaction before owner A released it")
		default:
		}
		close(store.releaseOwnershipRead)
		select {
		case replacement = <-replacementDone:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("replacement prepare did not finish after owner A released the lifecycle transaction")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement neither completed nor attempted the lifecycle transaction")
	}
	if replacement.err != nil {
		t.Fatalf("prepare replacement intent: %v", replacement.err)
	}
	select {
	case retry := <-publicationDone:
		if retry {
			t.Fatal("owner A publication requested retry")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("owner A publication did not finish")
	}

	durable, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get replacement lifecycle: %v", err)
	}
	retained, err := decodeMoleculeLifecycleIntent(durable.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil {
		t.Fatalf("decode retained replacement intent: %v (metadata=%v)", err, durable.Metadata)
	}
	if retained.IntentID != replacement.intent.IntentID || retained.CloseReason != moleculeSourceAutocloseReason ||
		durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		t.Fatalf("replacement lifecycle was not retained: intent=%+v replacement=%+v metadata=%v", retained, replacement.intent, durable.Metadata)
	}
}

func TestPublishPendingMoleculeLifecycleBarrierOrdersPriorUpdateAndMarkerClear(t *testing.T) {
	base := beads.NewMemStore()
	root, intent := seedPendingClosedMolecule(t, base, false, moleculeAutocloseReason, time.Now().UTC())
	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	intentClearEntered := make(chan struct{})
	releaseIntentClear := make(chan struct{})
	var blockOnce sync.Once
	var blockIntentClearOnce sync.Once
	order := &lifecycleOrderLog{}
	cache := beads.NewCachingStoreForTest(base, func(eventType, id string, payload json.RawMessage) {
		if eventType != events.BeadUpdated || id != root.ID {
			return
		}
		var snapshot beads.Bead
		_ = json.Unmarshal(payload, &snapshot)
		if snapshot.Metadata["prior"] == "queued" && snapshot.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] == moleculeLifecycleVersionV1 {
			blockOnce.Do(func() {
				order.add("prior update")
				close(observerEntered)
				<-releaseObserver
			})
			return
		}
		if snapshot.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] == "" && snapshot.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
			order.add("marker clear update")
			return
		}
		if snapshot.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] == "" && snapshot.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] == "" {
			blockIntentClearOnce.Do(func() {
				order.add("intent clear update")
				close(intentClearEntered)
				<-releaseIntentClear
			})
		}
	})

	writeDone := make(chan error, 1)
	go func() { writeDone <- cache.SetMetadata(root.ID, "prior", "queued") }()
	select {
	case <-observerEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior update observer did not enter")
	}
	rec := &orderedLifecycleRecorder{order: order}
	completion := publishPendingMoleculeLifecycle(cache, rec, root.ID, intent.IntentID, nil)
	select {
	case <-completion.Done():
		t.Fatal("publication completed before prior observer")
	default:
	}
	close(releaseObserver)
	select {
	case <-intentClearEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("intent-clear observer did not enter")
	}
	select {
	case <-completion.Done():
		t.Fatal("publication completed before intent-clear observer drained")
	default:
	}
	close(releaseIntentClear)
	if err := <-writeDone; err != nil {
		t.Fatalf("prior SetMetadata: %v", err)
	}
	if retry := completion.Wait(); retry {
		t.Fatal("publication retry = true, want complete")
	}
	if got, want := order.snapshot(), []string{"prior update", events.BeadClosed, events.MoleculeResolved, "marker clear update", "intent clear update"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

const (
	eventsPkgBeadClosed       = events.BeadClosed
	eventsPkgMoleculeResolved = events.MoleculeResolved
)

func seedPendingClosedMolecule(t *testing.T, store beads.Store, ephemeral bool, reason string, requestedAt time.Time) (beads.Bead, moleculeLifecycleIntent) {
	t.Helper()
	intent, err := newMoleculeLifecycleIntent("open", "controller", reason, requestedAt)
	if err != nil {
		t.Fatalf("newMoleculeLifecycleIntent: %v", err)
	}
	raw, err := marshalMoleculeLifecycleIntent(intent)
	if err != nil {
		t.Fatalf("marshalMoleculeLifecycleIntent: %v", err)
	}
	root, err := store.Create(beads.Bead{
		Title:     "pending lifecycle",
		Type:      "molecule",
		Ephemeral: ephemeral,
		Metadata: map[string]string{
			"close_reason": reason,
			beadmeta.MoleculeLifecycleIntentMetadataKey:  raw,
			beadmeta.MoleculeLifecyclePendingMetadataKey: moleculeLifecycleVersionV1,
			beadmeta.SessionNameMetadataKey:              "forge",
			beadmeta.SessionIDMetadataKey:                "session-1",
			beadmeta.StepIDMetadataKey:                   "recover-lifecycle",
			beadmeta.WorkDirMetadataKey:                  "/city/rigs/forge",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(root.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get closed: %v", err)
	}
	return closed, intent
}

func seedPendingOpenMolecule(t *testing.T, store beads.Store, reason string, requestedAt time.Time) (beads.Bead, moleculeLifecycleIntent) {
	t.Helper()
	intent, err := newMoleculeLifecycleIntent("open", "controller", reason, requestedAt)
	if err != nil {
		t.Fatalf("newMoleculeLifecycleIntent: %v", err)
	}
	raw, err := marshalMoleculeLifecycleIntent(intent)
	if err != nil {
		t.Fatalf("marshalMoleculeLifecycleIntent: %v", err)
	}
	root, err := store.Create(beads.Bead{
		Title: "pending open lifecycle",
		Type:  "molecule",
		Metadata: map[string]string{
			"close_reason": reason,
			beadmeta.MoleculeLifecycleIntentMetadataKey:  raw,
			beadmeta.MoleculeLifecyclePendingMetadataKey: moleculeLifecycleVersionV1,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return root, intent
}

func assertRecoveredLifecyclePayloads(t *testing.T, got []events.Event, root beads.Bead, intent moleculeLifecycleIntent) {
	t.Helper()
	for _, event := range got {
		if event.RunID != root.ID || event.SessionID != "session-1" || event.StepID != "recover-lifecycle" {
			t.Errorf("%s correlation = run:%q session:%q step:%q, want %q/session-1/recover-lifecycle", event.Type, event.RunID, event.SessionID, event.StepID, root.ID)
		}
	}
	var closed beads.Bead
	if err := json.Unmarshal(got[0].Payload, &closed); err != nil {
		t.Fatalf("decode bead.closed payload: %v", err)
	}
	if closed.ID != root.ID || closed.Status != "closed" || closed.Metadata["close_reason"] != intent.CloseReason {
		t.Fatalf("bead.closed payload = %+v, want authoritative closed root", closed)
	}
	var resolved gcapi.MoleculeResolvedPayload
	if err := json.Unmarshal(got[1].Payload, &resolved); err != nil {
		t.Fatalf("decode molecule.resolved payload: %v", err)
	}
	if resolved.IssueID != root.ID || resolved.FromStatus != intent.FromStatus || resolved.ToStatus != "closed" || resolved.Actor != intent.Actor || resolved.CloseReason != intent.CloseReason || !resolved.Ts.Equal(intent.RequestedAt) {
		t.Fatalf("molecule.resolved payload = %+v, want intent-derived transition", resolved)
	}
	if resolved.SessionName != "forge" || resolved.SessionID != "session-1" || resolved.WorkDir != "/city/rigs/forge" {
		t.Fatalf("molecule.resolved attribution = %+v, want closed snapshot metadata", resolved)
	}
}

func lifecycleEventTypes(got []events.Event) []string {
	types := make([]string, 0, len(got))
	for _, event := range got {
		types = append(types, event.Type)
	}
	return types
}

type lifecycleRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *lifecycleRecorder) Record(event events.Event) {
	_ = r.RecordDurably(event)
}

func (r *lifecycleRecorder) RecordDurably(batch ...events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, batch...)
	return nil
}

func (r *lifecycleRecorder) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.events...)
}

type failOnceDurableLifecycleRecorder struct {
	mu       sync.Mutex
	events   []events.Event
	failures int
}

func (r *failOnceDurableLifecycleRecorder) Record(event events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *failOnceDurableLifecycleRecorder) RecordDurably(batch ...events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures > 0 {
		r.failures--
		return errors.New("injected durable publication failure")
	}
	r.events = append(r.events, batch...)
	return nil
}

func (r *failOnceDurableLifecycleRecorder) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.events...)
}

type partialFailOnceDurableLifecycleRecorder struct {
	mu     sync.Mutex
	events []events.Event
	failed bool
}

func (r *partialFailOnceDurableLifecycleRecorder) Record(event events.Event) {
	_ = r.RecordDurably(event)
}

func (r *partialFailOnceDurableLifecycleRecorder) RecordDurably(batch ...events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.failed {
		r.failed = true
		if len(batch) > 0 {
			r.events = append(r.events, batch[0])
		}
		return errors.New("injected partial durable publication failure")
	}
	r.events = append(r.events, batch...)
	return nil
}

func (r *partialFailOnceDurableLifecycleRecorder) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.events...)
}

type lifecycleOrderLog struct {
	mu    sync.Mutex
	items []string
}

func (l *lifecycleOrderLog) add(item string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, item)
}

func (l *lifecycleOrderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.items...)
}

type orderedLifecycleRecorder struct{ order *lifecycleOrderLog }

func (r *orderedLifecycleRecorder) Record(event events.Event) { r.order.add(event.Type) }

func (r *orderedLifecycleRecorder) RecordDurably(batch ...events.Event) error { //nolint:unparam // error return satisfies events.DurableRecorder; this ordering spy always succeeds
	for _, event := range batch {
		r.order.add(event.Type)
	}
	return nil
}

type lifecycleWriteOrderStore struct {
	beads.Store
	mu     sync.Mutex
	writes []string
}

func (s *lifecycleWriteOrderStore) SetMetadataBatch(id string, values map[string]string) error {
	s.mu.Lock()
	s.writes = append(s.writes, "batch:close_reason,intent")
	s.mu.Unlock()
	return s.Store.SetMetadataBatch(id, values)
}

func (s *lifecycleWriteOrderStore) SetMetadata(id, key, value string) error {
	s.mu.Lock()
	if key == beadmeta.MoleculeLifecyclePendingMetadataKey {
		s.writes = append(s.writes, "set:pending="+value)
	} else {
		s.writes = append(s.writes, "set:"+key)
	}
	s.mu.Unlock()
	return s.Store.SetMetadata(id, key, value)
}

func (s *lifecycleWriteOrderStore) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

type lifecyclePartialPrepareStore struct{ beads.Store }

func (s *lifecyclePartialPrepareStore) SetMetadataBatch(id string, _ map[string]string) error {
	_ = s.SetMetadata(id, "close_reason", moleculeAutocloseReason)
	return errors.New("injected partial metadata batch failure")
}

type lifecycleFaultStore struct {
	beads.Store
	failGetID  string
	failSetKey string
}

type lifecycleReplacementRaceStore struct {
	beads.Store

	txMu sync.Mutex

	ownershipReadState     atomic.Uint32
	ownershipReadEntered   chan struct{}
	releaseOwnershipRead   chan struct{}
	transactionCount       atomic.Uint32
	replacementAttempted   chan struct{}
	replacementAttemptOnce sync.Once
	replacementEnteredLock chan struct{}
	replacementEnteredOnce sync.Once
}

func (s *lifecycleReplacementRaceStore) Get(id string) (beads.Bead, error) {
	fresh, err := s.Store.Get(id)
	if err == nil && fresh.ID == id && fresh.Status == "closed" &&
		fresh.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] == "" &&
		fresh.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" &&
		s.ownershipReadState.CompareAndSwap(0, 1) {
		close(s.ownershipReadEntered)
		<-s.releaseOwnershipRead
	}
	return fresh, err
}

func (s *lifecycleReplacementRaceStore) WithLifecycleMetadataTransaction(id string, fn func(beads.LifecycleMetadataTransaction) error) error {
	transactionNumber := s.transactionCount.Add(1)
	if transactionNumber > 1 {
		s.replacementAttemptOnce.Do(func() { close(s.replacementAttempted) })
	}
	s.txMu.Lock()
	defer s.txMu.Unlock()
	if transactionNumber > 1 {
		s.replacementEnteredOnce.Do(func() { close(s.replacementEnteredLock) })
	}
	return fn(lifecycleReplacementRaceTransaction{store: s, id: id})
}

type lifecycleReplacementRaceTransaction struct {
	store *lifecycleReplacementRaceStore
	id    string
}

func (tx lifecycleReplacementRaceTransaction) Get() (beads.Bead, error) {
	return tx.store.Get(tx.id)
}

func (tx lifecycleReplacementRaceTransaction) SetMetadata(key, value string) error {
	return tx.store.SetMetadata(tx.id, key, value)
}

func (tx lifecycleReplacementRaceTransaction) SetMetadataBatch(values map[string]string) error {
	return tx.store.SetMetadataBatch(tx.id, values)
}

type lifecycleExplicitHandlesStore struct {
	beads.Store
	stale beads.Bead
}

func (s *lifecycleExplicitHandlesStore) Handles() beads.StoreHandles {
	handles := beads.HandlesFor(s.Store)
	handles.Cached = lifecycleStaleCachedReader{CachedReader: handles.Cached, stale: s.stale}
	handles.Writer = s
	return handles
}

type lifecycleStaleCachedReader struct {
	beads.CachedReader
	stale beads.Bead
}

func (r lifecycleStaleCachedReader) Get(id string) (beads.Bead, error) {
	if id == r.stale.ID {
		return r.stale, nil
	}
	return r.CachedReader.Get(id)
}

type lifecyclePartialListStore struct{ beads.Store }

func (s *lifecyclePartialListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil {
		return rows, err
	}
	return rows, errors.New("injected partial List failure")
}

type lifecycleDiscoveryPartialListStore struct{ beads.Store }

func (s *lifecycleDiscoveryPartialListStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *lifecycleDiscoveryPartialListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil {
		return rows, err
	}
	if query.Type == "molecule" && !query.IncludeClosed {
		return rows, &beads.PartialResultError{Op: "injected discovery list", Err: errors.New("malformed unrelated row")}
	}
	return rows, nil
}

type indexedParentLifecycleReadTransaction struct {
	beads.Store
	id            string
	parentQueries []string
}

func (tx *indexedParentLifecycleReadTransaction) Get() (beads.Bead, error) {
	return tx.GetByID(tx.id)
}

func (tx *indexedParentLifecycleReadTransaction) GetByID(id string) (beads.Bead, error) {
	return tx.Store.Get(id)
}

func (tx *indexedParentLifecycleReadTransaction) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.AllowScan || len(query.ParentIDs) > 0 {
		return nil, fmt.Errorf("unindexed subtree query: ParentIDs=%v AllowScan=%t", query.ParentIDs, query.AllowScan)
	}
	if query.ParentID != "" {
		tx.parentQueries = append(tx.parentQueries, query.ParentID)
	}
	return tx.Store.List(query)
}

func (tx *indexedParentLifecycleReadTransaction) SetMetadata(key, value string) error {
	return tx.Store.SetMetadata(tx.id, key, value)
}

func (tx *indexedParentLifecycleReadTransaction) SetMetadataBatch(values map[string]string) error {
	return tx.Store.SetMetadataBatch(tx.id, values)
}

type lifecycleSidecarListErrorStore struct {
	beads.Store
	listErr error
}

func (s *lifecycleSidecarListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.listErr
}

type lifecycleFailOnceSidecarListStore struct {
	beads.Store
	mu       sync.Mutex
	failures int
}

func (s *lifecycleFailOnceSidecarListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.TrimSpace(query.Metadata[beadmeta.RootBeadIDMetadataKey]) != "" {
		s.mu.Lock()
		if s.failures > 0 {
			s.failures--
			s.mu.Unlock()
			return nil, errors.New("injected one-shot sidecar list failure")
		}
		s.mu.Unlock()
	}
	return s.Store.List(query)
}

func (s *lifecycleFaultStore) Get(id string) (beads.Bead, error) {
	if id == s.failGetID {
		return beads.Bead{}, errors.New("injected live get failure")
	}
	return s.Store.Get(id)
}

func (s *lifecycleFaultStore) SetMetadata(id, key, value string) error {
	if key == s.failSetKey {
		return errors.New("injected metadata clear failure")
	}
	return s.Store.SetMetadata(id, key, value)
}
