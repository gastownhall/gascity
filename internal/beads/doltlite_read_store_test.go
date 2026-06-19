//go:build gascity_native_beads

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

// TestDoltliteReadStoreBeforeFiltersRespectCutoff verifies that the CreatedBefore
// and UpdatedBefore list filters return only rows whose timestamps precede the
// cutoff. Timestamps are seeded in the store's canonical SQLite text format
// (doltliteSQLiteTime) because the before-filters compare with SQLite julianday()
// and parse with parseTimeString, both of which require ISO-8601 text. Binding a
// raw time.Time instead delegates formatting to the SQL driver:
// github.com/mattn/go-sqlite3 emitted ISO text, but modernc.org/sqlite emits
// time.Time.String() (e.g. "2026-06-01 07:00:00 +0000 UTC"), which julianday()
// cannot parse — the filter would then drop every row. See ga-p7ipsu.
func TestDoltliteReadStoreBeforeFiltersRespectCutoff(t *testing.T) {
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
			issue.id, issue.id, doltliteSQLiteTime(issue.createdAt), doltliteSQLiteTime(issue.updatedAt)); err != nil {
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
	db, err := sql.Open("sqlite", ":memory:")
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

// TestDoltliteReadStoreTierModesIncludeWisps pins the storage-tier contract
// from query.go (TierMode) to the same shape TestBdStoreListStorageTierConformance
// pins for BdStore (#3045, #3444): TierIssues keeps history and no-history
// rows and drops only ephemeral ones; TierWisps keeps no-history and
// ephemeral rows; TierBoth unions everything.
func TestDoltliteReadStoreTierModesIncludeWisps(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	issues, err := store.List(ListQuery{Label: "tier-test", Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List issues tier: %v", err)
	}
	if got := testBeadIDs(issues); !slices.Equal(got, []string{"gc-tier-issue", "gc-tier-nohistory"}) {
		t.Fatalf("issues tier ids = %v, want [gc-tier-issue gc-tier-nohistory]; rows=%#v", got, issues)
	}
	noHistory := findTestBead(t, issues, "gc-tier-nohistory")
	if noHistory.Ephemeral || !noHistory.NoHistory {
		t.Fatalf("no-history row flags = %#v, want Ephemeral=false NoHistory=true", noHistory)
	}
	durable := findTestBead(t, issues, "gc-tier-issue")
	if durable.Ephemeral || durable.NoHistory {
		t.Fatalf("history row flags = %#v, want Ephemeral=false NoHistory=false", durable)
	}

	wisps, err := store.List(ListQuery{Label: "tier-test", TierMode: TierWisps, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List wisps tier: %v", err)
	}
	if got := testBeadIDs(wisps); !slices.Equal(got, []string{"gc-tier-wisp", "gc-tier-nohistory"}) {
		t.Fatalf("wisps tier ids = %v, want [gc-tier-wisp gc-tier-nohistory]; rows=%#v", got, wisps)
	}
	if ephemeral := findTestBead(t, wisps, "gc-tier-wisp"); !ephemeral.Ephemeral || ephemeral.NoHistory {
		t.Fatalf("ephemeral row flags = %#v, want Ephemeral=true NoHistory=false", ephemeral)
	}

	both, err := store.List(ListQuery{Label: "tier-test", TierMode: TierBoth, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List both tiers: %v", err)
	}
	if got := testBeadIDs(both); !slices.Equal(got, []string{"gc-tier-issue", "gc-tier-wisp", "gc-tier-nohistory"}) {
		t.Fatalf("both tier ids = %v, want [gc-tier-issue gc-tier-wisp gc-tier-nohistory]; rows=%#v", got, both)
	}
}

// TestDoltliteReadStoreLegacyWispsSchemaStaysEphemeralOnly pins backward
// compatibility for doltlite snapshots written before the wisps table carried
// the ephemeral/no_history storage-flag columns (beads migrations 0020/0023):
// without the discriminator every wisp row is ephemeral, so TierIssues must
// keep excluding the whole wisps table and TierWisps must surface its rows
// with Ephemeral=true.
func TestDoltliteReadStoreLegacyWispsSchemaStaysEphemeralOnly(t *testing.T) {
	store, closeStore := newLegacyTestDoltliteReadStore(t)
	defer closeStore()

	issues, err := store.List(ListQuery{Label: "tier-test", Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List issues tier: %v", err)
	}
	if got := testBeadIDs(issues); !slices.Equal(got, []string{"gc-legacy-issue"}) {
		t.Fatalf("issues tier ids = %v, want [gc-legacy-issue]; rows=%#v", got, issues)
	}

	wisps, err := store.List(ListQuery{Label: "tier-test", TierMode: TierWisps, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List wisps tier: %v", err)
	}
	if len(wisps) != 1 || wisps[0].ID != "gc-legacy-wisp" || !wisps[0].Ephemeral {
		t.Fatalf("wisps tier rows = %#v, want only ephemeral gc-legacy-wisp", wisps)
	}

	both, err := store.List(ListQuery{Label: "tier-test", TierMode: TierBoth, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List both tiers: %v", err)
	}
	if got := testBeadIDs(both); !slices.Equal(got, []string{"gc-legacy-issue", "gc-legacy-wisp"}) {
		t.Fatalf("both tier ids = %v, want [gc-legacy-issue gc-legacy-wisp]; rows=%#v", got, both)
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

// TestDoltliteReadStoreLimitCutsDeterministicPrefixOnCreatedAtTies pins the
// (created_at, id) total order at the SQL layer (#3208): when rows share a
// created_at timestamp, a LIMIT-bounded read must cut the same prefix on
// every call. Without the id tiebreaker in ORDER BY, SQLite resolves ties in
// unspecified (rowid/insertion) order and the bounded subset is arbitrary.
func TestDoltliteReadStoreLimitCutsDeterministicPrefixOnCreatedAtTies(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	tie := doltliteSQLiteTime(time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC))
	// Insert in an order (c, a, b) that differs from both id directions so
	// an insertion-ordered tie-cut cannot accidentally match the contract.
	for _, id := range []string{"gc-tie-c", "gc-tie-a", "gc-tie-b"} {
		if _, err := writer.Exec(`INSERT INTO issues (
			id, title, status, issue_type, priority, created_at, updated_at,
			assignee, description, design, acceptance_criteria, notes, metadata
		) VALUES (?, ?, 'open', 'task', 2, ?, ?, 'rig/tie-order', '', '', '', '', '{}')`,
			id, id, tie, tie); err != nil {
			t.Fatalf("insert tie issue %s: %v", id, err)
		}
	}

	descTop2, err := store.List(ListQuery{
		Assignee:   "rig/tie-order",
		Sort:       SortCreatedDesc,
		Limit:      2,
		SkipLabels: true,
	})
	if err != nil {
		t.Fatalf("List desc limit 2: %v", err)
	}
	if got := testBeadIDs(descTop2); !slices.Equal(got, []string{"gc-tie-c", "gc-tie-b"}) {
		t.Fatalf("desc limit-2 ids = %v, want [gc-tie-c gc-tie-b]", got)
	}

	ascAll, err := store.List(ListQuery{
		Assignee:   "rig/tie-order",
		Sort:       SortCreatedAsc,
		SkipLabels: true,
	})
	if err != nil {
		t.Fatalf("List asc: %v", err)
	}
	if got := testBeadIDs(ascAll); !slices.Equal(got, []string{"gc-tie-a", "gc-tie-b", "gc-tie-c"}) {
		t.Fatalf("asc ids = %v, want [gc-tie-a gc-tie-b gc-tie-c]", got)
	}
}

// TestDoltliteReadStoreBoundedMultiTableMergeKeepsGlobalTopN pins the
// cross-table merge contract for bounded TierIssues reads (#3444). TierIssues
// now spans both the issues and wisps tables, and the merge dedupes by id
// before the global sort+limit. A per-table SQL LIMIT must therefore not be
// pushed for multi-table reads: if it were, a table whose limited prefix is
// entirely cross-table duplicates would never surface a later unique row that
// belongs in the global top-N. Here gc-dup-a and gc-dup-b are durable issues
// that outrank their no-history wisp twins, so a pushed per-table LIMIT 2
// would fetch only the duplicated wisps prefix (a@99, b@98) and never see the
// unique durable wisp gc-dup-c@97 that sorts into the true top-2.
func TestDoltliteReadStoreBoundedMultiTableMergeKeepsGlobalTopN(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	base := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	for _, issue := range []testDoltliteIssue{
		{ID: "gc-dup-a", Title: "dup a issue", CreatedAt: base.Add(100 * time.Second), Labels: []string{"dup-prefix"}},
		{ID: "gc-dup-b", Title: "dup b issue", CreatedAt: base.Add(1 * time.Second), Labels: []string{"dup-prefix"}},
	} {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", issue)
	}
	for _, wisp := range []testDoltliteIssue{
		{ID: "gc-dup-a", Title: "dup a wisp", CreatedAt: base.Add(99 * time.Second), Labels: []string{"dup-prefix"}, NoHistory: true},
		{ID: "gc-dup-b", Title: "dup b wisp", CreatedAt: base.Add(98 * time.Second), Labels: []string{"dup-prefix"}, NoHistory: true},
		{ID: "gc-dup-c", Title: "dup c wisp", CreatedAt: base.Add(97 * time.Second), Labels: []string{"dup-prefix"}, NoHistory: true},
	} {
		insertTestDoltliteIssue(t, writer, "wisps", "wisp_labels", "wisp_dependencies", wisp)
	}

	// Unbounded TierIssues read is the ground truth: the durable issue rows
	// plus the deduped unique no-history wisp (the a/b wisp twins fold into
	// their issue rows), sorted created-desc.
	all, err := store.List(ListQuery{Label: "dup-prefix", Sort: SortCreatedDesc, SkipLabels: true})
	if err != nil {
		t.Fatalf("List unbounded: %v", err)
	}
	if got := testBeadIDs(all); !slices.Equal(got, []string{"gc-dup-a", "gc-dup-c", "gc-dup-b"}) {
		t.Fatalf("unbounded ids = %v, want [gc-dup-a gc-dup-c gc-dup-b]", got)
	}

	// The bounded read must equal the unbounded top-2, not the per-table
	// limited prefix [gc-dup-a gc-dup-b].
	top2, err := store.List(ListQuery{Label: "dup-prefix", Sort: SortCreatedDesc, Limit: 2, SkipLabels: true})
	if err != nil {
		t.Fatalf("List limit 2: %v", err)
	}
	if got := testBeadIDs(top2); !slices.Equal(got, []string{"gc-dup-a", "gc-dup-c"}) {
		t.Fatalf("bounded top-2 ids = %v, want [gc-dup-a gc-dup-c]", got)
	}
}

// TestDoltliteReadStoreBoundedTopNAvoidsFullHistoryHydration pins the bounded
// multi-table read fix (#3449 review). A limited default-tier read now selects
// the exact global top-N ids in one SQL statement and hydrates only those ids,
// instead of reading every matching row from both tables and cutting in Go.
// The dataset's highest-sorted row is an ephemeral wisp that must stay out of
// TierIssues, and a no-history wisp twin folds into its durable issue, so the
// bounded result must equal the unbounded top-N across the issues/wisps
// boundary while the id selection itself stays O(limit).
func TestDoltliteReadStoreBoundedTopNAvoidsFullHistoryHydration(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	base := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	for _, issue := range []testDoltliteIssue{
		{ID: "gc-bt-i1", Title: "i1", CreatedAt: base.Add(10 * time.Second), Labels: []string{"bt"}},
		{ID: "gc-bt-i2", Title: "i2", CreatedAt: base.Add(8 * time.Second), Labels: []string{"bt"}},
		{ID: "gc-bt-i3", Title: "i3", CreatedAt: base.Add(2 * time.Second), Labels: []string{"bt"}},
	} {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", issue)
	}
	for _, wisp := range []testDoltliteIssue{
		{ID: "gc-bt-w1", Title: "w1", CreatedAt: base.Add(9 * time.Second), Labels: []string{"bt"}, NoHistory: true},
		{ID: "gc-bt-i2", Title: "i2 wisp twin", CreatedAt: base.Add(7 * time.Second), Labels: []string{"bt"}, NoHistory: true},
		{ID: "gc-bt-w2", Title: "w2", CreatedAt: base.Add(5 * time.Second), Labels: []string{"bt"}, NoHistory: true},
		{ID: "gc-bt-eph", Title: "ephemeral", CreatedAt: base.Add(100 * time.Second), Labels: []string{"bt"}, Ephemeral: true},
	} {
		insertTestDoltliteIssue(t, writer, "wisps", "wisp_labels", "wisp_dependencies", wisp)
	}

	// Ground truth: the ephemeral wisp is excluded from TierIssues, the i2 wisp
	// twin folds into its durable issue, sorted created-desc.
	wantUnbounded := []string{"gc-bt-i1", "gc-bt-w1", "gc-bt-i2", "gc-bt-w2", "gc-bt-i3"}
	all, err := store.List(ListQuery{Label: "bt", Sort: SortCreatedDesc})
	if err != nil {
		t.Fatalf("List unbounded: %v", err)
	}
	if got := testBeadIDs(all); !slices.Equal(got, wantUnbounded) {
		t.Fatalf("unbounded ids = %v, want %v", got, wantUnbounded)
	}

	// The id selection is bounded and exact: it returns exactly the top-3 ids
	// (not all five matches), proving the SQL LIMIT is applied during selection
	// rather than after hydrating full matching history.
	sets := doltliteTableSetsForMode(TierIssues)
	ids, err := store.selectBoundedTopNIDs(ListQuery{Label: "bt", Sort: SortCreatedDesc}, sets, 3)
	if err != nil {
		t.Fatalf("selectBoundedTopNIDs: %v", err)
	}
	if !slices.Equal(ids, []string{"gc-bt-i1", "gc-bt-w1", "gc-bt-i2"}) {
		t.Fatalf("selected top-3 ids = %v, want [gc-bt-i1 gc-bt-w1 gc-bt-i2]", ids)
	}

	// End to end the bounded List equals the unbounded top-3, and the hydrated
	// rows carry their labels and storage flags across both tables.
	top3, err := store.List(ListQuery{Label: "bt", Sort: SortCreatedDesc, Limit: 3})
	if err != nil {
		t.Fatalf("List limit 3: %v", err)
	}
	if got := testBeadIDs(top3); !slices.Equal(got, []string{"gc-bt-i1", "gc-bt-w1", "gc-bt-i2"}) {
		t.Fatalf("bounded top-3 ids = %v, want [gc-bt-i1 gc-bt-w1 gc-bt-i2]", got)
	}
	w1 := findTestBead(t, top3, "gc-bt-w1")
	if !slices.Contains(w1.Labels, "bt") {
		t.Fatalf("hydrated wisp gc-bt-w1 missing label bt: %#v", w1.Labels)
	}
	if !w1.NoHistory {
		t.Fatalf("hydrated wisp gc-bt-w1 should be NoHistory: %#v", w1)
	}

	// TierBoth takes the same bounded fast path but applies no tier filter, so
	// the ephemeral wisp (highest created_at) is the bounded top-1 — the
	// opposite of the TierIssues result above.
	bothTop1, err := store.List(ListQuery{Label: "bt", Sort: SortCreatedDesc, Limit: 1, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List TierBoth limit 1: %v", err)
	}
	if got := testBeadIDs(bothTop1); !slices.Equal(got, []string{"gc-bt-eph"}) {
		t.Fatalf("TierBoth bounded top-1 ids = %v, want [gc-bt-eph]", got)
	}
}

// TestDoltliteReadStoreBoundedSameSecondPrefixMatchesUnbounded pins the #3449
// review fix for sub-second precision: scanBead truncates CreatedAt to whole
// seconds, so the Go merge orders same-second rows by id. A bounded read's SQL
// LIMIT must cut on that same second-granular key, not the raw sub-second
// created_at text, or it selects a different prefix than the unbounded merge.
// The sub-second offsets below are the exact reverse of the id order, so a raw
// ordering would invert the canonical prefix at every limit boundary. Covers
// both the multi-table bounded path (selectBoundedTopNIDs) and the single-table
// limited path (queryIssueTable).
func TestDoltliteReadStoreBoundedSameSecondPrefixMatchesUnbounded(t *testing.T) {
	base := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)

	assertPrefixParity := func(t *testing.T, store *DoltliteReadStore, tier TierMode) {
		t.Helper()
		for _, sort := range []SortOrder{SortCreatedDesc, SortCreatedAsc} {
			unbounded, err := store.List(ListQuery{Label: "ss", Sort: sort, TierMode: tier})
			if err != nil {
				t.Fatalf("List unbounded (tier=%v sort=%v): %v", tier, sort, err)
			}
			if len(unbounded) != 4 {
				t.Fatalf("unbounded len = %d, want 4 (ids %v)", len(unbounded), testBeadIDs(unbounded))
			}
			for k := 1; k <= 4; k++ {
				bounded, err := store.List(ListQuery{Label: "ss", Sort: sort, TierMode: tier, Limit: k})
				if err != nil {
					t.Fatalf("List(tier=%v sort=%v limit=%d): %v", tier, sort, k, err)
				}
				got, want := testBeadIDs(bounded), testBeadIDs(unbounded[:k])
				if !slices.Equal(got, want) {
					t.Fatalf("bounded prefix (tier=%v sort=%v limit=%d) = %v, want unbounded prefix %v", tier, sort, k, got, want)
				}
			}
		}
	}

	t.Run("tier_issues_multi_table", func(t *testing.T) {
		issues := []testDoltliteIssue{
			{ID: "gc-ss-a", Title: "a", CreatedAt: base.Add(800 * time.Millisecond), Labels: []string{"ss"}},
			{ID: "gc-ss-c", Title: "c", CreatedAt: base.Add(400 * time.Millisecond), Labels: []string{"ss"}},
		}
		wisps := []testDoltliteIssue{
			{ID: "gc-ss-b", Title: "b", CreatedAt: base.Add(600 * time.Millisecond), Labels: []string{"ss"}, NoHistory: true},
			{ID: "gc-ss-d", Title: "d", CreatedAt: base.Add(200 * time.Millisecond), Labels: []string{"ss"}, NoHistory: true},
		}
		assertPrefixParity(t, newDoltliteStoreWithRows(t, issues, wisps), TierIssues)
	})

	t.Run("tier_wisps_single_table", func(t *testing.T) {
		wisps := []testDoltliteIssue{
			{ID: "gc-ss-a", Title: "a", CreatedAt: base.Add(800 * time.Millisecond), Labels: []string{"ss"}, NoHistory: true},
			{ID: "gc-ss-b", Title: "b", CreatedAt: base.Add(600 * time.Millisecond), Labels: []string{"ss"}, NoHistory: true},
			{ID: "gc-ss-c", Title: "c", CreatedAt: base.Add(400 * time.Millisecond), Labels: []string{"ss"}, NoHistory: true},
			{ID: "gc-ss-d", Title: "d", CreatedAt: base.Add(200 * time.Millisecond), Labels: []string{"ss"}, NoHistory: true},
		}
		assertPrefixParity(t, newDoltliteStoreWithRows(t, nil, wisps), TierWisps)
	})
}

// TestDoltliteReadStoreBoundedHydrationChunksLargeIDSet pins the deep-pagination
// fix for the bounded multi-table read (#3449 review). The bounded path selects
// up to `limit` ids in one SQL statement and hydrates them with an
// `i.id IN (?,...)` clause. The native DoltLite driver (modernc.org/sqlite) caps
// a statement at SQLITE_MAX_VARIABLE_NUMBER = 32766 bound parameters, and the
// API sets query.Limit to the cursor Offset+Limit with the offset uncapped
// (internal/api/pagination.go), so a deep cursor walk over a large rig can select
// more ids than the cap. An unchunked IN clause then overflows with
// "too many SQL variables" and the read 500s on its last page(s) — a
// success→error regression on exactly the large-rig hot path this PR optimizes,
// invisible to first-page reads. Seed more than the cap worth of matching rows
// and assert a large-limit List succeeds and returns the exact global order.
func TestDoltliteReadStoreBoundedHydrationChunksLargeIDSet(t *testing.T) {
	// 32766 is modernc.org/sqlite's SQLITE_MAX_VARIABLE_NUMBER; seed above it so
	// an unchunked hydrate IN clause would overflow.
	const rowCount = 33000
	store := newDoltliteStoreWithBulkIssues(t, rowCount)

	// limit > rowCount so the bounded selection returns every matching id: the
	// hydrate clause therefore carries all rowCount ids, exceeding the variable
	// cap unless hydrateBeadsByID chunks it.
	got, err := store.List(ListQuery{AllowScan: true, Sort: SortCreatedAsc, Limit: rowCount + 1000})
	if err != nil {
		t.Fatalf("large-limit bounded List failed (deep-pagination IN overflow regression): %v", err)
	}
	if len(got) != rowCount {
		t.Fatalf("List returned %d rows, want %d", len(got), rowCount)
	}
	// Chunked hydration must not change which ids survive the cut or their order:
	// the rows are id-ordered with strictly increasing created_at, so the asc
	// result is the seed order from first to last id.
	if got[0].ID != "gc-bulk-000000" {
		t.Fatalf("first row = %q, want gc-bulk-000000", got[0].ID)
	}
	if last, want := got[len(got)-1].ID, fmt.Sprintf("gc-bulk-%06d", rowCount-1); last != want {
		t.Fatalf("last row = %q, want %q", last, want)
	}
}

// newDoltliteStoreWithBulkIssues seeds a fresh DoltLite snapshot with n minimal
// durable task issues inside one transaction (autocommit would fsync per row,
// which is far too slow for the tens of thousands of rows the variable-cap
// regression needs) and returns a read store over it. Rows are id-ordered
// gc-bulk-NNNNNN with strictly increasing created_at so the bounded read order
// is deterministic.
func newDoltliteStoreWithBulkIssues(t testing.TB, n int) *DoltliteReadStore {
	t.Helper()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "doltlite"), 0o755); err != nil {
		t.Fatalf("mkdir doltlite dir: %v", err)
	}
	meta := []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	dbPath := filepath.Join(beadsDir, "doltlite", "hq.db")
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open doltlite fixture db: %v", err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	createTestDoltliteSchema(t, db)

	base := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin bulk insert: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO issues (
		id, title, status, issue_type, priority, created_at, updated_at,
		assignee, description, design, acceptance_criteria, notes, metadata,
		ephemeral, no_history
	) VALUES (?, ?, 'open', 'task', 2, ?, ?, '', '', '', '', '', '{}', 0, 0)`)
	if err != nil {
		t.Fatalf("prepare bulk insert: %v", err)
	}
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		id := fmt.Sprintf("gc-bulk-%06d", i)
		if _, err := stmt.Exec(id, id, ts, ts); err != nil {
			t.Fatalf("bulk insert row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close bulk insert stmt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit bulk insert: %v", err)
	}

	backing := NewBdStore(dir, func(string, string, ...string) ([]byte, error) {
		t.Fatal("backing bd runner should not be called by doltlite read tests")
		return nil, nil
	})
	store, err := NewDoltliteReadStore(dir, backing)
	if err != nil {
		t.Fatalf("NewDoltliteReadStore: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// TestDoltliteReadStoreCustomOrderByRejectsMultiTableSet pins the invariant that
// a custom orderBy must target a single table set (#3449 review). The merged
// read path applies a custom orderBy per table in SQL and skips the cross-table
// Go re-sort, so a multi-table set would silently concatenate independently
// ordered tables. The public List path always passes the default (empty)
// orderBy and Ready passes a single issues-only set, so this guard never fires
// in production; it converts a latent silent-misordering into an explicit error
// for any future caller.
func TestDoltliteReadStoreCustomOrderByRejectsMultiTableSet(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()

	sets := doltliteTableSetsForMode(TierIssues)
	if len(sets) < 2 {
		t.Fatalf("TierIssues table sets = %d, want >= 2 to exercise the guard", len(sets))
	}
	const customOrder = "ORDER BY i.created_at ASC, i.id ASC"
	if _, err := store.queryIssuesOrderedInTables(ListQuery{AllowScan: true}, sets, "", nil, 0, customOrder); err == nil {
		t.Fatal("custom orderBy with multiple table sets should error, got nil")
	} else if !strings.Contains(err.Error(), "single table set") {
		t.Fatalf("error = %q, want it to mention the single-table-set invariant", err)
	}

	// The same custom orderBy against a single table set is allowed.
	if _, err := store.queryIssuesOrderedInTables(ListQuery{AllowScan: true}, []doltliteTableSet{doltliteIssueTables}, "", nil, 0, customOrder); err != nil {
		t.Fatalf("custom orderBy with a single table set should succeed, got %v", err)
	}
}

// TestDoltliteReadStoreReadyLimitCutsDeterministicPrefixOnTies pins the same
// (#3208) tie-cut contract for the Ready path, whose custom ORDER BY
// (priority, created_at) also needs the id tiebreaker for a deterministic
// LIMIT prefix when rows share both keys.
func TestDoltliteReadStoreReadyLimitCutsDeterministicPrefixOnTies(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup

	tie := doltliteSQLiteTime(time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC))
	// Insert in an order (c, a, b) that differs from both id directions so
	// an insertion-ordered tie-cut cannot accidentally match the contract.
	for _, id := range []string{"gc-rtie-c", "gc-rtie-a", "gc-rtie-b"} {
		if _, err := writer.Exec(`INSERT INTO issues (
			id, title, status, issue_type, priority, created_at, updated_at,
			assignee, description, design, acceptance_criteria, notes, metadata
		) VALUES (?, ?, 'open', 'task', 2, ?, ?, 'rig/rtie-order', '', '', '', '', '{}')`,
			id, id, tie, tie); err != nil {
			t.Fatalf("insert ready tie issue %s: %v", id, err)
		}
	}

	top2, err := store.Ready(ReadyQuery{Assignee: "rig/rtie-order", Limit: 2})
	if err != nil {
		t.Fatalf("Ready limit 2: %v", err)
	}
	if got := testBeadIDs(top2); !slices.Equal(got, []string{"gc-rtie-a", "gc-rtie-b"}) {
		t.Fatalf("ready limit-2 ids = %v, want [gc-rtie-a gc-rtie-b]", got)
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
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000")
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
		Ephemeral: true,
	})
	insertTestDoltliteIssue(t, db, "wisps", "wisp_labels", "wisp_dependencies", testDoltliteIssue{
		ID:        "gc-tier-nohistory",
		Title:     "tier no-history",
		Status:    "open",
		IssueType: "task",
		CreatedAt: now.Add(5 * time.Second),
		Assignee:  "rig/nohistory-worker",
		Labels:    []string{"tier-test"},
		Metadata:  map[string]string{"kind": "no-history"},
		NoHistory: true,
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
	Ephemeral    bool
	NoHistory    bool
}

// createTestDoltliteSchema mirrors the snapshot schema the current DoltLite
// beads backend writes: the upstream wisps/no-history migrations (0020/0023)
// give both row tables ephemeral and no_history storage-flag columns.
// createLegacyTestDoltliteSchema covers snapshots from before those columns.
func createTestDoltliteSchema(t testing.TB, db *sql.DB) {
	t.Helper()
	const storageFlagColumns = `,
			ephemeral INTEGER DEFAULT 0,
			no_history INTEGER DEFAULT 0`
	createTestDoltliteSchemaWithRowColumns(t, db, storageFlagColumns)
}

// createLegacyTestDoltliteSchema mirrors doltlite snapshots written before
// the wisps table carried storage-flag columns: every wisps row is ephemeral.
func createLegacyTestDoltliteSchema(t testing.TB, db *sql.DB) {
	t.Helper()
	createTestDoltliteSchemaWithRowColumns(t, db, "")
}

func createTestDoltliteSchemaWithRowColumns(t testing.TB, db *sql.DB, extraRowColumns string) {
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
			metadata TEXT` + extraRowColumns + `
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
			metadata TEXT` + extraRowColumns + `
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

func insertTestDoltliteIssue(t testing.TB, db *sql.DB, issueTable, labelTable, depTable string, issue testDoltliteIssue) {
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
		assignee, description, design, acceptance_criteria, notes, metadata,
		ephemeral, no_history
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', ?, ?, ?)`,
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
		boolToTestInt(issue.Ephemeral),
		boolToTestInt(issue.NoHistory),
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

func boolToTestInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// newLegacyTestDoltliteReadStore builds a read store over a pre-storage-flag
// snapshot (no ephemeral/no_history columns) seeded with one durable issue
// and one wisps row, both labeled tier-test.
func newLegacyTestDoltliteReadStore(t *testing.T) (*DoltliteReadStore, func()) {
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
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open legacy doltlite fixture db: %v", err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	createLegacyTestDoltliteSchema(t, db)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{
			`INSERT INTO issues (
			id, title, status, issue_type, priority, created_at, updated_at,
			assignee, description, design, acceptance_criteria, notes, metadata
		) VALUES (?, ?, 'open', 'task', 2, ?, ?, '', '', '', '', '', '{}')`,
			[]any{"gc-legacy-issue", "legacy issue", now, now},
		},
		{`INSERT INTO labels (issue_id, label) VALUES ('gc-legacy-issue', 'tier-test')`, nil},
		{
			`INSERT INTO wisps (
			id, title, status, issue_type, priority, created_at, updated_at,
			assignee, description, design, acceptance_criteria, notes, metadata
		) VALUES (?, ?, 'open', 'task', 2, ?, ?, '', '', '', '', '', '{}')`,
			[]any{"gc-legacy-wisp", "legacy wisp", now, now},
		},
		{`INSERT INTO wisp_labels (issue_id, label) VALUES ('gc-legacy-wisp', 'tier-test')`, nil},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed legacy doltlite fixture: %v\nstmt: %s", err, stmt.sql)
		}
	}

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

	writer, err := sql.Open("sqlite", "file:"+dbPath+"?mode=rw&_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open writable doltlite db: %v", err)
	}
	return writer
}
