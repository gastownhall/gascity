package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// A split work store can contain only a claimed input: its graph dependencies
// live in another class store, so no blocking edge ever witnesses this scope's
// inline dependency projection. Keep BdStore's real list/prime/enrichment path;
// change only the listed native issue fields to the selected status fixture.
func primedBdCacheWithoutReadyCandidates(t *testing.T, inputs []bdIssue) (*CachingStore, *BdStore, *fakeBdLedger) {
	t.Helper()
	ledger := &fakeBdLedger{}
	for _, input := range inputs {
		ledger.rows = append(ledger.rows, bdLedgerRow{id: input.ID})
	}
	run := func(dir, name string, args ...string) ([]byte, error) {
		out, err := ledger.run(dir, name, args...)
		if err != nil {
			return nil, err
		}
		if args[0] == "version" {
			return []byte("bd version 1.3.2\n"), nil
		}
		if args[0] != "list" && args[0] != "query" {
			return out, nil
		}
		return noncandidateReadyNativeRows(out, inputs)
	}
	scope := t.TempDir()
	// Validate with the production native parser before Prime can enter its
	// unchanged retry/backoff path. A malformed unit fixture fails immediately.
	for _, verb := range []string{"list", "query"} {
		out, err := run(scope, "bd", verb, "--json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseIssuesTolerant(out); err != nil {
			t.Fatalf("invalid native %s fixture before Prime: %v", verb, err)
		}
	}
	store := NewBdStore(scope, run)
	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.listIncludesCompleteDependencies() {
		t.Fatal("edge-free rows must not invent a dependency-projection witness")
	}
	cache.mu.RLock()
	state, count, complete := cache.state, len(cache.beads), cache.depsComplete
	cache.mu.RUnlock()
	if state != cacheLive || count != len(inputs) || complete {
		t.Fatalf("fixture = state %v, rows %d, depsComplete %v; want complete nonempty row census without dependency witness", state, count, complete)
	}
	return cache, store, ledger
}

// Preserve the native wire representation rather than serializing bdIssue:
// it is a decoder DTO and its flexible scalar fields are not JSON encoders.
func noncandidateReadyNativeRows(out []byte, inputs []bdIssue) ([]byte, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	for i := range rows {
		var id string
		if err := json.Unmarshal(rows[i]["id"], &id); err != nil {
			return nil, err
		}
		for _, input := range inputs {
			if id != input.ID {
				continue
			}
			fields, err := json.Marshal(struct {
				Status    string   `json:"status"`
				IssueType string   `json:"issue_type"`
				Labels    []string `json:"labels"`
				Assignee  string   `json:"assignee"`
			}{Status: input.Status, IssueType: input.IssueType, Labels: input.Labels, Assignee: input.Assignee})
			if err != nil {
				return nil, err
			}
			var patch map[string]json.RawMessage
			if err := json.Unmarshal(fields, &patch); err != nil {
				return nil, err
			}
			for key, value := range patch {
				rows[i][key] = value
			}
		}
	}
	return json.Marshal(rows)
}

func TestCachingStoreReadyContextClaimedInputWithoutDependencyWitness(t *testing.T) {
	cache, store, ledger := primedBdCacheWithoutReadyCandidates(t, []bdIssue{{ID: "of-input", Status: "in_progress", IssueType: "task"}})
	ledger.mu.Lock()
	before := len(ledger.calls)
	ledger.mu.Unlock()
	for range 3 {
		rows, err := cache.ReadyContext(context.Background())
		if err != nil || len(rows) != 0 {
			t.Fatalf("ReadyContext = %+v, %v; a full clean claimed-only snapshot proves no ready candidate", rows, err)
		}
	}
	ledger.mu.Lock()
	after := len(ledger.calls)
	ledger.mu.Unlock()
	cache.mu.RLock()
	complete := cache.depsComplete
	cache.mu.RUnlock()
	if before != after || complete || store.listIncludesCompleteDependencies() {
		t.Fatalf("empty readiness spent extra calls or invented dependency completeness: before=%d after=%d depsComplete=%v", before, after, complete)
	}
}

func TestCachingStoreReadyContextNoncandidateCensusUsesNormalEligibility(t *testing.T) {
	cases := []struct {
		name  string
		input bdIssue
		query ReadyQuery
	}{
		{"in progress", bdIssue{ID: "of-input", Status: "in_progress", IssueType: "task"}, ReadyQuery{}},
		{"deferred", bdIssue{ID: "of-input", Status: "deferred", IssueType: "task"}, ReadyQuery{}},
		{"excluded label", bdIssue{ID: "of-input", Status: "open", IssueType: "task", Labels: []string{"gc:session"}}, ReadyQuery{}},
		{"other assignee", bdIssue{ID: "of-input", Status: "open", IssueType: "task", Assignee: "first"}, ReadyQuery{Assignee: "second"}},
		{"other tier", bdIssue{ID: "of-input", Status: "open", IssueType: "task"}, ReadyQuery{TierMode: TierWisps}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache, _, _ := primedBdCacheWithoutReadyCandidates(t, []bdIssue{tc.input})
			rows, err := cache.cachedReadyCompleteOnly(context.Background(), tc.query)
			if err != nil || len(rows) != 0 {
				t.Fatalf("filtered ready = %+v, %v; want proven no eligible candidate", rows, err)
			}
		})
	}
}

func TestCachingStoreReadyContextNoncandidateCensusKeepsUnknownGuards(t *testing.T) {
	cases := []struct {
		name   string
		change func(*CachingStore)
	}{
		{"eligible open candidate", func(c *CachingStore) {
			b := c.beads["of-input"]
			b.Status = "open"
			c.beads[b.ID] = b
		}},
		{"dirty row", func(c *CachingStore) { c.dirty["of-input"] = struct{}{} }},
		{"partial active census", func(c *CachingStore) { c.state = cachePartial }},
		{"uninitialized", func(c *CachingStore) { c.state = cacheUninitialized }},
		{"partial list", func(c *CachingStore) { c.primePartialErr = errors.New("incomplete native list") }},
		{"degraded ready projection", func(c *CachingStore) { c.readyProjectionDegraded.Store(true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache, _, _ := primedBdCacheWithoutReadyCandidates(t, []bdIssue{{ID: "of-input", Status: "in_progress", IssueType: "task"}})
			cache.mu.Lock()
			tc.change(cache)
			cache.mu.Unlock()
			rows, err := cache.ReadyContext(context.Background())
			if !errors.Is(err, ErrCacheUnavailable) || len(rows) != 0 {
				t.Fatalf("ReadyContext = %+v, %v; uncertainty must not become an empty success", rows, err)
			}
		})
	}
	t.Run("canceled context", func(t *testing.T) {
		cache, _, _ := primedBdCacheWithoutReadyCandidates(t, []bdIssue{{ID: "of-input", Status: "in_progress", IssueType: "task"}})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if rows, err := cache.ReadyContext(ctx); !errors.Is(err, context.Canceled) || len(rows) != 0 {
			t.Fatalf("canceled ready = %+v, %v", rows, err)
		}
	})
}

func TestCachingStoreNoncandidateFixturePreservesNativeWireFields(t *testing.T) {
	raw := []byte(`[{"id":"of-input","status":"open","issue_type":"task","ephemeral":true,"no_history":false,"is_blocked":false,"created_at":"2026-11-04T06:00:00Z","updated_at":"2026-11-04T06:00:01Z","dependency_count":0,"dependencies":[],"metadata":{"native":"keep"}}, {"id":"of-other","status":"open","issue_type":"task","ephemeral":false,"is_blocked":true}]`)
	out, err := noncandidateReadyNativeRows(raw, []bdIssue{{ID: "of-input", Status: "in_progress", IssueType: "task", Labels: []string{"owned-fixture"}, Assignee: "first"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseIssuesTolerant(out); err != nil {
		t.Fatalf("patched native rows do not parse: %v", err)
	}
	var before, after []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("fixture changed native row census: before=%d after=%d", len(before), len(after))
	}
	for i, row := range before {
		for key, value := range row {
			if i == 0 && (key == "status" || key == "issue_type" || key == "labels" || key == "assignee") {
				continue
			}
			if string(after[i][key]) != string(value) {
				t.Fatalf("fixture changed native row%d field%s: before=%s after=%s", i, key, value, after[i][key])
			}
		}
	}
	if string(after[0]["status"]) != `"in_progress"` || string(after[1]["status"]) != `"open"` {
		t.Fatalf("fixture did not patch only its selected identity: %s", out)
	}
}

func TestCachingStoreNoncandidateFixtureRejectsMalformedNativeBoolBeforePrime(t *testing.T) {
	out, err := noncandidateReadyNativeRows([]byte(`[{"id":"of-input","status":"open","issue_type":"task","is_blocked":{}}]`), []bdIssue{{ID: "of-input", Status: "in_progress", IssueType: "task"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseIssuesTolerant(out); err == nil {
		t.Fatal("native parser accepted malformed bool; fixture would reach production Prime retries")
	}
}
