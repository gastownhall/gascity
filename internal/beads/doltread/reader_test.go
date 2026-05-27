package doltread

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestValidateSupportedRejectsBroadHistoryAndRawStatuses(t *testing.T) {
	for _, query := range []beads.ListQuery{
		{IncludeClosed: true},
		{Status: "closed"},
		{Status: "closed", Limit: 1},
		{IncludeClosed: true, Label: "order-run:digest"},
		{IncludeClosed: true, ExcludeType: "epic", Limit: 1},
		{Status: "blocked"},
	} {
		if err := validateSupported(query); !errors.Is(err, beads.ErrIndexedListUnsupported) {
			t.Fatalf("validateSupported(%+v) = %v, want ErrIndexedListUnsupported", query, err)
		}
	}

	for _, query := range []beads.ListQuery{
		{},
		{Status: "open"},
		{Status: "in_progress"},
		{IncludeClosed: true, Label: "order-run:digest", Limit: 1},
		{Status: "closed", Label: "order-tracking", Limit: 5},
		{IncludeClosed: true, ParentID: "bd-parent"},
		{IncludeClosed: true, Metadata: map[string]string{"configured_named_identity": "gastown.deacon"}},
		{Status: "closed", Metadata: map[string]string{"session_name": "vgc-lyr"}},
		{IncludeClosed: true, Type: "session"},
		{IncludeClosed: true, Label: "gc:session"},
	} {
		if err := validateSupported(query); err != nil {
			t.Fatalf("validateSupported(%+v) = %v, want nil", query, err)
		}
	}
}

func TestValidateSupportedStatusAllowsBroadCountShapes(t *testing.T) {
	for _, query := range []beads.ListQuery{
		{IncludeClosed: true},
		{Status: "closed"},
		{IncludeClosed: true, Label: "order-run:digest"},
	} {
		if err := validateSupportedStatus(query); err != nil {
			t.Fatalf("validateSupportedStatus(%+v) = %v, want nil", query, err)
		}
	}
	if err := validateSupportedStatus(beads.ListQuery{Status: "blocked"}); !errors.Is(err, beads.ErrIndexedListUnsupported) {
		t.Fatalf("validateSupportedStatus(blocked) = %v, want ErrIndexedListUnsupported", err)
	}
}

func TestBuildDSNUsesManagedDoltDriverDefaults(t *testing.T) {
	dsn := buildDSN(Config{
		Host:     "127.0.0.1",
		Port:     3317,
		Database: "hq",
	})

	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	if cfg.User != "root" {
		t.Fatalf("User = %q, want root", cfg.User)
	}
	if cfg.Net != "tcp" || cfg.Addr != "127.0.0.1:3317" || cfg.DBName != "hq" {
		t.Fatalf("target = net:%q addr:%q db:%q, want tcp 127.0.0.1:3317 hq", cfg.Net, cfg.Addr, cfg.DBName)
	}
	if !cfg.ParseTime {
		t.Fatal("ParseTime = false, want true")
	}
	if !cfg.AllowNativePasswords {
		t.Fatal("AllowNativePasswords = false, want true for Dolt SQL auth")
	}
	if cfg.Timeout != 10*time.Second || cfg.ReadTimeout != 10*time.Second || cfg.WriteTimeout != 10*time.Second {
		t.Fatalf("timeouts = %s/%s/%s, want 10s/10s/10s", cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout)
	}
}

func TestBuildListSQLUsesBoundedSplitDependencySelectors(t *testing.T) {
	createdBefore := time.Date(2026, 5, 25, 16, 30, 0, 0, time.UTC)
	sqlText, args := buildListSQL(beads.ListQuery{
		Status:        "open",
		Type:          "task",
		ExcludeType:   "epic",
		Label:         "ready",
		Assignee:      "rig/agent",
		ParentID:      "bd-parent",
		CreatedBefore: createdBefore,
		Metadata: map[string]string{
			"plain":        "value",
			"gc.routed_to": "refinery",
		},
		Limit: 5,
		Sort:  beads.SortCreatedAsc,
	}, tierIssues, true)

	for _, want := range []string{
		"FROM labels l JOIN issues b ON b.id = l.issue_id",
		"JOIN dependencies d ON d.issue_id = b.id AND d.type = 'parent-child'",
		"b.status NOT IN ('closed', 'in_progress')",
		"b.issue_type <> ?",
		"l.label = ?",
		"COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?",
		"JSON_UNQUOTE(JSON_EXTRACT(b.metadata, ?)) = ?",
		"ORDER BY b.created_at ASC, b.id ASC LIMIT ?",
	} {
		if !strings.Contains(sqlText, want) {
			t.Fatalf("SQL missing %q:\n%s", want, sqlText)
		}
	}
	if strings.Contains(sqlText, "d.depends_on_id") {
		t.Fatalf("SQL references legacy physical dependency column:\n%s", sqlText)
	}

	wantArgs := []any{
		"task",
		"epic",
		"rig/agent",
		createdBefore,
		`$."gc.routed_to"`,
		"refinery",
		`$."plain"`,
		"value",
		"ready",
		"bd-parent",
		5,
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
}

func TestBuildListSQLStatusSemantics(t *testing.T) {
	sqlText, args := buildListSQL(beads.ListQuery{AllowScan: true}, tierIssues, true)
	if !strings.Contains(sqlText, "b.status <> 'closed'") {
		t.Fatalf("no-status SQL = %s, want non-closed predicate", sqlText)
	}
	if len(args) != 0 {
		t.Fatalf("no-status args = %#v, want none", args)
	}

	sqlText, args = buildListSQL(beads.ListQuery{Status: "in_progress"}, tierIssues, true)
	if !strings.Contains(sqlText, "b.status = ?") {
		t.Fatalf("in_progress SQL = %s, want raw status equality", sqlText)
	}
	if !reflect.DeepEqual(args, []any{"in_progress"}) {
		t.Fatalf("in_progress args = %#v, want [in_progress]", args)
	}

	sqlText, args = buildListSQL(beads.ListQuery{Status: "closed", Label: "order-tracking", Limit: 5}, tierIssues, true)
	if !strings.Contains(sqlText, "b.status = ?") {
		t.Fatalf("closed SQL = %s, want raw status equality", sqlText)
	}
	if !reflect.DeepEqual(args, []any{"closed", "order-tracking", 5}) {
		t.Fatalf("closed args = %#v, want [closed order-tracking 5]", args)
	}

	sqlText, args = buildListSQL(beads.ListQuery{IncludeClosed: true, Label: "order-run:digest", Limit: 1}, tierIssues, true)
	if strings.Contains(sqlText, "b.status <> 'closed'") ||
		strings.Contains(sqlText, "b.status NOT IN") ||
		strings.Contains(sqlText, "b.status = ?") {
		t.Fatalf("include-closed SQL has active status predicate:\n%s", sqlText)
	}
	if !reflect.DeepEqual(args, []any{"order-run:digest", 1}) {
		t.Fatalf("include-closed args = %#v, want [order-run:digest 1]", args)
	}
}

func TestBuildCountSQLSupportsBroadIncludeClosedWithoutHydration(t *testing.T) {
	sqlText, args := buildCountSQL(beads.ListQuery{AllowScan: true, IncludeClosed: true}, tierIssues)
	if strings.Contains(sqlText, "ORDER BY") || strings.Contains(sqlText, "JSON_ARRAYAGG") {
		t.Fatalf("count SQL should not hydrate or sort rows:\n%s", sqlText)
	}
	if !strings.Contains(sqlText, "SELECT COUNT(*) FROM issues b WHERE 1=1") {
		t.Fatalf("count SQL = %s, want broad count over issues", sqlText)
	}
	if len(args) != 0 {
		t.Fatalf("count args = %#v, want none", args)
	}

	sqlText, args = buildCountSQL(beads.ListQuery{Status: "closed", Label: "order-tracking"}, tierIssues)
	if !strings.Contains(sqlText, "FROM labels l JOIN issues b ON b.id = l.issue_id") ||
		!strings.Contains(sqlText, "b.status = ?") ||
		!strings.Contains(sqlText, "l.label = ?") {
		t.Fatalf("filtered count SQL missing status/label predicates:\n%s", sqlText)
	}
	if !reflect.DeepEqual(args, []any{"closed", "order-tracking"}) {
		t.Fatalf("filtered count args = %#v, want [closed order-tracking]", args)
	}
}

func TestBuildListSQLLimitZeroMeansUnlimited(t *testing.T) {
	sqlText, args := buildListSQL(beads.ListQuery{Status: "open", Limit: 0}, tierIssues, true)
	if strings.Contains(sqlText, " LIMIT ") {
		t.Fatalf("limit=0 SQL has LIMIT clause:\n%s", sqlText)
	}
	if len(args) != 0 {
		t.Fatalf("limit=0 args = %#v, want none", args)
	}
}

func TestNormalizeStatusMapsCustomActiveStatusesToOpen(t *testing.T) {
	for _, raw := range []string{"open", "blocked", "deferred", "pinned", "hooked", "custom"} {
		if got := normalizeStatus(raw); got != "open" {
			t.Fatalf("normalizeStatus(%q) = %q, want open", raw, got)
		}
	}
	if got := normalizeStatus("in_progress"); got != "in_progress" {
		t.Fatalf("normalizeStatus(in_progress) = %q", got)
	}
	if got := normalizeStatus("closed"); got != "closed" {
		t.Fatalf("normalizeStatus(closed) = %q", got)
	}
}

func TestBuildListSQLSupportsWispsTier(t *testing.T) {
	sqlText, _ := buildListSQL(beads.ListQuery{
		Status:   "in_progress",
		Label:    "dispatch",
		ParentID: "bd-parent",
	}, tierWisps, false)

	for _, want := range []string{
		"FROM wisp_labels l JOIN wisps b ON b.id = l.issue_id",
		"JOIN wisp_dependencies d ON d.issue_id = b.id AND d.type = 'parent-child'",
		"b.status = ?",
		"l.label = ?",
		"COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external) = ?",
	} {
		if !strings.Contains(sqlText, want) {
			t.Fatalf("SQL missing %q:\n%s", want, sqlText)
		}
	}
	if strings.Contains(sqlText, " LIMIT ") {
		t.Fatalf("SQL has unexpected tier-local limit:\n%s", sqlText)
	}
}

func TestDependencyTargetExprUsesSplitColumns(t *testing.T) {
	got := dependencyTargetExpr("d")
	want := "COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)"
	if got != want {
		t.Fatalf("dependencyTargetExpr() = %q, want %q", got, want)
	}
}

func TestMetadataJSONPathEscapesSpecialCharacters(t *testing.T) {
	got := metadataJSONPath(`gc."route"\owner`)
	want := `$."gc.\"route\"\\owner"`
	if got != want {
		t.Fatalf("metadataJSONPath() = %q, want %q", got, want)
	}
}

func TestMergeTierResultsSortsLimitsAndDedupes(t *testing.T) {
	t1 := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t2.Add(time.Hour)

	result := mergeTierResults(
		beads.ListQuery{Sort: beads.SortCreatedDesc, Limit: 2, TierMode: beads.TierBoth},
		beads.IndexedListResult{
			Beads: []beads.Bead{
				{ID: "bd-a", Status: "open", CreatedAt: t1},
				{ID: "bd-dup", Status: "open", CreatedAt: t2},
			},
			DepsByID:           map[string][]beads.Dep{"bd-a": {{IssueID: "bd-a", DependsOnID: "bd-root"}}},
			DependencyCoverage: true,
			LabelsCoverage:     true,
		},
		beads.IndexedListResult{
			Beads: []beads.Bead{
				{ID: "bd-b", Status: "open", CreatedAt: t3, Ephemeral: true},
				{ID: "bd-dup", Status: "open", CreatedAt: t3, Ephemeral: true},
			},
			DepsByID:           map[string][]beads.Dep{"bd-b": {{IssueID: "bd-b", DependsOnID: "bd-root"}}},
			DependencyCoverage: true,
			LabelsCoverage:     true,
		},
	)

	if len(result.Beads) != 2 {
		t.Fatalf("merged len = %d, want 2", len(result.Beads))
	}
	if got := []string{result.Beads[0].ID, result.Beads[1].ID}; !reflect.DeepEqual(got, []string{"bd-b", "bd-dup"}) {
		t.Fatalf("merged IDs = %v, want [bd-b bd-dup]", got)
	}
	if !result.DependencyCoverage || !result.LabelsCoverage {
		t.Fatalf("coverage = deps:%v labels:%v, want true/true", result.DependencyCoverage, result.LabelsCoverage)
	}
	if _, ok := result.DepsByID["bd-a"]; !ok {
		t.Fatalf("DepsByID missing bd-a: %#v", result.DepsByID)
	}
	if _, ok := result.DepsByID["bd-b"]; !ok {
		t.Fatalf("DepsByID missing bd-b: %#v", result.DepsByID)
	}
}
