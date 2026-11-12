package beadstest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestFakeBdUpdateUnsetsMetadataThroughBdStore pins the fake's --unset-metadata:
// a BdStore unset round-trips through it and the key is gone, so a BdStore
// case cannot pass with the key still set.
func TestFakeBdUpdateUnsetsMetadataThroughBdStore(t *testing.T) {
	store := NewFakeBd().NewBdStore()
	created, err := store.Create(beads.Bead{Title: "unset target", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Update(created.ID, beads.UpdateOpts{Metadata: map[string]string{"kept": "1", "gone": "2"}}); err != nil {
		t.Fatalf("Update(set): %v", err)
	}
	if err := store.Update(created.ID, beads.UpdateOpts{UnsetMetadata: []string{"gone"}}); err != nil {
		t.Fatalf("Update(unset): %v", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Metadata["gone"]; ok || got.Metadata["kept"] != "1" {
		t.Errorf("metadata = %v, want only kept=1", got.Metadata)
	}
}

// TestFakeBdUpdateRefusesAFlagItDoesNotModel pins the other half: an update
// flag the fake does not apply fails the command rather than being dropped.
func TestFakeBdUpdateRefusesAFlagItDoesNotModel(t *testing.T) {
	f := NewFakeBd()
	created, err := f.NewBdStore().Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = f.Run("", "bd", "update", "--json", created.ID, "--add-label", "x")
	if err == nil || !strings.Contains(err.Error(), "--add-label") {
		t.Fatalf("update --add-label = %v, want a refusal naming the flag", err)
	}
	got, err := f.NewBdStore().Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Labels) != 0 {
		t.Errorf("labels = %v, want the refused update to write nothing", got.Labels)
	}
}

// TestFakeBdUpdateEnforcesItsPreconditions pins the fake's --if-status and
// --if-assignee: a BdStore compare-and-swap that does not hold writes nothing
// and reports the lost swap, and one that holds writes.
func TestFakeBdUpdateEnforcesItsPreconditions(t *testing.T) {
	store := NewFakeBd().NewBdStore()
	created, err := store.Create(beads.Bead{Title: "fenced", Type: "task", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed := "closed"
	ok, err := store.UpdateIfAssignment(created.ID, "open", "worker-2", beads.UpdateOpts{Status: &closed})
	if err != nil || ok {
		t.Fatalf("UpdateIfAssignment(wrong assignee) = (%t, %v), want a lost swap", ok, err)
	}
	if got, err := store.Get(created.ID); err != nil || got.Status != "open" {
		t.Fatalf("after a lost swap: status = %q (%v), want open", got.Status, err)
	}
	ok, err = store.UpdateIfAssignment(created.ID, "open", "worker-1", beads.UpdateOpts{Status: &closed})
	if err != nil || !ok {
		t.Fatalf("UpdateIfAssignment(holder) = (%t, %v), want the swap", ok, err)
	}
	if got, err := store.Get(created.ID); err != nil || got.Status != "closed" {
		t.Fatalf("after the swap: status = %q (%v), want closed", got.Status, err)
	}
}

// TestFakeBdCreateKeepsParentAndEphemeral pins the fake's create flags:
// --parent records the parent (and fails for an unknown one, as bd does), and
// --ephemeral keeps the row off `bd list` while `bd show` still answers it.
func TestFakeBdCreateKeepsParentAndEphemeral(t *testing.T) {
	fb := NewFakeBd()
	store := fb.NewBdStore()
	parent, err := store.Create(beads.Bead{Title: "parent", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", Type: "step", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	if got, err := fb.Bead(child.ID); err != nil || got.ParentID != parent.ID {
		t.Fatalf("child's parent = %q (err %v), want %q", got.ParentID, err, parent.ID)
	}
	if _, err := store.Create(beads.Bead{Title: "orphan", Type: "step", ParentID: "no-such-bead"}); err == nil {
		t.Fatal("a create naming an unknown --parent succeeded; bd fails it")
	}
	eph, err := store.Create(beads.Bead{Title: "wisp", Type: "step", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create(ephemeral): %v", err)
	}
	if got, err := fb.Bead(eph.ID); err != nil || !got.Ephemeral {
		t.Fatalf("ephemeral create stored Ephemeral=%v (err %v)", got.Ephemeral, err)
	}
	listed, err := store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierIssues})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range listed {
		if b.ID == eph.ID {
			t.Fatal("bd list returned an ephemeral bead")
		}
	}
	if _, err := store.Get(eph.ID); err != nil {
		t.Fatalf("bd show did not answer the ephemeral bead: %v", err)
	}
}

// TestFakeBdModelsBdDefaultCloseReason pins that a close given no --reason
// shows bd's default "Closed" on the wire, as the real bd does
// (cmd/bd/close.go resolveCloseReasons), while a given reason and an open row
// show what they hold. BdStore must read the default as no reason; a fake that
// answered "" would let it skip that.
func TestFakeBdModelsBdDefaultCloseReason(t *testing.T) {
	f := NewFakeBd()
	store := f.NewBdStore()
	reasonless, err := store.Create(beads.Bead{Title: "closed without a reason", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	reasoned, err := store.Create(beads.Bead{Title: "closed with a reason", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	open, err := store.Create(beads.Bead{Title: "still open", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run("", "bd", "close", "--json", reasonless.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run("", "bd", "close", "--json", reasoned.ID, "--reason", "fixed in abc123"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{reasonless.ID: "Closed", reasoned.ID: "fixed in abc123", open.ID: ""} {
		out, err := f.Run("", "bd", "show", "--json", id)
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			CloseReason string `json:"close_reason"`
		}
		if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
			t.Fatalf("show %s = %s, %v", id, out, err)
		}
		if rows[0].CloseReason != want {
			t.Errorf("show %s close_reason = %q, want %q", id, rows[0].CloseReason, want)
		}
	}
}

// TestFakeBdKeepsTheCloseReasonColumnApartFromMetadata pins that the fake holds
// bd's close_reason column apart from metadata.close_reason, as real bd does:
// `bd close --reason X` writes only the column, so the metadata key stays the
// input BdStore forwards as --reason and never a copy of bd's output. Reopen
// clears the column, and a close given no reason stamps bd's default again.
func TestFakeBdKeepsTheCloseReasonColumnApartFromMetadata(t *testing.T) {
	f := NewFakeBd()
	store := f.NewBdStore()
	created, err := store.Create(beads.Bead{Title: "closed straight through bd", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	show := func(step string) (column string, metadataKey bool) {
		t.Helper()
		out, err := f.Run("", "bd", "show", "--json", created.ID)
		if err != nil {
			t.Fatalf("%s: show: %v", step, err)
		}
		var rows []struct {
			CloseReason string            `json:"close_reason"`
			Metadata    map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
			t.Fatalf("%s: show = %s, %v", step, out, err)
		}
		_, metadataKey = rows[0].Metadata["close_reason"]
		return rows[0].CloseReason, metadataKey
	}

	if _, err := f.Run("", "bd", "close", "--json", created.ID, "--reason", "fixed in abc123"); err != nil {
		t.Fatal(err)
	}
	if column, leaked := show("close --reason"); column != "fixed in abc123" || leaked {
		t.Errorf("after close --reason: column = %q, metadata.close_reason present = %v; want the column only", column, leaked)
	}

	if _, err := f.Run("", "bd", "reopen", "--json", created.ID); err != nil {
		t.Fatal(err)
	}
	if column, leaked := show("reopen"); column != "" || leaked {
		t.Errorf("after reopen: column = %q, metadata.close_reason present = %v; want neither", column, leaked)
	}

	if _, err := f.Run("", "bd", "close", "--json", created.ID); err != nil {
		t.Fatal(err)
	}
	if column, leaked := show("close without a reason"); column != "Closed" || leaked {
		t.Errorf("after a close with no reason: column = %q, metadata.close_reason present = %v; want bd's default and no key", column, leaked)
	}
}

// TestFakeBdBackedCloseForwardsTheMetadataReasonToBd pins, through the fake,
// that BdStore.Close forwards metadata.close_reason to bd as --reason: the
// column bd answers with is the reason the caller wrote, not bd's default
// "Closed". A BdStore.Close that stopped forwarding it would leave the column
// at the default, and this test would fail where a fake echoing the metadata
// back on its own would stay green.
func TestFakeBdBackedCloseForwardsTheMetadataReasonToBd(t *testing.T) {
	f := NewFakeBd()
	store := f.NewBdStore()
	created, err := store.Create(beads.Bead{Title: "close forwards its reason", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(created.ID, beads.UpdateOpts{Metadata: map[string]string{"close_reason": "shipped in def456"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(created.ID); err != nil {
		t.Fatal(err)
	}
	out, err := f.Run("", "bd", "show", "--json", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		CloseReason string `json:"close_reason"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("show = %s, %v", out, err)
	}
	if rows[0].CloseReason != "shipped in def456" {
		t.Errorf("bd close_reason column = %q, want the forwarded %q", rows[0].CloseReason, "shipped in def456")
	}
}
