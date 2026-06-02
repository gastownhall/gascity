//go:build cgo && gascity_native_beads

package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDoltliteReadStoreListsSessionBeads(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	rows, err := store.List(ListQuery{
		Label: "gc:session",
		Sort:  SortCreatedDesc,
	})
	if err != nil {
		t.Fatalf("List session beads: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("session rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.ID != "gc-session" || got.Type != "session" || got.Metadata["session_name"] != "session-1" {
		t.Fatalf("session bead = %#v", got)
	}
	if !slices.Contains(got.Labels, "gc:session") {
		t.Fatalf("labels = %v, missing gc:session", got.Labels)
	}
}

func TestDoltliteReadStoreSkipLabels(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	rows, err := store.List(ListQuery{
		Label:      "gc:session",
		SkipLabels: true,
	})
	if err != nil {
		t.Fatalf("List session beads: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("session rows = %d, want 1", len(rows))
	}
	if len(rows[0].Labels) != 0 {
		t.Fatalf("labels hydrated with SkipLabels=true: %v", rows[0].Labels)
	}
}

func TestDoltliteReadStoreHydratesParent(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	withParent, err := store.List(ListQuery{Type: "task", Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List tasks with parent: %v", err)
	}
	child := findTestBead(t, withParent, "gc-child")
	if child.ParentID != "gc-parent" {
		t.Fatalf("child parent = %q, want gc-parent", child.ParentID)
	}
}

func TestDoltliteReadStoreTypeFallbackCanSkipLabels(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	rows, err := store.List(ListQuery{
		Type:       "session",
		SkipLabels: true,
	})
	if err != nil {
		t.Fatalf("List type=session: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("type=session rows = %d, want 1", len(rows))
	}
	if rows[0].ID != "gc-session" {
		t.Fatalf("type=session row = %s, want gc-session", rows[0].ID)
	}
	if len(rows[0].Labels) != 0 {
		t.Fatalf("unexpected hydrated labels: %v", rows[0].Labels)
	}
}

func TestDoltliteReadStoreReadyUsesDoltlite(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	rows, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !hasTestBead(rows, "gc-ready") {
		t.Fatalf("Ready missing gc-ready: %#v", rows)
	}
	if hasTestBead(rows, "gc-session") {
		t.Fatalf("Ready included session bead: %#v", rows)
	}
	if hasTestBead(rows, "gc-blocked") {
		t.Fatalf("Ready included blocked bead: %#v", rows)
	}
}

func TestDoltliteReadStoreReadyBlocksWorkflowDependencyTypes(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	for _, depType := range []string{"waits-for", "conditional-blocks"} {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
			ID:        "gc-blocked-" + depType,
			Title:     "workflow blocked",
			Status:    "open",
			IssueType: "task",
			CreatedAt: time.Now().UTC().Add(time.Minute),
			Dependencies: []testDoltliteDependency{{
				DependsOnID: "gc-blocker",
				Type:        depType,
			}},
		})
	}

	rows, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, depType := range []string{"waits-for", "conditional-blocks"} {
		id := "gc-blocked-" + depType
		if hasTestBead(rows, id) {
			t.Fatalf("Ready included %s blocked by %s: %#v", id, depType, rows)
		}
	}
}

func TestDoltliteReadStoreReadyDefaultsMissingDependencyTypeToBlocks(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
		ID:        "gc-empty-dep-type",
		Title:     "blocked by empty dependency type",
		Status:    "open",
		IssueType: "task",
		CreatedAt: time.Now().UTC().Add(time.Minute),
		Assignee:  "rig/missing-dep-type",
		Dependencies: []testDoltliteDependency{{
			DependsOnID: "gc-blocker",
			Type:        "",
		}},
	})
	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
		ID:        "gc-null-dep-type",
		Title:     "blocked by null dependency type",
		Status:    "open",
		IssueType: "task",
		CreatedAt: time.Now().UTC().Add(2 * time.Minute),
		Assignee:  "rig/missing-dep-type",
	})
	if _, err := writer.Exec(`INSERT INTO dependencies (
		issue_id, depends_on_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type
	) VALUES (?, ?, ?, '', '', NULL)`, "gc-null-dep-type", "gc-blocker", "gc-blocker"); err != nil {
		t.Fatalf("insert null dependency type: %v", err)
	}

	rows, err := store.Ready(ReadyQuery{Assignee: "rig/missing-dep-type"})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("Ready included rows blocked by missing dependency types: %#v", rows)
	}
}

func TestDoltliteReadStoreReadyBlocksMissingTargets(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	for _, depType := range []string{"blocks", "waits-for", "conditional-blocks"} {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
			ID:        "gc-missing-target-" + depType,
			Title:     "missing target blocked",
			Status:    "open",
			IssueType: "task",
			CreatedAt: time.Now().UTC().Add(time.Minute),
			Assignee:  "rig/missing-targets",
			Dependencies: []testDoltliteDependency{{
				DependsOnID: "gc-missing-" + depType,
				Type:        depType,
			}},
		})
	}

	rows, err := store.Ready(ReadyQuery{Assignee: "rig/missing-targets"})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("Ready included beads with missing blockers: %#v", rows)
	}
}

func TestDoltliteReadStoreReadyBlocksOpenWispTargets(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	for _, depType := range []string{"blocks", "waits-for", "conditional-blocks"} {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
			ID:        "gc-wisp-blocked-" + depType,
			Title:     "wisp target blocked",
			Status:    "open",
			IssueType: "task",
			CreatedAt: time.Now().UTC().Add(time.Minute),
			Assignee:  "rig/wisp-blockers",
			Dependencies: []testDoltliteDependency{{
				DependsOnWispID: "gc-tier-wisp",
				Type:            depType,
			}},
		})
	}

	rows, err := store.Ready(ReadyQuery{Assignee: "rig/wisp-blockers"})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("Ready included beads blocked by open wisps: %#v", rows)
	}
}

func TestDoltliteReadStoreReadyUsesTypedWispTargetWhenIDsCollide(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
		ID:        "gc-collision-target",
		Title:     "closed issue sharing wisp id",
		Status:    "closed",
		IssueType: "task",
		CreatedAt: time.Now().UTC(),
	})
	insertTestDoltliteIssue(t, writer, "wisps", "wisp_labels", "wisp_dependencies", testDoltliteIssue{
		ID:        "gc-collision-target",
		Title:     "open wisp sharing issue id",
		Status:    "open",
		IssueType: "molecule",
		CreatedAt: time.Now().UTC(),
	})
	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
		ID:        "gc-wisp-collision-blocked",
		Title:     "blocked by typed wisp target",
		Status:    "open",
		IssueType: "task",
		CreatedAt: time.Now().UTC().Add(time.Minute),
		Assignee:  "rig/wisp-collision",
		Dependencies: []testDoltliteDependency{{
			DependsOnWispID: "gc-collision-target",
			Type:            "blocks",
		}},
	})

	rows, err := store.Ready(ReadyQuery{Assignee: "rig/wisp-collision"})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("Ready used closed issue status instead of open typed wisp target: %#v", rows)
	}
}

func TestDoltliteReadStoreReadyHonorsLimit(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	rows, err := store.Ready(ReadyQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Ready(limit=1): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Ready(limit=1) returned %d rows, want 1: %#v", len(rows), rows)
	}
}

func TestDoltliteReadStoreReadyLimitFindsReadyBehindBlockedWindow(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	depTypes := []string{"blocks", "waits-for", "conditional-blocks"}
	now := time.Now().UTC().Add(time.Minute)
	for i := 0; i < 100; i++ {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
			ID:        fmt.Sprintf("gc-newer-blocked-%03d", i),
			Title:     "newer blocked",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			Dependencies: []testDoltliteDependency{{
				DependsOnID: "gc-blocker",
				Type:        depTypes[i%len(depTypes)],
			}},
		})
	}

	rows, err := store.Ready(ReadyQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Ready(limit=1): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Ready(limit=1) returned %d rows, want 1; rows=%#v", len(rows), rows)
	}
	if strings.HasPrefix(rows[0].ID, "gc-newer-blocked-") {
		t.Fatalf("Ready(limit=1) returned blocked row %#v", rows[0])
	}
}

func TestDoltliteReadStoreReadyOrdersPriorityBeforeCreated(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	now := time.Now().UTC()
	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
		ID:        "gc-priority-low-newer",
		Title:     "low priority newer",
		Status:    "open",
		IssueType: "task",
		Priority:  2,
		CreatedAt: now.Add(time.Minute),
		Assignee:  "rig/priority",
	})
	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
		ID:        "gc-priority-high-older",
		Title:     "high priority older",
		Status:    "open",
		IssueType: "task",
		Priority:  0,
		CreatedAt: now,
		Assignee:  "rig/priority",
	})

	rows, err := store.Ready(ReadyQuery{Assignee: "rig/priority", Limit: 1})
	if err != nil {
		t.Fatalf("Ready priority limit: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "gc-priority-high-older" {
		t.Fatalf("Ready priority order = %#v, want gc-priority-high-older first", rows)
	}
}

func TestDoltliteReadStoreHandlesNullDescription(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := writer.Exec(`
		INSERT INTO issues (
			id, title, status, issue_type, priority, created_at, updated_at,
			assignee, description, design, acceptance_criteria, notes, metadata
		)
		VALUES (?, ?, 'open', 'task', 2, ?, ?, ?, NULL, '', '', '', '{}')
	`, "gc-null-description", "null description", now, now, "rig/null-description"); err != nil {
		t.Fatalf("insert null description issue: %v", err)
	}

	got, err := store.Get("gc-null-description")
	if err != nil {
		t.Fatalf("Get null description: %v", err)
	}
	if got.Description != "" {
		t.Fatalf("Get description = %q, want empty string", got.Description)
	}

	listed, err := store.List(ListQuery{Assignee: "rig/null-description"})
	if err != nil {
		t.Fatalf("List null description: %v", err)
	}
	if len(listed) != 1 || listed[0].Description != "" {
		t.Fatalf("List null description rows = %#v, want one row with empty description", listed)
	}

	ready, err := store.Ready(ReadyQuery{Assignee: "rig/null-description"})
	if err != nil {
		t.Fatalf("Ready null description: %v", err)
	}
	if len(ready) != 1 || ready[0].Description != "" {
		t.Fatalf("Ready null description rows = %#v, want one row with empty description", ready)
	}
}

func TestDoltliteReadStoreBeforeFiltersUseDriverNativeTimestamps(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	cutoff := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	for _, issue := range []struct {
		id        string
		createdAt time.Time
		updatedAt time.Time
	}{
		{id: "gc-native-time-before", createdAt: cutoff.Add(-time.Hour), updatedAt: cutoff.Add(-30 * time.Minute)},
		{id: "gc-native-time-after", createdAt: cutoff.Add(time.Hour), updatedAt: cutoff.Add(30 * time.Minute)},
	} {
		if _, err := writer.Exec(`INSERT INTO issues (
			id, title, status, issue_type, priority, created_at, updated_at,
			assignee, description, design, acceptance_criteria, notes, metadata
		) VALUES (?, ?, 'open', 'task', 2, ?, ?, 'rig/native-time', '', '', '', '', '{}')`,
			issue.id, issue.id, issue.createdAt, issue.updatedAt); err != nil {
			t.Fatalf("insert native timestamp issue %s: %v", issue.id, err)
		}
	}

	createdRows, err := store.List(ListQuery{
		Assignee:      "rig/native-time",
		CreatedBefore: cutoff,
		Sort:          SortCreatedAsc,
		SkipLabels:    true,
	})
	if err != nil {
		t.Fatalf("List CreatedBefore: %v", err)
	}
	if got := testBeadIDs(createdRows); !slices.Equal(got, []string{"gc-native-time-before"}) {
		t.Fatalf("CreatedBefore ids = %v, want [gc-native-time-before]; rows=%#v", got, createdRows)
	}

	updatedRows, err := store.List(ListQuery{
		Assignee:      "rig/native-time",
		UpdatedBefore: cutoff,
		Sort:          SortCreatedAsc,
		SkipLabels:    true,
	})
	if err != nil {
		t.Fatalf("List UpdatedBefore: %v", err)
	}
	if got := testBeadIDs(updatedRows); !slices.Equal(got, []string{"gc-native-time-before"}) {
		t.Fatalf("UpdatedBefore ids = %v, want [gc-native-time-before]; rows=%#v", got, updatedRows)
	}
}

func TestDoltliteReadStoreCachesInvalidateOnWorkingSetWrites(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	sessions, err := store.ListSessionBeads()
	if err != nil {
		t.Fatalf("ListSessionBeads before write: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("session count before write = %d, want 1", len(sessions))
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := writer.Exec(`
		INSERT INTO issues (
			id, title, status, issue_type, priority, created_at, updated_at,
			description, design, acceptance_criteria, notes, metadata
		)
		VALUES (?, ?, 'open', 'session', 2, ?, ?, '', '', '', '', ?)
	`, "gc-session-2", "session 2", now, now, `{"session_name":"session-2"}`); err != nil {
		t.Fatalf("insert session through writer: %v", err)
	}

	sessions, err = store.ListSessionBeads()
	if err != nil {
		t.Fatalf("ListSessionBeads after write: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("session count after uncommitted write = %d, want 2", len(sessions))
	}

	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready before task write: %v", err)
	}
	if hasTestBead(ready, "gc-ready-2") {
		t.Fatalf("Ready unexpectedly found gc-ready-2 before insert: %#v", ready)
	}

	later := time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	if _, err := writer.Exec(`
		INSERT INTO issues (
			id, title, status, issue_type, priority, created_at, updated_at,
			description, design, acceptance_criteria, notes, metadata
		)
		VALUES (?, ?, 'open', 'task', 2, ?, ?, '', '', '', '', ?)
	`, "gc-ready-2", "ready 2", later, later, `{}`); err != nil {
		t.Fatalf("insert ready work through writer: %v", err)
	}

	ready, err = store.Ready()
	if err != nil {
		t.Fatalf("Ready after task write: %v", err)
	}
	if !hasTestBead(ready, "gc-ready-2") {
		t.Fatalf("Ready after task write missing gc-ready-2: %#v", ready)
	}
}

func TestDoltliteReadStoreReadsOrderRunHotPaths(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	last, err := store.LastOrderRun("rig/sweep")
	if err != nil {
		t.Fatalf("LastOrderRun: %v", err)
	}
	if last.IsZero() {
		t.Fatal("LastOrderRun returned zero time")
	}

	open, err := store.HasOpenOrderRun("rig/sweep")
	if err != nil {
		t.Fatalf("HasOpenOrderRun(open): %v", err)
	}
	if open {
		t.Fatal("HasOpenOrderRun reported open for closed run")
	}

	open, err = store.HasOpenOrderRun("rig/active")
	if err != nil {
		t.Fatalf("HasOpenOrderRun(active): %v", err)
	}
	if !open {
		t.Fatal("HasOpenOrderRun did not find active run")
	}
}

func TestDoltliteReadStoreListsQueuedNudgeBeads(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	rows, err := store.List(ListQuery{
		Label: "gc:nudge",
	})
	if err != nil {
		t.Fatalf("List queued nudge beads: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("nudge rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.ID != "gc-nudge" || got.Type != "chore" {
		t.Fatalf("nudge bead = %#v", got)
	}
	if got.Metadata["state"] != "queued" || got.Metadata["nudge_id"] != "nudge-1" {
		t.Fatalf("nudge metadata = %#v", got.Metadata)
	}
	if !slices.Contains(got.Labels, "agent:gastown/polecat") || !slices.Contains(got.Labels, "nudge:nudge-1") {
		t.Fatalf("nudge labels = %v", got.Labels)
	}
}

func TestDoltliteReadStoreFiltersNudgesByMetadata(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	rows, err := store.List(ListQuery{
		Type: "chore",
		Metadata: map[string]string{
			"target_session": "gastown__polecat-abc123",
			"state":          "queued",
		},
		SkipLabels: true,
	})
	if err != nil {
		t.Fatalf("List nudge by metadata: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "gc-nudge" {
		t.Fatalf("metadata rows = %#v, want gc-nudge", rows)
	}
	if len(rows[0].Labels) != 0 {
		t.Fatalf("labels hydrated with SkipLabels=true: %v", rows[0].Labels)
	}
}

func TestDoltliteReadStoreMetadataFilterFindsMatchBehindLimit(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	base := time.Now().UTC().Add(10 * time.Minute)
	for i := 0; i < 75; i++ {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
			ID:        fmt.Sprintf("gc-metadata-skip-%02d", i),
			Title:     "newer non-match",
			Status:    "open",
			IssueType: "chore",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			Metadata: map[string]string{
				"state":          "queued",
				"target_session": "other-session",
			},
		})
	}
	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{
		ID:        "gc-metadata-match",
		Title:     "older match",
		Status:    "open",
		IssueType: "chore",
		CreatedAt: base.Add(-time.Hour),
		Metadata: map[string]string{
			"state":          "queued",
			"target_session": "metadata-sql-target",
		},
	})

	rows, err := store.List(ListQuery{
		Type: "chore",
		Metadata: map[string]string{
			"state":          "queued",
			"target_session": "metadata-sql-target",
		},
		Limit:      1,
		Sort:       SortCreatedDesc,
		SkipLabels: true,
	})
	if err != nil {
		t.Fatalf("List metadata with limit: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "gc-metadata-match" {
		t.Fatalf("metadata limited rows = %#v, want gc-metadata-match", rows)
	}
}

func TestDoltliteMetadataFilterPredicatesMatchStringValues(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	if _, err := db.Exec(`CREATE TABLE rows (id TEXT, metadata TEXT)`); err != nil {
		t.Fatalf("create rows: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO rows (id, metadata) VALUES ('match', '{"state":"queued","target_session":"worker-1"}')`,
		`INSERT INTO rows (id, metadata) VALUES ('spaced', '{"state": "queued", "target_session": "worker-1"}')`,
		`INSERT INTO rows (id, metadata) VALUES ('wrong', '{"state":"queued","target_session":"worker-2"}')`,
		`INSERT INTO rows (id, metadata) VALUES ('malformed', '{')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("insert fixture: %v", err)
		}
	}

	where, args := doltliteMetadataFilterPredicates(map[string]string{
		"state":          "queued",
		"target_session": "worker-1",
	})
	rows, err := db.Query(`SELECT id FROM rows i WHERE `+strings.Join(where, " AND ")+` ORDER BY id`, args...)
	if err != nil {
		t.Fatalf("query metadata predicates: %v", err)
	}
	defer rows.Close() //nolint:errcheck // test cleanup

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if !slices.Equal(ids, []string{"match", "spaced"}) {
		t.Fatalf("predicate ids = %v, want [match spaced]", ids)
	}
}

func TestDoltliteReadStoreTierModesIncludeWisps(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	issues, err := store.List(ListQuery{Label: "tier-test", Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List issues tier: %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "gc-tier-issue" || issues[0].Ephemeral {
		t.Fatalf("issues tier rows = %#v, want only non-ephemeral gc-tier-issue", issues)
	}

	wisps, err := store.List(ListQuery{Label: "tier-test", TierMode: TierWisps, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List wisps tier: %v", err)
	}
	if len(wisps) != 1 || wisps[0].ID != "gc-tier-wisp" || !wisps[0].Ephemeral {
		t.Fatalf("wisps tier rows = %#v, want only ephemeral gc-tier-wisp", wisps)
	}

	both, err := store.List(ListQuery{Label: "tier-test", TierMode: TierBoth, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List both tiers: %v", err)
	}
	if got := testBeadIDs(both); !slices.Equal(got, []string{"gc-tier-issue", "gc-tier-wisp"}) {
		t.Fatalf("both tier ids = %v, want [gc-tier-issue gc-tier-wisp]; rows=%#v", got, both)
	}
}

func TestDoltliteReadStoreGetFindsWisps(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	got, err := store.Get("gc-tier-wisp")
	if err != nil {
		t.Fatalf("Get wisp: %v", err)
	}
	if got.ID != "gc-tier-wisp" || !got.Ephemeral {
		t.Fatalf("Get wisp = %#v, want ephemeral gc-tier-wisp", got)
	}
}

func TestDoltliteReadStoreFiltersPluralAssigneesAcrossTiers(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	rows, err := store.List(ListQuery{
		Assignees: []string{"rig/ready-worker", "rig/wisp-worker"},
		TierMode:  TierBoth,
		Sort:      SortCreatedAsc,
	})
	if err != nil {
		t.Fatalf("List plural assignees: %v", err)
	}
	if got := testBeadIDs(rows); !slices.Equal(got, []string{"gc-assigned-ready", "gc-tier-wisp"}) {
		t.Fatalf("plural assignee ids = %v, want [gc-assigned-ready gc-tier-wisp]; rows=%#v", got, rows)
	}
	if !rows[1].Ephemeral {
		t.Fatalf("wisp row Ephemeral = false: %#v", rows[1])
	}
}

func TestDoltliteCachingStoreLiveFastReadDoesNotEraseDependencyCache(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	before, err := cache.DepList("gc-child", "down")
	if err != nil {
		t.Fatalf("DepList before fast read: %v", err)
	}
	if len(before) != 1 || before[0].DependsOnID != "gc-parent" {
		t.Fatalf("deps before fast read = %#v, want parent gc-parent", before)
	}

	if _, err := cache.List(ListQuery{
		Type:       "task",
		Live:       true,
		SkipLabels: true,
	}); err != nil {
		t.Fatalf("fast live List: %v", err)
	}

	after, err := cache.DepList("gc-child", "down")
	if err != nil {
		t.Fatalf("DepList after fast read: %v", err)
	}
	if len(after) != 1 || after[0].DependsOnID != "gc-parent" {
		t.Fatalf("deps after fast read = %#v, want parent gc-parent", after)
	}
}

func newTestDoltliteReadStore(t *testing.T) (*DoltliteReadStore, func()) {
	t.Helper()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	meta := []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	dbDir := filepath.Join(beadsDir, "doltlite")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir doltlite dir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "hq.db")
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open doltlite fixture db: %v", err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	createTestDoltliteSchema(t, db)

	now := time.Now().UTC()
	created := []testDoltliteIssue{
		{
			ID:          "gc-session",
			Title:       "session",
			Status:      "open",
			IssueType:   "session",
			CreatedAt:   now,
			Labels:      []string{"gc:session", "agent:test"},
			Metadata:    map[string]string{"session_name": "session-1"},
			Description: "session bead",
		},
		{
			ID:        "gc-parent",
			Title:     "parent",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now,
		},
		{
			ID:        "gc-child",
			Title:     "child",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now,
			Dependencies: []testDoltliteDependency{{
				DependsOnID: "gc-parent",
				Type:        "parent-child",
			}},
		},
		{
			ID:        "gc-ready",
			Title:     "ready",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now,
		},
		{
			ID:        "gc-assigned-progress",
			Title:     "assigned progress",
			Status:    "in_progress",
			IssueType: "task",
			CreatedAt: now,
			Assignee:  "rig/worker",
		},
		{
			ID:        "gc-assigned-ready",
			Title:     "assigned ready",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now,
			Assignee:  "rig/ready-worker",
		},
		{
			ID:        "gc-routed",
			Title:     "routed",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now,
			Metadata:  map[string]string{"gc.routed_to": "rig/polecat"},
		},
		{
			ID:        "gc-blocker",
			Title:     "blocker",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now,
		},
		{
			ID:        "gc-blocked",
			Title:     "blocked",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now,
			Dependencies: []testDoltliteDependency{{
				DependsOnID: "gc-blocker",
				Type:        "blocks",
			}},
		},
		{
			ID:        "gc-nudge",
			Title:     "Queued nudge for gastown/polecat",
			Status:    "open",
			IssueType: "chore",
			CreatedAt: now,
			Labels:    []string{"gc:nudge", "agent:gastown/polecat", "nudge:nudge-1", "source:wait"},
			Metadata: map[string]string{
				"agent":          "gastown/polecat",
				"message":        "wait satisfied; continue",
				"nudge_id":       "nudge-1",
				"source":         "wait",
				"state":          "queued",
				"target_session": "gastown__polecat-abc123",
				"wait_bead_id":   "gc-wait",
			},
		},
		{
			ID:        "gc-wait",
			Title:     "Wait for dependency",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now,
			Labels:    []string{"gc:wait"},
			Metadata: map[string]string{
				"nudge_id": "nudge-1",
				"state":    "ready",
			},
		},
		{
			ID:        "gc-order-closed",
			Title:     "order:rig/sweep",
			Status:    "closed",
			IssueType: "task",
			CreatedAt: now.Add(time.Second),
			Labels:    []string{"order-run:rig/sweep", "gc:order-tracking"},
		},
		{
			ID:        "gc-order-open",
			Title:     "order:rig/active",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now.Add(2 * time.Second),
			Labels:    []string{"order-run:rig/active", "gc:order-tracking"},
		},
		{
			ID:        "gc-tier-issue",
			Title:     "tier issue",
			Status:    "open",
			IssueType: "task",
			CreatedAt: now.Add(3 * time.Second),
			Labels:    []string{"tier-test"},
		},
	}
	for _, issue := range created {
		insertTestDoltliteIssue(t, db, "issues", "labels", "dependencies", issue)
	}
	insertTestDoltliteIssue(t, db, "wisps", "wisp_labels", "wisp_dependencies", testDoltliteIssue{
		ID:        "gc-tier-wisp",
		Title:     "tier wisp",
		Status:    "open",
		IssueType: "task",
		CreatedAt: now.Add(4 * time.Second),
		Assignee:  "rig/wisp-worker",
		Labels:    []string{"tier-test"},
		Metadata:  map[string]string{"kind": "wisp"},
	})

	backing := NewBdStore(dir, func(string, string, ...string) ([]byte, error) {
		t.Fatal("backing bd runner should not be called by doltlite read tests")
		return nil, nil
	})
	store, err := NewDoltliteReadStore(dir, backing)
	if err != nil {
		t.Fatalf("NewDoltliteReadStore: %v", err)
	}
	return store, func() { _ = store.CloseStore() }
}

type testDoltliteDependency struct {
	DependsOnID       string
	DependsOnIssueID  string
	DependsOnWispID   string
	DependsOnExternal string
	Type              string
}

type testDoltliteIssue struct {
	ID           string
	Title        string
	Status       string
	IssueType    string
	Priority     int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Assignee     string
	Description  string
	Labels       []string
	Metadata     map[string]string
	Dependencies []testDoltliteDependency
}

func createTestDoltliteSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE issues (
			id TEXT PRIMARY KEY,
			title TEXT,
			status TEXT,
			issue_type TEXT,
			priority INTEGER,
			created_at TEXT,
			updated_at TEXT,
			assignee TEXT,
			description TEXT,
			design TEXT,
			acceptance_criteria TEXT,
			notes TEXT,
			metadata TEXT
		)`,
		`CREATE TABLE wisps (
			id TEXT PRIMARY KEY,
			title TEXT,
			status TEXT,
			issue_type TEXT,
			priority INTEGER,
			created_at TEXT,
			updated_at TEXT,
			assignee TEXT,
			description TEXT,
			design TEXT,
			acceptance_criteria TEXT,
			notes TEXT,
			metadata TEXT
		)`,
		`CREATE TABLE labels (issue_id TEXT, label TEXT)`,
		`CREATE TABLE wisp_labels (issue_id TEXT, label TEXT)`,
		`CREATE TABLE dependencies (
			issue_id TEXT,
			depends_on_id TEXT,
			depends_on_issue_id TEXT,
			depends_on_wisp_id TEXT,
			depends_on_external TEXT,
			type TEXT
		)`,
		`CREATE TABLE wisp_dependencies (
			issue_id TEXT,
			depends_on_id TEXT,
			depends_on_issue_id TEXT,
			depends_on_wisp_id TEXT,
			depends_on_external TEXT,
			type TEXT
		)`,
		`INSERT INTO config (key, value) VALUES ('issue_prefix', 'gc')`,
		`INSERT INTO config (key, value) VALUES ('types.custom', 'session,agent,role,rig,message,convoy,molecule,gate,merge-request')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create test doltlite schema: %v\nstmt: %s", err, stmt)
		}
	}
}

func insertTestDoltliteIssue(t *testing.T, db *sql.DB, issueTable, labelTable, depTable string, issue testDoltliteIssue) {
	t.Helper()
	if issue.Status == "" {
		issue.Status = "open"
	}
	if issue.IssueType == "" {
		issue.IssueType = "task"
	}
	if issue.CreatedAt.IsZero() {
		issue.CreatedAt = time.Now().UTC()
	}
	if issue.UpdatedAt.IsZero() {
		issue.UpdatedAt = issue.CreatedAt
	}
	metadata := "{}"
	if len(issue.Metadata) > 0 {
		raw, err := json.Marshal(issue.Metadata)
		if err != nil {
			t.Fatalf("marshal metadata for %s: %v", issue.ID, err)
		}
		metadata = string(raw)
	}
	_, err := db.Exec(`INSERT INTO `+issueTable+` (
		id, title, status, issue_type, priority, created_at, updated_at,
		assignee, description, design, acceptance_criteria, notes, metadata
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', ?)`,
		issue.ID,
		issue.Title,
		issue.Status,
		issue.IssueType,
		issue.Priority,
		issue.CreatedAt.Format(time.RFC3339Nano),
		issue.UpdatedAt.Format(time.RFC3339Nano),
		issue.Assignee,
		issue.Description,
		metadata,
	)
	if err != nil {
		t.Fatalf("insert %s into %s: %v", issue.ID, issueTable, err)
	}
	for _, label := range issue.Labels {
		if _, err := db.Exec(`INSERT INTO `+labelTable+` (issue_id, label) VALUES (?, ?)`, issue.ID, label); err != nil {
			t.Fatalf("insert label %s for %s: %v", label, issue.ID, err)
		}
	}
	for _, dep := range issue.Dependencies {
		dependsOnIssueID := dep.DependsOnIssueID
		if dependsOnIssueID == "" && dep.DependsOnWispID == "" && dep.DependsOnExternal == "" {
			dependsOnIssueID = dep.DependsOnID
		}
		if _, err := db.Exec(`INSERT INTO `+depTable+` (
			issue_id, depends_on_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type
		) VALUES (?, ?, ?, ?, ?, ?)`, issue.ID, dep.DependsOnID, dependsOnIssueID, dep.DependsOnWispID, dep.DependsOnExternal, dep.Type); err != nil {
			t.Fatalf("insert dep %s -> %s: %v", issue.ID, dep.DependsOnID, err)
		}
	}
}

func testBeadIDs(rows []Bead) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func findTestBead(t *testing.T, rows []Bead, id string) Bead {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("missing bead %s in %#v", id, rows)
	return Bead{}
}

func hasTestBead(rows []Bead, id string) bool {
	for _, row := range rows {
		if row.ID == id {
			return true
		}
	}
	return false
}

func openTestDoltliteWriter(t *testing.T, readDB *sql.DB) *sql.DB {
	t.Helper()
	rows, err := readDB.Query("PRAGMA database_list")
	if err != nil {
		t.Fatalf("query database list: %v", err)
	}
	defer rows.Close() //nolint:errcheck // test cleanup

	var dbPath string
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			t.Fatalf("scan database list: %v", err)
		}
		if name == "main" {
			dbPath = file
			break
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read database list: %v", err)
	}
	if dbPath == "" {
		t.Fatal("main database path not found")
	}

	writer, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rw&_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open writable doltlite db: %v", err)
	}
	return writer
}
