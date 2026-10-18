package beads_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func hasKeyIDs(items []beads.Bead) []string {
	ids := make([]string, 0, len(items))
	for _, b := range items {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}

// hasKeyCorpus creates three beads: one carrying the key, one carrying it
// empty, one without it. Only the first matches HasMetadataKey.
func hasKeyCorpus(t *testing.T, store beads.Store) string {
	t.Helper()
	with, err := store.Create(beads.Bead{Title: "with", Metadata: map[string]string{"gc.root_bead_id": "r1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{Title: "empty", Metadata: map[string]string{"gc.root_bead_id": ""}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{Title: "without", Metadata: map[string]string{"other": "x"}}); err != nil {
		t.Fatal(err)
	}
	return with.ID
}

func TestHasMetadataKeyIsAFilter(t *testing.T) {
	if !(beads.ListQuery{HasMetadataKey: "gc.root_bead_id"}).HasFilter() {
		t.Fatal("a HasMetadataKey query must count as a filter, so it needs no AllowScan")
	}
}

func TestMemStoreListHasMetadataKey(t *testing.T) {
	store := beads.NewMemStore()
	want := hasKeyCorpus(t, store)
	got, err := store.List(beads.ListQuery{HasMetadataKey: "gc.root_bead_id"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := hasKeyIDs(got); len(ids) != 1 || ids[0] != want {
		t.Fatalf("List(HasMetadataKey) = %v, want only %s", ids, want)
	}
}

func TestSQLiteStoreListHasMetadataKey(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := hasKeyCorpus(t, store)
	for _, limit := range []int{0, 1} {
		got, err := store.List(beads.ListQuery{HasMetadataKey: "gc.root_bead_id", Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		if ids := hasKeyIDs(got); len(ids) != 1 || ids[0] != want {
			t.Fatalf("List(HasMetadataKey, limit %d) = %v, want only %s", limit, ids, want)
		}
	}
}

// TestBdStoreListHasMetadataKeyPushesTheFlag: the key goes to bd as
// --has-metadata-key, bd reads with no row limit because its "set" admits an
// empty value, and the empty-valued row is cut Go-side before the caller's
// limit applies. The ephemeral leg of a both-tier read is filtered the same way.
func TestBdStoreListHasMetadataKeyPushesTheFlag(t *testing.T) {
	var calls []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		calls = append(calls, cmd)
		if strings.Contains(cmd, " query ") {
			return []byte(`[
				{"id":"bd-eph","title":"eph","status":"closed","issue_type":"task","created_at":"2026-05-04T00:00:00Z","ephemeral":true,"metadata":{"gc.root_bead_id":"r2"}},
				{"id":"bd-eph-none","title":"eph none","status":"closed","issue_type":"task","created_at":"2026-05-05T00:00:00Z","ephemeral":true,"metadata":{}}
			]`), nil
		}
		return []byte(`[
			{"id":"bd-empty","title":"empty","status":"closed","issue_type":"task","created_at":"2026-05-03T00:00:00Z","metadata":{"gc.root_bead_id":""}},
			{"id":"bd-step","title":"step","status":"closed","issue_type":"task","created_at":"2026-05-01T00:00:00Z","metadata":{"gc.root_bead_id":"r1"}}
		]`), nil
	}
	s := beads.NewBdStore("/city", runner)
	got, err := s.List(beads.ListQuery{
		HasMetadataKey: "gc.root_bead_id",
		IncludeClosed:  true,
		TierMode:       beads.TierBoth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := hasKeyIDs(got); strings.Join(ids, ",") != "bd-eph,bd-step" {
		t.Fatalf("List(HasMetadataKey) = %v, want bd-eph and bd-step", ids)
	}
	var list string
	for _, cmd := range calls {
		if strings.Contains(cmd, " list ") {
			list = cmd
		}
	}
	if !strings.Contains(list, "--has-metadata-key gc.root_bead_id") || !strings.Contains(list, "--all") || !strings.Contains(list, "--limit 0") {
		t.Fatalf("bd list = %q, want --has-metadata-key gc.root_bead_id, --all and --limit 0", list)
	}

	calls = nil
	got, err = s.List(beads.ListQuery{HasMetadataKey: "gc.root_bead_id", IncludeClosed: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "bd-step" {
		t.Fatalf("List(HasMetadataKey, limit 1) = %v, want bd-step after the empty-valued row is cut", hasKeyIDs(got))
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "--limit 0") {
		t.Fatalf("bd calls = %q, want one bd list with --limit 0", calls)
	}
}

// TestBdStoreListSkipTextAsksForBrief: a caller that reads no free-form text
// asks bd for --brief, which leaves the description out of every row.
func TestBdStoreListSkipTextAsksForBrief(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`[]`), nil
	}
	s := beads.NewBdStore("/city", runner)
	if _, err := s.List(beads.ListQuery{HasMetadataKey: "gc.root_bead_id", SkipText: true}); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(gotArgs, " "); !strings.Contains(args, "--brief") {
		t.Fatalf("args = %q, want --brief", args)
	}
	if _, err := s.List(beads.ListQuery{HasMetadataKey: "gc.root_bead_id"}); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(gotArgs, " "); strings.Contains(args, "--brief") {
		t.Fatalf("args = %q, want no --brief when the caller reads text", args)
	}
}
