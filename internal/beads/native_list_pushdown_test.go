package beads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The pushdown's own tests. A plan may only narrow what crosses from the
// backend; ApplyListQuery still decides the answer, so every pushed predicate
// must be exact against what the reader role does with it.

type planShape struct {
	assignee  string
	status    string
	issueType string
}

func shapesOf(plan []issueops.ListRequest) []planShape {
	out := make([]planShape, 0, len(plan))
	for _, req := range plan {
		out = append(out, planShape{assignee: req.Assignee, status: req.Status, issueType: req.IssueType})
	}
	return out
}

func TestNativeListPlanPushesEveryExactFilter(t *testing.T) {
	many := make([]string, nativeListAssigneeFanOutMax+1)
	for i := range many {
		many[i] = fmt.Sprintf("worker-%d", i)
	}
	for _, tc := range []struct {
		name  string
		query ListQuery
		want  []planShape
	}{
		{
			name:  "one plural assignee is the singular predicate",
			query: ListQuery{Assignees: []string{"worker"}},
			want:  []planShape{{assignee: "worker"}},
		},
		{
			name:  "plural assignees fan out one keyed request each",
			query: ListQuery{Type: "message", Status: "open", Assignees: []string{"worker", "gc-1"}},
			want:  []planShape{{assignee: "worker"}, {assignee: "gc-1"}},
		},
		{
			name:  "duplicate assignees are asked once",
			query: ListQuery{Assignees: []string{"worker", "worker", "gc-1"}},
			want:  []planShape{{assignee: "worker"}, {assignee: "gc-1"}},
		},
		{
			name:  "an empty assignee means unassigned, which the request cannot say",
			query: ListQuery{Assignees: []string{"worker", ""}},
			want:  []planShape{{}},
		},
		{
			name:  "past the fan-out cap the assignees stay client-side",
			query: ListQuery{Assignees: many},
			want:  []planShape{{}},
		},
		{
			name:  "in_progress maps onto one bd status",
			query: ListQuery{Status: "in_progress", Assignees: []string{"worker", "gc-1"}},
			want:  []planShape{{assignee: "worker", status: "in_progress"}, {assignee: "gc-1", status: "in_progress"}},
		},
		{
			name:  "closed maps onto one bd status",
			query: ListQuery{Status: "closed", Label: "gc:session"},
			want:  []planShape{{status: "closed"}},
		},
		{
			name:  "gc open is every bd status but two, so it stays client-side",
			query: ListQuery{Status: "open", Label: "gc:session"},
			want:  []planShape{{}},
		},
		{
			name:  "a registered non-infra type is pushed",
			query: ListQuery{Type: "session", Status: "open"},
			want:  []planShape{{issueType: "session"}},
		},
		{
			name:  "molecule steps are pushed with their status",
			query: ListQuery{Type: "molecule", Status: "in_progress", Assignees: []string{"worker"}},
			want:  []planShape{{assignee: "worker", status: "in_progress", issueType: "molecule"}},
		},
		{
			name:  "an infra type would read the ephemeral plane alone, so it stays client-side",
			query: ListQuery{Type: "message", Assignee: "worker"},
			want:  []planShape{{assignee: "worker"}},
		},
		{
			name:  "rig was a bd default infra type until Jun 2026, so it stays client-side",
			query: ListQuery{Type: "rig", Status: "open"},
			want:  []planShape{{}},
		},
		{
			name:  "a type gc does not register would be refused, so it stays client-side",
			query: ListQuery{Type: "wisp", Status: "in_progress", Assignees: []string{"worker"}},
			want:  []planShape{{assignee: "worker", status: "in_progress"}},
		},
		{
			name:  "the singular assignee is never fanned out",
			query: ListQuery{Assignee: "worker", Status: "closed"},
			want:  []planShape{{assignee: "worker", status: "closed"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shapesOf(NativeListPlan(tc.query))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("plan = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A fanned-out plan never carries a backing limit: each request's page would
// be cut before the union and the client-side filter run.
func TestNativeListPlanNeverPushesALimitOnAFanOut(t *testing.T) {
	query := ListQuery{
		Assignees:     []string{"worker", "gc-1"},
		IncludeClosed: true,
		TierMode:      TierBoth,
		Sort:          SortCreatedAsc,
		Limit:         1,
	}
	for _, req := range NativeListPlan(query) {
		if req.Limit == nil || *req.Limit != 0 {
			t.Fatalf("fanned-out request limit = %v, want an explicit 0", req.Limit)
		}
	}
}

func TestNativeListPlanHooksNeverWalkTheLedger(t *testing.T) {
	routes := []string{"planner-a", "gc-1"}
	for _, query := range []ListQuery{
		{Type: "message", Status: "open", Assignees: routes, TierMode: TierBoth},
		{Type: "molecule", Status: "in_progress", Assignees: routes, TierMode: TierBoth},
		{Type: "wisp", Status: "in_progress", Assignees: routes, TierMode: TierBoth},
		{Status: "in_progress", Assignees: routes, TierMode: TierBoth},
		{Type: "session", Status: "open", TierMode: TierBoth},
	} {
		for _, req := range NativeListPlan(query) {
			if !NativeListRequestKeyed(req) {
				t.Errorf("hook query %+v plans an unkeyed request", query)
			}
		}
	}
}

// The union is by id: a row both requests return is delivered once.
func TestNativeDoltStoreListUnionsFannedOutRowsByID(t *testing.T) {
	rows := map[string][]*beadslib.Issue{
		"worker": {
			{ID: "gc-1", Title: "one", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "worker"},
			{ID: "gc-shared", Title: "shared", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "worker"},
		},
		"gc-9": {
			{ID: "gc-shared", Title: "shared", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "worker"},
			{ID: "gc-2", Title: "two", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "gc-9"},
		},
	}
	var asked []string
	storage := &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			asked = append(asked, req.Assignee)
			var page []*issueops.IssueWithCounts
			for _, issue := range rows[req.Assignee] {
				page = append(page, &issueops.IssueWithCounts{Issue: cloneNativeIssueForTest(issue)})
			}
			return issueops.IssuePage{Items: page}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	got, err := store.List(ListQuery{Type: "message", Assignees: []string{"worker", "gc-9"}, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(asked, []string{"worker", "gc-9"}) {
		t.Fatalf("asked %v, want one request per assignee", asked)
	}
	var ids []string
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	if !slices.Equal(ids, []string{"gc-1", "gc-2", "gc-shared"}) {
		t.Fatalf("List = %v, want the union by id", ids)
	}
	if stats := listRequestStatsForTest(t, store); stats.Requests != 2 || stats.Unkeyed != 0 || stats.Lists != 1 {
		t.Fatalf("ListRequestStatsOf = %+v, want 1 list of 2 keyed requests", stats)
	}
}

func TestNativeDoltStoreListRetriesWithoutARefusedType(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal error
	}{
		{"wire validation", fmt.Errorf("listIssues: %w: issue_type", issueops.ErrValidation)},
		{"local filter validation", errors.New(`invalid issue type "session" (valid: bug, feature, task, epic, chore, decision)`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []string
			storage := &nativeDoltReaderSpy{
				list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
					sent = append(sent, req.IssueType)
					if req.IssueType != "" {
						return issueops.IssuePage{}, tc.refusal
					}
					return issueops.IssuePage{Items: []*issueops.IssueWithCounts{{Issue: &beadslib.Issue{
						ID: "gc-1", Title: "s", Status: beadslib.StatusOpen, IssueType: "session",
					}}}}, nil
				},
			}
			var logs bytes.Buffer
			store := newNativeDoltStoreForTest(storage)
			store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

			for i := 0; i < 2; i++ {
				got, err := store.List(ListQuery{Type: "session", Status: "open"})
				if err != nil {
					t.Fatalf("List #%d: %v", i, err)
				}
				if len(got) != 1 || got[0].ID != "gc-1" {
					t.Fatalf("List #%d = %+v, want the session row", i, got)
				}
			}
			if !slices.Equal(sent, []string{"session", "", ""}) {
				t.Fatalf("sent types %q, want one refused push, its retry, then no push", sent)
			}
			if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
				t.Fatalf("warned %d times, want once:\n%s", n, logs.String())
			}
			if stats := listRequestStatsForTest(t, store); stats.Requests != 3 || stats.Lists != 2 {
				t.Fatalf("ListRequestStatsOf = %+v, want 2 lists of 3 requests, the refused one counted", stats)
			}
		})
	}
}

// A refused type whose type-less retry fails too is not evidence the scope
// lacks gc's types: the refusal is returned, nothing latches, and the next
// listing pushes the type again.
func TestNativeDoltStoreListDoesNotLatchWhenTheRetryFails(t *testing.T) {
	refusal := fmt.Errorf("listIssues: %w: issue_type", issueops.ErrValidation)
	var sent []string
	storage := &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			sent = append(sent, req.IssueType)
			if req.IssueType != "" {
				return issueops.IssuePage{}, refusal
			}
			return issueops.IssuePage{}, errors.New("statement refused for an unrelated reason")
		},
	}
	var logs bytes.Buffer
	store := newNativeDoltStoreForTest(storage)
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for i := 0; i < 2; i++ {
		if _, err := store.List(ListQuery{Type: "session", Status: "open"}); !errors.Is(err, issueops.ErrValidation) {
			t.Fatalf("List #%d = %v, want the original refusal", i, err)
		}
	}
	if store.typePushdownOff.Load() {
		t.Fatal("a failed type-less retry latched type pushdown off")
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("a failed type-less retry warned:\n%s", logs.String())
	}
	if !slices.Equal(sent, []string{"session", "", "session", ""}) {
		t.Fatalf("sent types %q, want the push retried on each list", sent)
	}
}

// infraSettingStorage answers the workspace-config role's types.infra read the
// way a scope configures it, or fails it.
type infraSettingStorage struct {
	*nativeDoltReaderSpy
	value string
	err   error
}

func (s infraSettingStorage) WorkspaceConfig() (issueops.WorkspaceConfig, error) {
	return infraSettingConfig{value: s.value, err: s.err}, nil
}

type infraSettingConfig struct {
	value string
	err   error
}

func (c infraSettingConfig) GetSetting(_ context.Context, req issueops.GetSettingRequest) (issueops.SettingResult, error) {
	if c.err != nil {
		return issueops.SettingResult{}, c.err
	}
	if req.Key != nativeTypesInfraConfigKey {
		return issueops.SettingResult{Key: req.Key}, nil
	}
	return issueops.SettingResult{Key: req.Key, Value: c.value}, nil
}

func (infraSettingConfig) ListSettings(context.Context, issueops.ListSettingsRequest) (issueops.ListSettingsResult, error) {
	panic("infraSettingConfig: ListSettings is not read by the list pushdown")
}

func (infraSettingConfig) SetSetting(context.Context, issueops.SetSettingRequest) (issueops.SetSettingResult, error) {
	panic("infraSettingConfig: SetSetting is not written by the list pushdown")
}

func (infraSettingConfig) UnsetSetting(context.Context, issueops.UnsetSettingRequest) (issueops.UnsetSettingResult, error) {
	panic("infraSettingConfig: UnsetSetting is not written by the list pushdown")
}

func TestNativeListPushableTypesFor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		pushed  []string
		kept    []string
	}{
		{name: "unset", setting: "", pushed: []string{"session", "molecule", "step"}, kept: []string{"agent", "rig", "role", "message"}},
		{name: "a configured infra type", setting: "agent, session ,role", pushed: []string{"molecule", "step"}, kept: []string{"session", "agent", "role"}},
		{name: "a setting that drops a historical default", setting: "molecule", pushed: []string{"session"}, kept: []string{"molecule", "agent", "rig", "role", "message"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nativeListPushableTypesFor(tc.setting)
			for _, typ := range tc.pushed {
				if !got[typ] {
					t.Errorf("%q is not pushable", typ)
				}
			}
			for _, typ := range tc.kept {
				if got[typ] {
					t.Errorf("%q is pushable, want it client-side", typ)
				}
			}
			for typ := range got {
				if !slices.Contains(RequiredCustomTypes, typ) {
					t.Errorf("%q is pushable but gc does not register it", typ)
				}
			}
		})
	}
}

// The store pushes a type only when its scope's types.infra setting says the
// server reads it from the durable plane, and pushes none when it cannot tell.
func TestNativeDoltStoreListPushesOnlyTypesTheScopeKeepsDurable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		err     error
		want    map[string]string
	}{
		{name: "a scope that makes session infra", setting: "agent,role,message,session", want: map[string]string{"session": "", "molecule": "molecule"}},
		{name: "an unreadable setting", err: errors.New("config table unavailable"), want: map[string]string{"session": "", "molecule": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []string
			spy := &nativeDoltReaderSpy{
				list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
					sent = append(sent, req.IssueType)
					return issueops.IssuePage{}, nil
				},
			}
			storage := infraSettingStorage{nativeDoltReaderSpy: spy, value: tc.setting, err: tc.err}
			store := newNativeDoltStoreWithStorage(storage, "native-test")
			var logs bytes.Buffer
			store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			store.loadListPushableTypes(context.Background())
			for typ, wantSent := range tc.want {
				sent = nil
				if _, err := store.List(ListQuery{Type: typ, Status: "in_progress"}); err != nil {
					t.Fatalf("List(%s): %v", typ, err)
				}
				if !slices.Equal(sent, []string{wantSent}) {
					t.Errorf("List(%s) sent types %q, want %q", typ, sent, wantSent)
				}
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != (tc.err != nil) {
				t.Errorf("warned = %v, want %v:\n%s", warned, tc.err != nil, logs.String())
			}
		})
	}
}

// A store built over a bare storage value has read no setting and pushes no
// type.
func TestNativeDoltStoreWithoutASettingPushesNoType(t *testing.T) {
	var sent []string
	store := newNativeDoltStoreWithStorage(&nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			sent = append(sent, req.IssueType)
			return issueops.IssuePage{}, nil
		},
	}, "native-test")
	if _, err := store.List(ListQuery{Type: "session", Status: "in_progress"}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(sent, []string{""}) {
		t.Fatalf("sent types %q, want none pushed", sent)
	}
}

// closableReaderSpy is a reader spy a reconnect may retire: the store closes
// the handle it replaces.
type closableReaderSpy struct {
	*nativeDoltReaderSpy
}

func (closableReaderSpy) Close() error { return nil }

// A List retried by withReadRetry counts what its last attempt sent, not the
// abandoned attempt's requests as well.
func TestNativeDoltStoreListCountsOnlyItsLastAttempt(t *testing.T) {
	dead := closableReaderSpy{&nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			if req.Assignee == "worker" {
				return issueops.IssuePage{Items: []*issueops.IssueWithCounts{nativeIssueRowForTest("gc-1")}}, nil
			}
			return issueops.IssuePage{}, errors.New("[mysql] i/o timeout")
		},
	}}
	healthy := closableReaderSpy{&nativeDoltReaderSpy{
		list: func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
			return issueops.IssuePage{Items: []*issueops.IssueWithCounts{nativeIssueRowForTest("gc-1")}}, nil
		},
	}}
	var reopens int32
	store := storeWithReopen(dead, healthy, &reopens)
	if _, err := store.List(ListQuery{Assignees: []string{"worker", "gc-9"}, TierMode: TierBoth}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if reopens == 0 {
		t.Fatal("the transient failure did not reconnect; the test exercises nothing")
	}
	if stats := listRequestStatsForTest(t, store); stats.Requests != 2 || stats.Rows != 2 || stats.Lists != 1 {
		t.Fatalf("ListRequestStatsOf = %+v, want 1 list of 2 requests returning 2 rows", stats)
	}
}

// Any other failure is the backend's, and is returned rather than retried
// without the type.
func TestNativeDoltStoreListDoesNotRetryABackendFailure(t *testing.T) {
	boom := errors.New("connection reset by peer")
	calls := 0
	storage := &nativeDoltReaderSpy{
		list: func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
			calls++
			return issueops.IssuePage{}, boom
		},
	}
	store := newNativeDoltStoreForTest(storage)
	if _, err := store.List(ListQuery{Type: "session", Status: "open"}); !errors.Is(err, boom) {
		t.Fatalf("List = %v, want the backend failure", err)
	}
	if store.typePushdownOff.Load() {
		t.Fatal("a backend failure latched type pushdown off")
	}
}

func TestNativeDoltStoreListLogsAnUnkeyedPlan(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{})
	var logs bytes.Buffer
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := store.List(ListQuery{Label: "gc:session"}); err != nil {
		t.Fatalf("keyed List: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a keyed list logged:\n%s", logs.String())
	}
	if _, err := store.List(ListQuery{Status: "open", Type: "wisp"}); err != nil {
		t.Fatalf("unkeyed List: %v", err)
	}
	if !strings.Contains(logs.String(), "level=DEBUG") || !strings.Contains(logs.String(), "wisp") {
		t.Fatalf("an unkeyed list did not log its query at debug:\n%s", logs.String())
	}
	if stats := listRequestStatsForTest(t, store); stats.Unkeyed != 1 {
		t.Fatalf("ListRequestStatsOf.Unkeyed = %d, want 1", stats.Unkeyed)
	}
}

// filteringReader answers the reader role over a fixed row set the way
// workapi.BuildListFilter disposes of the request this store sends: exact
// status, type and assignee predicates; an infra type routes the read to the
// ephemeral rows alone; and the pinned default applies unless AllFlag lifts it.
// It is the oracle the pushdown property is proved against.
type filteringReader struct {
	rows []*beadslib.Issue
}

// filteringReaderInfra is a server whose built-in infra default still carries
// rig, as bd's did until Jun 2026.
var filteringReaderInfra = map[string]bool{"agent": true, "rig": true, "role": true, "message": true}

func (r filteringReader) list(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	var page []*issueops.IssueWithCounts
	for _, issue := range r.rows {
		if req.Status != "" && string(issue.Status) != req.Status {
			continue
		}
		if req.IssueType != "" {
			if string(issue.IssueType) != req.IssueType {
				continue
			}
			if filteringReaderInfra[req.IssueType] && !issue.Ephemeral {
				continue
			}
		}
		if req.Assignee != "" && issue.Assignee != req.Assignee {
			continue
		}
		if !req.AllFlag && issue.Pinned {
			continue
		}
		if len(req.Labels) > 0 && !slices.Contains(issue.Labels, req.Labels[0]) {
			continue
		}
		page = append(page, &issueops.IssueWithCounts{Issue: cloneNativeIssueForTest(issue)})
	}
	sort.SliceStable(page, func(i, j int) bool { return readerOrderLess(page[i].Issue, page[j].Issue, req) })
	if req.Limit != nil && *req.Limit > 0 && len(page) > *req.Limit {
		page = page[:*req.Limit]
	}
	return issueops.IssuePage{Items: page}, nil
}

// readerOrderLess is sqlbuild.OrderByForColumns as the oracle's order: no sort
// key is (priority ASC, created_at DESC, id ASC); "created" is created_at DESC
// (ASC when Reverse), ties by id ASC.
func readerOrderLess(a, b *beadslib.Issue, req issueops.ListRequest) bool {
	if req.SortBy == "created" {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if req.Reverse {
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID < b.ID
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID < b.ID
}

func randomPushdownFixture(rng *rand.Rand, n int) []*beadslib.Issue {
	statuses := []beadslib.Status{beadslib.StatusOpen, beadslib.StatusInProgress, beadslib.StatusClosed, "deferred", "blocked", "hooked"}
	types := []string{"message", "session", "molecule", "step", "wisp", "task", "agent", "rig"}
	assignees := []string{"", "planner-a", "gc-1", "worker", "gc-2"}
	rows := make([]*beadslib.Issue, 0, n)
	for i := 0; i < n; i++ {
		issue := &beadslib.Issue{
			ID:        fmt.Sprintf("gc-%03d", i),
			Title:     fmt.Sprintf("row %d", i),
			Status:    statuses[rng.Intn(len(statuses))],
			IssueType: beadslib.IssueType(types[rng.Intn(len(types))]),
			Assignee:  assignees[rng.Intn(len(assignees))],
			Ephemeral: rng.Intn(2) == 0,
			Pinned:    rng.Intn(5) == 0,
			Priority:  rng.Intn(4),
			// A handful of instants, so created_at ties are common.
			CreatedAt: time.Date(2026, 10, 1, 0, 0, rng.Intn(8), 0, time.UTC),
		}
		if issue.Status == beadslib.StatusClosed {
			now := issue.CreatedAt
			issue.ClosedAt = &now
		}
		if rng.Intn(3) == 0 {
			issue.Labels = []string{"gc:session"}
		}
		rows = append(rows, issue)
	}
	return rows
}

func randomPushdownQuery(rng *rand.Rand) ListQuery {
	pick := func(options ...string) string { return options[rng.Intn(len(options))] }
	q := ListQuery{
		Status:        pick("", "", "open", "in_progress", "closed"),
		Type:          pick("", "message", "session", "molecule", "step", "wisp", "agent", "rig"),
		IncludeClosed: rng.Intn(2) == 0,
		TierMode:      TierMode(rng.Intn(3)),
		AllowScan:     true,
	}
	switch rng.Intn(4) {
	case 0:
		q.Assignee = pick("planner-a", "gc-1", "worker")
	case 1:
		n := 1 + rng.Intn(3)
		for i := 0; i < n; i++ {
			q.Assignees = append(q.Assignees, pick("planner-a", "gc-1", "worker", "gc-2", ""))
		}
	case 2:
		q.Label = "gc:session"
	}
	q.Sort = []SortOrder{SortDefault, SortDefault, SortCreatedAsc, SortCreatedDesc}[rng.Intn(4)]
	if rng.Intn(2) == 0 {
		q.Limit = 1 + rng.Intn(5)
	}
	if q.Sort != SortDefault && rng.Intn(3) == 0 {
		q.SeekAfter = &SeekBoundary{
			CreatedAt: time.Date(2026, 10, 1, 0, 0, rng.Intn(8), 0, time.UTC),
			ID:        fmt.Sprintf("gc-%03d", rng.Intn(60)),
		}
	}
	return q
}

// THE PROPERTY: for any query, the pushed-down store answers exactly what
// ApplyListQuery answers over every row, including the pinned rows the request
// must keep lifting and the durable infra-typed rows a pushed infra type would
// lose.
func TestNativeDoltStoreListPushdownAnswersTheUnpushedQuery(t *testing.T) {
	rng := rand.New(rand.NewSource(20261010))
	for round := 0; round < 40; round++ {
		rows := randomPushdownFixture(rng, 60)
		reader := filteringReader{rows: rows}
		store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{list: reader.list})

		all := make([]Bead, 0, len(rows))
		for _, issue := range rows {
			bead, err := beadFromNativeIssue(cloneNativeIssueForTest(issue))
			if err != nil {
				t.Fatalf("beadFromNativeIssue: %v", err)
			}
			bead.Revision = 0
			all = append(all, bead)
		}
		// The unpushed answer is one unfiltered request's rows, in the order
		// that request answers in, through ApplyListQuery.
		defaultOrder := issueops.ListRequest{}
		sort.SliceStable(all, func(i, j int) bool {
			return readerOrderLess(issueForOrder(all[i]), issueForOrder(all[j]), defaultOrder)
		})
		for i := 0; i < 50; i++ {
			query := randomPushdownQuery(rng)
			got, err := store.List(query)
			if err != nil {
				t.Fatalf("List(%+v): %v", query, err)
			}
			want := ApplyListQuery(all, query)
			if gotIDs, wantIDs := beadIDsInOrder(got), beadIDsInOrder(want); !slices.Equal(gotIDs, wantIDs) {
				t.Fatalf("List(%+v)\n got  %v\n want %v", query, gotIDs, wantIDs)
			}
		}
	}
}

func issueForOrder(b Bead) *beadslib.Issue {
	priority := 2
	if b.Priority != nil {
		priority = *b.Priority
	}
	return &beadslib.Issue{ID: b.ID, Priority: priority, CreatedAt: b.CreatedAt}
}

func beadIDsInOrder(items []Bead) []string {
	ids := make([]string, 0, len(items))
	for _, b := range items {
		ids = append(ids, b.ID)
	}
	return ids
}

func TestNativeDoltStoreListKeepsAPinnedInProgressRow(t *testing.T) {
	reader := filteringReader{rows: []*beadslib.Issue{
		{ID: "gc-pinned", Title: "pinned", Status: beadslib.StatusInProgress, IssueType: "molecule", Assignee: "planner-a", Pinned: true},
	}}
	store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{list: reader.list})
	got, err := store.List(ListQuery{Type: "molecule", Status: "in_progress", Assignees: []string{"planner-a", "gc-1"}, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-pinned" {
		t.Fatalf("List = %v, want the pinned in-progress row", sortedBeadIDs(got))
	}
}

func sortedBeadIDs(items []Bead) []string {
	ids := make([]string, 0, len(items))
	for _, b := range items {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}
