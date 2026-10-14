//go:build integration

package beads

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// The list pushdown against REAL upstream native storage. The unit property
// proves the plan against an oracle of the reader role; this proves the role
// itself agrees: every query answers exactly what ApplyListQuery answers over
// the whole ledger, with and without the scope's type vocabulary registered.

func seedPushdownLedger(t *testing.T, store *NativeDoltStore) {
	t.Helper()
	type seed struct {
		typ, assignee, status string
		ephemeral             bool
		labels                []string
	}
	var seeds []seed
	for _, typ := range []string{"message", "session", "molecule", "step", "task"} {
		for _, assignee := range []string{"", "planner-a", "gc-route", "worker"} {
			for _, status := range []string{"open", "in_progress", "closed"} {
				for _, ephemeral := range []bool{false, true} {
					s := seed{typ: typ, assignee: assignee, status: status, ephemeral: ephemeral}
					if typ == "session" {
						s.labels = []string{"gc:session"}
					}
					seeds = append(seeds, s)
				}
			}
		}
	}
	for i, s := range seeds {
		created, err := store.Create(Bead{
			Title:     fmt.Sprintf("%s %s %s %d", s.typ, s.assignee, s.status, i),
			Type:      s.typ,
			Assignee:  s.assignee,
			Ephemeral: s.ephemeral,
			Labels:    s.labels,
		})
		if err != nil {
			t.Fatalf("Create(%+v): %v", s, err)
		}
		switch s.status {
		case "in_progress":
			status := s.status
			if err := store.Update(created.ID, UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("Update(%s, in_progress): %v", created.ID, err)
			}
		case "closed":
			if err := store.Close(created.ID); err != nil {
				t.Fatalf("Close(%s): %v", created.ID, err)
			}
		}
	}
}

func pushdownIntegrationQueries() []ListQuery {
	routes := []string{"planner-a", "gc-route"}
	var out []ListQuery
	for _, tier := range []TierMode{TierIssues, TierWisps, TierBoth} {
		out = append(out,
			ListQuery{Type: "message", Status: "open", Assignees: routes, TierMode: tier},
			ListQuery{Type: "message", Assignees: routes, IncludeClosed: true, TierMode: tier},
			ListQuery{Type: "molecule", Status: "in_progress", Assignees: routes, TierMode: tier},
			ListQuery{Status: "in_progress", Assignees: routes, TierMode: tier},
			ListQuery{Type: "session", Status: "open", TierMode: tier},
			ListQuery{Type: "session", Status: "closed", TierMode: tier},
			ListQuery{Label: "gc:session", Status: "closed", IncludeClosed: true, TierMode: tier},
			ListQuery{Type: "step", Assignee: "worker", Status: "in_progress", TierMode: tier},
			ListQuery{Type: "task", Assignees: []string{"worker", ""}, IncludeClosed: true, TierMode: tier},
		)
	}
	return out
}

func assertPushdownAnswersTheUnpushedQuery(t *testing.T, store *NativeDoltStore) {
	t.Helper()
	all, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) == 0 {
		t.Fatal("seeded ledger listed empty")
	}
	for _, query := range pushdownIntegrationQueries() {
		got, err := store.List(query)
		if err != nil {
			t.Fatalf("List(%+v): %v", query, err)
		}
		want := ApplyListQuery(all, query)
		if gotIDs, wantIDs := sortedBeadIDs(got), sortedBeadIDs(want); !slices.Equal(gotIDs, wantIDs) {
			t.Errorf("List(%+v)\n got  %v\n want %v", query, gotIDs, wantIDs)
		}
	}
}

func TestNativeDoltStoreListPushdownAgainstRealStorage(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "pushdown-test")
	if err := store.storage.SetConfig(context.Background(), "types.custom", strings.Join(RequiredCustomTypes, ",")); err != nil {
		t.Fatalf("register gc types: %v", err)
	}
	store.loadListPushableTypes(context.Background())
	var logs bytes.Buffer
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	seedPushdownLedger(t, store)

	assertPushdownAnswersTheUnpushedQuery(t, store)
	if store.typePushdownOff.Load() {
		t.Fatalf("a scope carrying gc's types refused a pushed type:\n%s", logs.String())
	}
	// The whole-ledger reads are the test's own List(all) and the queries
	// whose plan cannot be keyed (an "unassigned" assignee with an unpushable
	// type); every other query must have been served by keyed requests.
	wantUnkeyed := int64(1)
	for _, query := range pushdownIntegrationQueries() {
		for _, req := range NativeListPlan(query) {
			if !NativeListRequestKeyed(req) {
				wantUnkeyed++
			}
		}
	}
	if stats := listRequestStatsForTest(t, store); stats.Unkeyed != wantUnkeyed {
		t.Fatalf("ListRequestStatsOf = %+v, want %d whole-ledger reads", stats, wantUnkeyed)
	}
}

// A scope that has lost gc's types refuses a pushed type. The answers stay
// exact, the store falls back once and says so. The rows are written while the
// vocabulary is registered (the write path validates too) and the registration
// is then withdrawn.
func TestNativeDoltStoreListPushdownFallsBackOnAScopeWithoutGCTypes(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "pushdown-test")
	ctx := context.Background()
	if err := store.storage.SetConfig(ctx, "types.custom", strings.Join(RequiredCustomTypes, ",")); err != nil {
		t.Fatalf("register gc types: %v", err)
	}
	seedPushdownLedger(t, store)
	if err := store.storage.SetConfig(ctx, "types.custom", "message"); err != nil {
		t.Fatalf("withdraw gc types: %v", err)
	}
	store.loadListPushableTypes(ctx)
	var logs bytes.Buffer
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	assertPushdownAnswersTheUnpushedQuery(t, store)
	if !store.typePushdownOff.Load() {
		t.Skip("this storage accepts unregistered types on a listing; the fallback is covered by the unit tier")
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, logs.String())
	}
}

// A scope whose types.infra setting names a type gc would otherwise push. The
// server routes a listing naming it to the ephemeral plane alone, so a pushed
// type would lose that type's durable rows. Every pushable type is seeded in
// both tiers while the setting is unset (a durable create of an infra type is
// routed to the wisps), the setting is then written, and every pushable type's
// listings must still answer exactly what the unpushed query answers.
func TestNativeDoltStoreListPushdownHonorsTheScopeInfraTypes(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "pushdown-test")
	ctx := context.Background()
	if err := store.storage.SetConfig(ctx, "types.custom", strings.Join(RequiredCustomTypes, ",")); err != nil {
		t.Fatalf("register gc types: %v", err)
	}
	var pushable []string
	for _, typ := range RequiredCustomTypes {
		if nativeListDefaultPushableTypes[typ] {
			pushable = append(pushable, typ)
		}
	}
	if len(pushable) < 2 {
		t.Fatalf("pushable set %v is too small to make one of them infra and keep another pushed", pushable)
	}
	for i, typ := range pushable {
		for _, ephemeral := range []bool{false, true} {
			for _, status := range []string{"open", "in_progress"} {
				created, err := store.Create(Bead{
					Title:     fmt.Sprintf("%s %s %v %d", typ, status, ephemeral, i),
					Type:      typ,
					Assignee:  "worker",
					Ephemeral: ephemeral,
				})
				if err != nil {
					t.Fatalf("Create(%s, ephemeral=%v): %v", typ, ephemeral, err)
				}
				if created.Ephemeral != ephemeral {
					t.Fatalf("Create(%s, ephemeral=%v) landed ephemeral=%v; the seed is not in both tiers", typ, ephemeral, created.Ephemeral)
				}
				if status == "in_progress" {
					if err := store.Update(created.ID, UpdateOpts{Status: &status}); err != nil {
						t.Fatalf("Update(%s, in_progress): %v", created.ID, err)
					}
				}
			}
		}
	}
	infra := pushable[0]
	if err := store.storage.SetConfig(ctx, nativeTypesInfraConfigKey, "agent,role,message,"+infra); err != nil {
		t.Fatalf("set types.infra: %v", err)
	}
	store.loadListPushableTypes(ctx)
	if store.listPushableTypes[infra] {
		t.Fatalf("the store still pushes %q, which its scope makes infra", infra)
	}
	if !store.listPushableTypes[pushable[1]] {
		t.Fatalf("the store stopped pushing %q, which its scope keeps durable", pushable[1])
	}

	all, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	for _, typ := range pushable {
		for _, query := range []ListQuery{
			{Type: typ, TierMode: TierBoth},
			{Type: typ, TierMode: TierIssues},
			{Type: typ, Status: "in_progress", TierMode: TierBoth},
			{Type: typ, Status: "open", Assignees: []string{"worker", "planner-a"}, TierMode: TierBoth},
		} {
			got, err := store.List(query)
			if err != nil {
				t.Fatalf("List(%+v): %v", query, err)
			}
			want := ApplyListQuery(all, query)
			if len(want) == 0 {
				t.Fatalf("List(%+v): the unpushed answer is empty; the seed does not exercise it", query)
			}
			if gotIDs, wantIDs := sortedBeadIDs(got), sortedBeadIDs(want); !slices.Equal(gotIDs, wantIDs) {
				t.Errorf("List(%+v)\n got  %v\n want %v", query, gotIDs, wantIDs)
			}
		}
	}
}
