package opjournal

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

const companionKey = "companion_mark"

// companionRow creates the plain row a companion writes.
func companionRow(t *testing.T, mem *beads.MemStore) string {
	t.Helper()
	b, err := mem.Create(beads.Bead{ID: "gc-row", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

// companionOf writes companionKey=held on row id: as a batch item, or
// through store on its own, counting the solo writes.
func companionOf(store beads.Store, id string, writes *int) Companion {
	const value = "held"
	return Companion{
		Tag:  companionKey,
		Item: beads.GuardedItem{Update: &beads.GuardedUpdate{ID: id, Opts: beads.UpdateOpts{Metadata: map[string]string{companionKey: value}}}},
		Write: func() error {
			*writes++
			return store.SetMetadata(id, companionKey, value)
		},
	}
}

func companionMark(t *testing.T, mem *beads.MemStore, id string) string {
	t.Helper()
	b, err := mem.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata[companionKey]
}

func opExists(t *testing.T, mem *beads.MemStore, id string) bool {
	t.Helper()
	_, err := mem.Get(id)
	switch {
	case err == nil:
		return true
	case errors.Is(err, beads.ErrNotFound):
		return false
	default:
		t.Fatal(err)
		return false
	}
}

func TestProposeWithBatchesTheCompanionWithTheOp(t *testing.T) {
	h := newLaneHarness(t)
	row := companionRow(t, h.mem)
	writes := 0
	op, created, err := h.store.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(h.mem, row, &writes))
	if err != nil || !created {
		t.Fatalf("ProposeWith = created %v, %v", created, err)
	}
	batches := h.batcher.batches()
	if len(batches) != 1 || len(batches[0].Items) != 2 {
		t.Fatalf("batches = %+v, want one of two items", batches)
	}
	if it := batches[0].Items; it[0].Create == nil || it[0].Create.ID != op.ID || it[1].Update == nil || it[1].Update.ID != row {
		t.Fatalf("items = %+v, want the op's create then the companion", it)
	}
	if got := companionMark(t, h.mem, row); got != "held" {
		t.Fatalf("companion = %q, want it landed", got)
	}
	if stored := h.get(t, op.ID); stored.Revision != op.Revision {
		t.Fatalf("revision %d, stored %d", op.Revision, stored.Revision)
	}
	if writes != 0 {
		t.Fatalf("the solo write ran %d times beside the batch", writes)
	}
}

func TestProposeWithBatchIsAllOrNothing(t *testing.T) {
	h := newLaneHarness(t)
	writes := 0
	preview, err := h.store.Preview(laneMsg("k1", RolePrompt, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = h.store.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(h.mem, "gc-missing", &writes))
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("ProposeWith over a missing companion row = %v, want ErrNotFound", err)
	}
	if opExists(t, h.mem, preview.ID) {
		t.Fatal("the op landed without its companion")
	}
}

func TestProposeWithLosesARaceAsAReplay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		winner  string
		reused  bool
		created bool
	}{{"same request", "hi", false, false}, {"another payload", "other", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLaneHarness(t)
			row := companionRow(t, h.mem)
			winnerRow, err := h.mem.Create(beads.Bead{ID: "gc-winner-row", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			writes, winnerWrites := 0, 0
			winner := laneMsg("k1", RolePrompt, tc.winner)
			h.batcher.setFault(func(n int, _ beads.GuardedBatch) (bool, error) {
				if n == 0 {
					if _, _, err := h.store.ProposeWith(context.Background(), winner, companionOf(h.mem, winnerRow.ID, &winnerWrites)); err != nil {
						t.Fatal(err)
					}
				}
				return false, nil
			})
			op, created, err := h.store.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(h.mem, row, &writes))
			var reuse *KeyReuseError
			switch {
			case tc.reused && !errors.As(err, &reuse):
				t.Fatalf("ProposeWith = %v, want *KeyReuseError", err)
			case !tc.reused && (err != nil || created || op.Payload != "hi"):
				t.Fatalf("ProposeWith = %+v, created %v, %v; want the stored op as a replay", op, created, err)
			}
			if got := companionMark(t, h.mem, row); got != "" || writes != 0 {
				t.Fatalf("the companion was written (%q, %d solo writes) after a lost race", got, writes)
			}
		})
	}
}

func TestProposeWithUnknownBatchRereads(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		h := newLaneHarness(t)
		row := companionRow(t, h.mem)
		writes := 0
		h.batcher.setFault(func(int, beads.GuardedBatch) (bool, error) { return true, errLinkDropped })
		op, created, err := h.store.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(h.mem, row, &writes))
		if err != nil || created || op.ID == "" {
			t.Fatalf("ProposeWith = %+v, created %v, %v; want the committed op", op, created, err)
		}
		if got := companionMark(t, h.mem, row); got != "held" {
			t.Fatalf("companion = %q, want it committed with the op", got)
		}
	})
	t.Run("not committed", func(t *testing.T) {
		h := newLaneHarness(t)
		row := companionRow(t, h.mem)
		writes := 0
		h.batcher.setFault(func(int, beads.GuardedBatch) (bool, error) { return false, errLinkDropped })
		preview, _ := h.store.Preview(laneMsg("k1", RolePrompt, "hi"))
		_, _, err := h.store.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(h.mem, row, &writes))
		if !errors.Is(err, errLinkDropped) {
			t.Fatalf("ProposeWith = %v, want the batch's error", err)
		}
		if opExists(t, h.mem, preview.ID) || companionMark(t, h.mem, row) != "" || writes != 0 {
			t.Fatal("a batch that did not commit left a row behind")
		}
	})
}

// recordedKinds are the recorder's ops on ids, in order.
func recordedKinds(rec *beadstest.OpRecordingStore, ids ...string) []string {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []string
	for _, op := range rec.Ops() {
		if want[op.ID] {
			out = append(out, op.Kind+":"+op.ID)
		}
	}
	return out
}

func TestProposeWithWritesTheCompanionFirstWithoutBatches(t *testing.T) {
	h := newHarness(t)
	row := companionRow(t, h.mem)
	writes := 0
	op, created, err := h.store.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(h.rec, row, &writes))
	if err != nil || !created {
		t.Fatalf("ProposeWith = created %v, %v", created, err)
	}
	var order []string
	for _, k := range recordedKinds(h.rec, row, op.ID) {
		if k == beadstest.OpSetMetadata+":"+row || k == beadstest.OpCreate+":"+op.ID {
			order = append(order, k)
		}
	}
	if len(order) != 2 || order[0] != beadstest.OpSetMetadata+":"+row {
		t.Fatalf("write order = %v, want the companion before the op's create", order)
	}
	if writes != 1 || companionMark(t, h.mem, row) != "held" {
		t.Fatalf("companion = %q after %d writes", companionMark(t, h.mem, row), writes)
	}
}

func TestProposeWithProposesNothingWhenTheCompanionFails(t *testing.T) {
	h := newHarness(t)
	errRow := errors.New("row write refused")
	preview, _ := h.store.Preview(laneMsg("k1", RolePrompt, "hi"))
	with := Companion{
		Tag:   companionKey,
		Item:  beads.GuardedItem{Update: &beads.GuardedUpdate{ID: "gc-row"}},
		Write: func() error { return errRow },
	}
	if _, _, err := h.store.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), with); !errors.Is(err, errRow) {
		t.Fatalf("ProposeWith = %v, want the companion's error", err)
	}
	if opExists(t, h.mem, preview.ID) {
		t.Fatal("the op was proposed without its companion")
	}
}

func TestProposeWithKeepsTheCompanionWhenProposeFails(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	h := newHarnessOver(t, mem, typeRefusingStore{mem})
	row := companionRow(t, mem)
	writes := 0
	if _, _, err := h.store.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(mem, row, &writes)); !errors.Is(err, ErrBeadTypeUnregistered) {
		t.Fatalf("ProposeWith = %v, want the create's refusal", err)
	}
	if got := companionMark(t, mem, row); got != "held" || writes != 1 {
		t.Fatalf("companion = %q after %d writes, want the harmless half written", got, writes)
	}
}

func TestProposeWithReplaysWithoutTheCompanion(t *testing.T) {
	run := func(t *testing.T, mem *beads.MemStore, st *Store, batches func() int) {
		row := companionRow(t, mem)
		writes := 0
		first, created, err := st.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(mem, row, &writes))
		if err != nil || !created {
			t.Fatalf("first = created %v, %v", created, err)
		}
		if err := mem.SetMetadata(row, companionKey, ""); err != nil { // a resume cleared it since
			t.Fatal(err)
		}
		writes, before := 0, batches()
		again, created, err := st.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(mem, row, &writes))
		if err != nil || created || again.ID != first.ID {
			t.Fatalf("replay = %+v, created %v, %v; want the stored op", again, created, err)
		}
		var reuse *KeyReuseError
		if _, _, err := st.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "other"), companionOf(mem, row, &writes)); !errors.As(err, &reuse) {
			t.Fatalf("key reuse = %v, want *KeyReuseError", err)
		}
		if writes != 0 || batches() != before || companionMark(t, mem, row) != "" {
			t.Fatalf("a replay wrote the companion (%d solo writes, %d batches, cell %q)", writes, batches()-before, companionMark(t, mem, row))
		}
	}
	t.Run("batch", func(t *testing.T) {
		h := newLaneHarness(t)
		run(t, h.mem, h.store, func() int { return len(h.batcher.batches()) })
	})
	t.Run("fallback", func(t *testing.T) {
		h := newHarness(t)
		run(t, h.mem, h.store, func() int { return 0 })
	})
}

// TestProposeWithCompanionIsPartOfTheRequest: the companion's tag is in the
// request digest, so a key first proposed without a companion, or with
// another one, is a *KeyReuseError and writes nothing; a hold-less Stop
// cannot make a later held Stop on its key a silent replay.
func TestProposeWithCompanionIsPartOfTheRequest(t *testing.T) {
	run := func(t *testing.T, mem *beads.MemStore, st *Store, batches func() int) {
		row := companionRow(t, mem)
		writes := 0
		plain, created, err := st.Propose(laneMsg("k1", RolePrompt, "hi"))
		if err != nil || !created {
			t.Fatalf("Propose = created %v, %v", created, err)
		}
		before := batches()
		var reuse *KeyReuseError
		if _, _, err := st.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi"), companionOf(mem, row, &writes)); !errors.As(err, &reuse) || reuse.Existing.ID != plain.ID {
			t.Fatalf("ProposeWith over a plain op = %v, want *KeyReuseError naming %s", err, plain.ID)
		}
		if writes != 0 || batches() != before || companionMark(t, mem, row) != "" {
			t.Fatalf("a held Stop over a plain op wrote the companion (%d solo writes, %d batches)", writes, batches()-before)
		}
		other := companionOf(mem, row, &writes)
		other.Tag = "another_companion"
		if _, _, err := st.ProposeWith(context.Background(), laneMsg("k2", RolePrompt, "hi"), companionOf(mem, row, &writes)); err != nil {
			t.Fatal(err)
		}
		if err := mem.SetMetadata(row, companionKey, ""); err != nil {
			t.Fatal(err)
		}
		writes, before = 0, batches()
		if _, _, err := st.ProposeWith(context.Background(), laneMsg("k2", RolePrompt, "hi"), other); !errors.As(err, &reuse) {
			t.Fatalf("ProposeWith with another companion = %v, want *KeyReuseError", err)
		}
		if _, _, err := st.Propose(laneMsg("k2", RolePrompt, "hi")); !errors.As(err, &reuse) {
			t.Fatalf("Propose over an op proposed with a companion = %v, want *KeyReuseError", err)
		}
		if writes != 0 || batches() != before || companionMark(t, mem, row) != "" {
			t.Fatalf("a reused key wrote the companion (%d solo writes, %d batches, cell %q)", writes, batches()-before, companionMark(t, mem, row))
		}
	}
	t.Run("batch", func(t *testing.T) {
		h := newLaneHarness(t)
		run(t, h.mem, h.store, func() int { return len(h.batcher.batches()) })
	})
	t.Run("fallback", func(t *testing.T) {
		h := newHarness(t)
		run(t, h.mem, h.store, func() int { return 0 })
	})
}

func TestProposeWithRefusesBeforeWriting(t *testing.T) {
	h := newLaneHarness(t)
	row := companionRow(t, h.mem)
	preview, _ := h.store.Preview(laneMsg("k1", RolePrompt, "hi"))
	writes := 0
	good := companionOf(h.mem, row, &writes)
	cases := map[string]struct {
		req  Request
		with Companion
	}{
		"empty client key": {laneMsg("", RolePrompt, "hi"), good},
		"no item":          {laneMsg("k1", RolePrompt, "hi"), Companion{Tag: good.Tag, Write: good.Write}},
		"two items": {laneMsg("k1", RolePrompt, "hi"), Companion{
			Tag: good.Tag, Item: beads.GuardedItem{Create: &beads.Bead{ID: "gc-x"}, Update: good.Item.Update}, Write: good.Write,
		}},
		"nil write":       {laneMsg("k1", RolePrompt, "hi"), Companion{Tag: good.Tag, Item: good.Item}},
		"no tag":          {laneMsg("k1", RolePrompt, "hi"), Companion{Item: good.Item, Write: good.Write}},
		"spaced tag":      {laneMsg("k1", RolePrompt, "hi"), Companion{Tag: " hold", Item: good.Item, Write: good.Write}},
		"names the op id": {laneMsg("k1", RolePrompt, "hi"), companionOf(h.mem, preview.ID, &writes)},
	}
	for name, tc := range cases {
		if _, _, err := h.store.ProposeWith(context.Background(), tc.req, tc.with); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: ProposeWith = %v, want ErrInvalidRequest", name, err)
		}
	}
	if writes != 0 || len(h.batcher.batches()) != 0 || companionMark(t, h.mem, row) != "" || opExists(t, h.mem, preview.ID) {
		t.Fatalf("a refused proposal wrote (%d solo writes, %d batches)", writes, len(h.batcher.batches()))
	}
}

func TestProposeWithMintedKeySkipsTheRead(t *testing.T) {
	minted := func(r *Request) { r.MintedKey = true }
	check := func(t *testing.T, rec *beadstest.OpRecordingStore, mem *beads.MemStore, st *Store) {
		row := companionRow(t, mem)
		writes := 0
		op, created, err := st.ProposeWith(context.Background(), laneMsg("k1", RolePrompt, "hi", minted), companionOf(mem, row, &writes))
		if err != nil || !created {
			t.Fatalf("ProposeWith = created %v, %v", created, err)
		}
		for _, k := range recordedKinds(rec, op.ID) {
			if k == beadstest.OpGet+":"+op.ID {
				t.Fatalf("a minted key read the op first: %v", recordedKinds(rec, op.ID))
			}
		}
	}
	t.Run("batch", func(t *testing.T) {
		h := newLaneHarness(t)
		check(t, h.rec, h.mem, h.store)
	})
	t.Run("fallback", func(t *testing.T) {
		h := newHarness(t)
		check(t, h.rec, h.mem, h.store)
	})
}
