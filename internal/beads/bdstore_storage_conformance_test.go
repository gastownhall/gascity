package beads_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreCreateStorageFlagConformance(t *testing.T) {
	cases := []struct {
		name             string
		input            beads.Bead
		storage          beads.StorageClass
		wantEphemeral    bool
		wantNoHistory    bool
		wantCreatedFlags string
	}{
		{
			name:  "default history",
			input: beads.Bead{Title: "plain"},
		},
		{
			name:             "default honors legacy ephemeral field",
			input:            beads.Bead{Title: "legacy ephemeral", Ephemeral: true},
			wantEphemeral:    true,
			wantCreatedFlags: "ephemeral",
		},
		{
			name:             "default honors legacy no-history field",
			input:            beads.Bead{Title: "legacy no-history", NoHistory: true},
			wantNoHistory:    true,
			wantCreatedFlags: "no-history",
		},
		{
			name:    "policy history clears caller storage hints",
			input:   beads.Bead{Title: "forced history", Ephemeral: true, NoHistory: true},
			storage: beads.StorageHistory,
		},
		{
			name:             "policy no-history overrides caller ephemeral hint",
			input:            beads.Bead{Title: "forced no-history", Ephemeral: true},
			storage:          beads.StorageNoHistory,
			wantNoHistory:    true,
			wantCreatedFlags: "no-history",
		},
		{
			name:             "policy ephemeral overrides caller no-history hint",
			input:            beads.Bead{Title: "forced ephemeral", NoHistory: true},
			storage:          beads.StorageEphemeral,
			wantEphemeral:    true,
			wantCreatedFlags: "ephemeral",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			runner := func(_, name string, args ...string) ([]byte, error) {
				if name != "bd" {
					t.Fatalf("runner name = %q, want bd", name)
				}
				gotArgs = append([]string(nil), args...)
				payload := fmt.Sprintf(
					`{"id":"bd-x","title":%q,"status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z","ephemeral":%t,"no_history":%t}`,
					tc.input.Title,
					tc.wantEphemeral,
					tc.wantNoHistory,
				)
				return []byte(payload), nil
			}
			store := beads.NewBdStore("/city", runner)

			created, err := store.CreateWithStorage(tc.input, tc.storage)
			if err != nil {
				t.Fatalf("CreateWithStorage: %v", err)
			}

			assertArgPresence(t, gotArgs, "--ephemeral", tc.wantEphemeral)
			assertArgPresence(t, gotArgs, "--no-history", tc.wantNoHistory)
			if created.Ephemeral != tc.wantEphemeral || created.NoHistory != tc.wantNoHistory {
				t.Fatalf("created storage = ephemeral:%v no_history:%v, want ephemeral:%v no_history:%v",
					created.Ephemeral, created.NoHistory, tc.wantEphemeral, tc.wantNoHistory)
			}
			switch tc.wantCreatedFlags {
			case "":
				if created.Ephemeral || created.NoHistory {
					t.Fatalf("created storage flags = ephemeral:%v no_history:%v, want history", created.Ephemeral, created.NoHistory)
				}
			case "ephemeral":
				if !created.Ephemeral || created.NoHistory {
					t.Fatalf("created storage flags = ephemeral:%v no_history:%v, want ephemeral only", created.Ephemeral, created.NoHistory)
				}
			case "no-history":
				if created.Ephemeral || !created.NoHistory {
					t.Fatalf("created storage flags = ephemeral:%v no_history:%v, want no-history only", created.Ephemeral, created.NoHistory)
				}
			default:
				t.Fatalf("unknown expected created flags %q", tc.wantCreatedFlags)
			}
		})
	}
}

func TestBdStoreCreateFullFlagConformance(t *testing.T) {
	deferUntil := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	priority := 1
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(`{
			"id":"gc-explicit",
			"title":"full create",
			"status":"open",
			"issue_type":"feature",
			"priority":1,
			"created_at":"2026-05-01T00:00:00Z",
			"assignee":"worker",
			"parent":"bd-parent",
			"description":"body",
			"labels":["alpha","beta"],
			"needs":["bd-dep","validates:bd-check"],
			"metadata":{"from":"sender","gc.kind":"session","route":"a/b"},
			"defer_until":"2026-06-01T12:30:00Z",
			"no_history":true
		}`), nil
	}
	store := beads.NewBdStore("/city", runner)

	created, err := store.CreateWithStorage(beads.Bead{
		ID:          "gc-explicit",
		Title:       "full create",
		Type:        "feature",
		Priority:    &priority,
		Description: "body",
		Assignee:    "worker",
		Needs:       []string{"bd-dep", "validates:bd-check"},
		Labels:      []string{"alpha", "beta"},
		ParentID:    "bd-parent",
		From:        "sender",
		Metadata:    map[string]string{"gc.kind": "session", "route": "a/b"},
		DeferUntil:  &deferUntil,
	}, beads.StorageNoHistory)
	if err != nil {
		t.Fatalf("CreateWithStorage: %v", err)
	}

	wantPairs := map[string]string{
		"--id":          "gc-explicit",
		"-t":            "feature",
		"--priority":    "1",
		"--description": "body",
		"--assignee":    "worker",
		"--deps":        "bd-dep,validates:bd-check",
		"--labels":      "alpha,beta",
		"--parent":      "bd-parent",
		"--defer":       deferUntil.Format(time.RFC3339),
	}
	for flag, value := range wantPairs {
		assertArgPair(t, gotArgs, flag, value)
	}
	assertArgPresence(t, gotArgs, "--no-history", true)
	assertArgPresence(t, gotArgs, "--ephemeral", false)

	metadata := metadataArg(t, gotArgs)
	if metadata["from"] != "sender" || metadata["gc.kind"] != "session" || metadata["route"] != "a/b" {
		t.Fatalf("metadata arg = %#v, want from/gc.kind/route preserved", metadata)
	}
	if !created.NoHistory || created.Ephemeral {
		t.Fatalf("created storage = ephemeral:%v no_history:%v, want no-history", created.Ephemeral, created.NoHistory)
	}
}

func TestBdStoreCreateStorageRejectsInvalidConformance(t *testing.T) {
	cases := []struct {
		name    string
		input   beads.Bead
		storage beads.StorageClass
		want    string
	}{
		{
			name:  "default rejects mutually exclusive caller flags",
			input: beads.Bead{Title: "bad", Ephemeral: true, NoHistory: true},
			want:  "mutually exclusive",
		},
		{
			name:    "unknown policy storage class",
			input:   beads.Bead{Title: "bad"},
			storage: beads.StorageClass("archive"),
			want:    "unknown storage class",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			store := beads.NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
				called = true
				return nil, errors.New("runner should not be called")
			})
			_, err := store.CreateWithStorage(tc.input, tc.storage)
			if err == nil {
				t.Fatal("CreateWithStorage error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateWithStorage error = %q, want substring %q", err, tc.want)
			}
			if called {
				t.Fatal("runner was called after invalid storage validation")
			}
		})
	}
}

func TestBdStoreApplyGraphPlanStorageFlagConformance(t *testing.T) {
	cases := []struct {
		name          string
		storage       beads.StorageClass
		wantEphemeral bool
		wantNoHistory bool
	}{
		{name: "default history"},
		{name: "explicit history", storage: beads.StorageHistory},
		{name: "no-history", storage: beads.StorageNoHistory, wantNoHistory: true},
		{name: "ephemeral", storage: beads.StorageEphemeral, wantEphemeral: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var gotArgs []string
			var capturedPlan beads.GraphApplyPlan
			runner := func(cmdDir, name string, args ...string) ([]byte, error) {
				if cmdDir != dir {
					t.Fatalf("runner dir = %q, want %q", cmdDir, dir)
				}
				if name != "bd" {
					t.Fatalf("runner name = %q, want bd", name)
				}
				gotArgs = append([]string(nil), args...)
				data, err := os.ReadFile(args[2])
				if err != nil {
					t.Fatalf("reading graph plan: %v", err)
				}
				if err := json.Unmarshal(data, &capturedPlan); err != nil {
					t.Fatalf("unmarshal graph plan: %v", err)
				}
				return []byte(`{"ids":{"root":"bd-root","child":"bd-child"}}`), nil
			}
			store := beads.NewBdStore(dir, runner)

			result, err := store.ApplyGraphPlanWithStorage(t.Context(), &beads.GraphApplyPlan{
				Nodes: []beads.GraphApplyNode{
					{Key: "root", Title: "Root", Metadata: map[string]string{"gc.kind": "wisp"}},
					{Key: "child", Title: "Child", ParentKey: "root"},
				},
			}, tc.storage)
			if err != nil {
				t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
			}

			if result.IDs["root"] != "bd-root" || result.IDs["child"] != "bd-child" {
				t.Fatalf("result IDs = %#v, want root and child IDs", result.IDs)
			}
			if len(gotArgs) < 4 || gotArgs[0] != "create" || gotArgs[1] != "--graph" || gotArgs[3] != "--json" {
				t.Fatalf("graph args = %#v, want bd create --graph <file> --json", gotArgs)
			}
			assertArgPresence(t, gotArgs, "--ephemeral", tc.wantEphemeral)
			assertArgPresence(t, gotArgs, "--no-history", tc.wantNoHistory)
			graphJSON := mustJSON(t, capturedPlan)
			if strings.Contains(graphJSON, "ephemeral") || strings.Contains(graphJSON, "no_history") {
				t.Fatalf("graph plan JSON = %s, storage must be transported as bd flags only", graphJSON)
			}
		})
	}
}

func TestBdStoreGetUsesDirectShowForEphemeralRows(t *testing.T) {
	var calls []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		calls = append(calls, cmd)
		switch cmd {
		case "bd show --json bd-wisp":
			return []byte(`[{"id":"bd-wisp","title":"wisp","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z","ephemeral":true}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", cmd)
		}
	}
	store := beads.NewBdStore("/city", runner)

	got, err := store.Get("bd-wisp")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "bd-wisp" || !got.Ephemeral {
		t.Fatalf("Get = %+v, want ephemeral row bd-wisp", got)
	}
	wantCalls := []string{
		"bd show --json bd-wisp",
	}
	if fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestBdStoreListStorageTierConformance(t *testing.T) {
	listRows := `[
		{"id":"bd-history","title":"history","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z","labels":["scope"]},
		{"id":"bd-no-history","title":"no-history","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:01Z","labels":["scope"],"no_history":true}
	]`
	ephemeralRows := `[
		{"id":"bd-ephemeral","title":"ephemeral","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:02Z","labels":["scope"],"ephemeral":true}
	]`
	cases := []struct {
		name                  string
		query                 beads.ListQuery
		wantIDs               []string
		wantIncludeTemplates  bool
		wantUnlimitedPrelimit bool
		wantEphemeralQuery    bool
	}{
		{
			name:    "issues tier keeps history and no-history rows",
			query:   beads.ListQuery{Label: "scope"},
			wantIDs: []string{"bd-history", "bd-no-history"},
		},
		{
			name:                  "issues tier applies limit after client storage filtering",
			query:                 beads.ListQuery{Label: "scope", Limit: 1},
			wantIDs:               []string{"bd-history"},
			wantUnlimitedPrelimit: true,
		},
		{
			name:                  "wisps tier keeps no-history and ephemeral rows",
			query:                 beads.ListQuery{Label: "scope", TierMode: beads.TierWisps},
			wantIDs:               []string{"bd-no-history", "bd-ephemeral"},
			wantIncludeTemplates:  true,
			wantUnlimitedPrelimit: true,
			wantEphemeralQuery:    true,
		},
		{
			name:                 "both tiers keeps all storage rows",
			query:                beads.ListQuery{Label: "scope", TierMode: beads.TierBoth},
			wantIDs:              []string{"bd-history", "bd-no-history", "bd-ephemeral"},
			wantIncludeTemplates: true,
			wantEphemeralQuery:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			runner := func(_, name string, args ...string) ([]byte, error) {
				cmd := name + " " + strings.Join(args, " ")
				calls = append(calls, cmd)
				switch {
				case strings.HasPrefix(cmd, "bd list "):
					return []byte(listRows), nil
				case strings.HasPrefix(cmd, "bd query "):
					return []byte(ephemeralRows), nil
				default:
					return nil, fmt.Errorf("unexpected command: %s", cmd)
				}
			}
			store := beads.NewBdStore("/city", runner)
			got, err := store.List(tc.query)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			listCmd := firstCallWithPrefix(calls, "bd list ")
			if listCmd == "" {
				t.Fatalf("calls = %#v, want bd list call", calls)
			}
			assertCommandContains(t, listCmd, "--include-ephemeral", false)
			assertCommandContains(t, listCmd, "--include-templates", tc.wantIncludeTemplates)
			if tc.query.Limit > 0 {
				assertCommandContains(t, listCmd, "--limit 0", tc.wantUnlimitedPrelimit)
			}
			if gotQuery := firstCallWithPrefix(calls, "bd query "); (gotQuery != "") != tc.wantEphemeralQuery {
				t.Fatalf("bd query presence = %v, want %v; calls = %#v", gotQuery != "", tc.wantEphemeralQuery, calls)
			}
			if gotIDs := beadIDs(got); fmt.Sprint(gotIDs) != fmt.Sprint(tc.wantIDs) {
				t.Fatalf("List IDs = %v, want %v", gotIDs, tc.wantIDs)
			}
		})
	}
}

func TestBdStoreListBothTiersUnionsEphemeralQueryConformance(t *testing.T) {
	var calls []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		calls = append(calls, cmd)
		if strings.Contains(cmd, "--include-ephemeral") {
			t.Fatalf("bd list command = %q, --include-ephemeral is only valid for bd ready", cmd)
		}
		switch {
		case strings.HasPrefix(cmd, "bd list "):
			return []byte(`[
				{"id":"bd-history","title":"history","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z","labels":["scope"]},
				{"id":"bd-no-history","title":"no-history","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:01Z","labels":["scope"],"no_history":true}
			]`), nil
		case strings.HasPrefix(cmd, "bd query "):
			return []byte(`[
				{"id":"bd-ephemeral","title":"ephemeral","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:02Z","labels":["scope"],"ephemeral":true}
			]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", cmd)
		}
	}
	store := beads.NewBdStore("/city", runner)

	got, err := store.List(beads.ListQuery{Label: "scope", TierMode: beads.TierBoth, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %#v, want bd list plus bd query reads", calls)
	}
	if firstCallWithPrefix(calls, "bd query ") == "" {
		t.Fatalf("calls = %#v, want bd query ephemeral read", calls)
	}
	wantIDs := []string{"bd-history", "bd-no-history", "bd-ephemeral"}
	if gotIDs := beadIDs(got); fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Fatalf("List IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func TestBdStoreReadyStorageTierConformance(t *testing.T) {
	// Verified against the official bd 1.0.4 release binary: plain `bd ready`
	// returns history rows only. `--include-ephemeral` adds ephemeral rows, but
	// not no-history rows, which is why bd-1.0.4-compatible claimable work must
	// remain history-backed.
	defaultRows := `[
		{"id":"bd-history","title":"history","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z"}
	]`
	includeEphemeralRows := `[
		{"id":"bd-history","title":"history","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z"},
		{"id":"bd-ephemeral","title":"ephemeral","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:02Z","ephemeral":true}
	]`
	cases := []struct {
		name                 string
		query                beads.ReadyQuery
		wantIDs              []string
		wantIncludeEphemeral bool
	}{
		{
			name:    "issues tier matches bd ready default history-only release behavior",
			wantIDs: []string{"bd-history"},
		},
		{
			name:                 "wisps tier keeps ephemeral ready work returned by bd 1.0.4",
			query:                beads.ReadyQuery{TierMode: beads.TierWisps},
			wantIDs:              []string{"bd-ephemeral"},
			wantIncludeEphemeral: true,
		},
		{
			name:                 "both tiers keeps history and ephemeral ready work returned by bd 1.0.4",
			query:                beads.ReadyQuery{TierMode: beads.TierBoth},
			wantIDs:              []string{"bd-history", "bd-ephemeral"},
			wantIncludeEphemeral: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCmd string
			runner := func(_, name string, args ...string) ([]byte, error) {
				gotCmd = name + " " + strings.Join(args, " ")
				if strings.Contains(gotCmd, "--include-ephemeral") {
					return []byte(includeEphemeralRows), nil
				}
				return []byte(defaultRows), nil
			}
			store := beads.NewBdStore("/city", runner)
			got, err := store.Ready(tc.query)
			if err != nil {
				t.Fatalf("Ready: %v", err)
			}
			assertCommandContains(t, gotCmd, "--include-ephemeral", tc.wantIncludeEphemeral)
			if gotIDs := beadIDs(got); fmt.Sprint(gotIDs) != fmt.Sprint(tc.wantIDs) {
				t.Fatalf("Ready IDs = %v, want %v", gotIDs, tc.wantIDs)
			}
		})
	}
}

func assertArgPresence(t *testing.T, args []string, flag string, want bool) {
	t.Helper()
	got := slices.Contains(args, flag)
	if got != want {
		t.Fatalf("args = %#v, presence of %s = %v, want %v", args, flag, got, want)
	}
}

func assertArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Fatalf("args = %#v, want %s %s", args, flag, value)
}

func metadataArg(t *testing.T, args []string) map[string]string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--metadata" {
			var metadata map[string]string
			if err := json.Unmarshal([]byte(args[i+1]), &metadata); err != nil {
				t.Fatalf("unmarshal metadata arg %q: %v", args[i+1], err)
			}
			return metadata
		}
	}
	t.Fatalf("args = %#v, want --metadata", args)
	return nil
}

func assertCommandContains(t *testing.T, command, fragment string, want bool) {
	t.Helper()
	got := strings.Contains(command, fragment)
	if got != want {
		t.Fatalf("command = %q, contains %q = %v, want %v", command, fragment, got, want)
	}
}

func firstCallWithPrefix(calls []string, prefix string) string {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return call
		}
	}
	return ""
}

func beadIDs(items []beads.Bead) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}
