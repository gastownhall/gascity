package beads

import (
	"bytes"
	"context"
	"encoding/json"
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
	many := make([]string, 9)
	manyPlan := make([]planShape, 0, len(many))
	for i := range many {
		many[i] = fmt.Sprintf("worker-%d", i)
		manyPlan = append(manyPlan, planShape{assignee: many[i], status: "in_progress"})
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
			// A session's inbox joins its closed predecessors' routes, so a hook
			// can ask for more than one identity's routes; one unkeyed request
			// would walk the whole ledger instead.
			name:  "a large assignee set still fans out one keyed request each",
			query: ListQuery{Status: "in_progress", Assignees: many},
			want:  manyPlan,
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
			// sr-i54yd: a Live read is owed the backing's raw answer, as BdStore's
			// --status=open gives it, so a parked (blocked/deferred) bead is not
			// live work to a lifecycle gate.
			name:  "a Live open is bd's raw open, pushed",
			query: ListQuery{Status: "open", Assignee: "worker", Live: true},
			want:  []planShape{{assignee: "worker", status: "open"}},
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
// lacks gc's types: the refusal is returned with the retry's failure, nothing
// latches, and the next listing pushes the type again.
func TestNativeDoltStoreListDoesNotLatchWhenTheRetryFails(t *testing.T) {
	refusal := fmt.Errorf("listIssues: %w: issue_type", issueops.ErrValidation)
	retryFailure := errors.New("statement refused for an unrelated reason")
	var sent []string
	storage := &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			sent = append(sent, req.IssueType)
			if req.IssueType != "" {
				return issueops.IssuePage{}, refusal
			}
			return issueops.IssuePage{}, retryFailure
		},
	}
	var logs bytes.Buffer
	store := newNativeDoltStoreForTest(storage)
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for i := 0; i < 2; i++ {
		_, err := store.List(ListQuery{Type: "session", Status: "open"})
		if !errors.Is(err, issueops.ErrValidation) || !errors.Is(err, retryFailure) {
			t.Fatalf("List #%d = %v, want the original refusal and the retry's failure", i, err)
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

// A type-less retry that loses its connection reports that loss, not just the
// refusal: withReadRetry reconnects, and the fresh connection answers the
// retry and latches type pushdown off.
func TestNativeDoltStoreListReconnectsWhenTheRetryLosesItsConnection(t *testing.T) {
	refusal := fmt.Errorf("listIssues: %w: issue_type", issueops.ErrValidation)
	refusingReader := func(retry func() (issueops.IssuePage, error)) closableReaderSpy {
		return closableReaderSpy{&nativeDoltReaderSpy{
			list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
				if req.IssueType != "" {
					return issueops.IssuePage{}, refusal
				}
				return retry()
			},
		}}
	}
	dead := refusingReader(func() (issueops.IssuePage, error) {
		return issueops.IssuePage{}, errors.New("[mysql] invalid connection")
	})
	healthy := refusingReader(func() (issueops.IssuePage, error) {
		return issueops.IssuePage{Items: []*issueops.IssueWithCounts{{Issue: &beadslib.Issue{
			ID: "gc-1", Title: "s", Status: beadslib.StatusOpen, IssueType: "session",
		}}}}, nil
	})
	var reopens int32
	store := storeWithReopen(dead, healthy, &reopens)
	got, err := store.List(ListQuery{Type: "session", Status: "open"})
	if err != nil {
		t.Fatalf("List = %v, want the listing answered after a reconnect", err)
	}
	if reopens != 1 {
		t.Fatalf("reopens = %d, want 1: the retry's lost connection must reconnect", reopens)
	}
	if len(got) != 1 || got[0].ID != "gc-1" {
		t.Fatalf("List = %+v, want the session row", got)
	}
	if !store.typePushdownOff.Load() {
		t.Fatal("the retry answered on the fresh connection did not latch type pushdown off")
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

// closableInfraSettingStorage is an infraSettingStorage a reconnect may retire.
type closableInfraSettingStorage struct {
	infraSettingStorage
}

func (closableInfraSettingStorage) Close() error { return nil }

// A store reads its scope's types.infra once, at open. A setting changed
// afterwards does not reach it, not even through a reconnect, so it keeps
// pushing a type the scope has since made infra until its process restarts.
// A store opened after the change honors it
// (TestNativeDoltStoreListPushesOnlyTypesTheScopeKeepsDurable).
func TestNativeDoltStoreListKeepsTheTypesInfraItOpenedWith(t *testing.T) {
	var sent []string
	readerFor := func(fail bool) *nativeDoltReaderSpy {
		return &nativeDoltReaderSpy{
			list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
				if fail {
					return issueops.IssuePage{}, errors.New("[mysql] invalid connection")
				}
				sent = append(sent, req.IssueType)
				return issueops.IssuePage{}, nil
			},
		}
	}
	atOpen := closableInfraSettingStorage{infraSettingStorage{nativeDoltReaderSpy: readerFor(true)}}
	changed := closableInfraSettingStorage{infraSettingStorage{nativeDoltReaderSpy: readerFor(false), value: "agent,role,message,session"}}
	var reopens int32
	store := storeWithReopen(atOpen, changed, &reopens)
	store.loadListPushableTypes(context.Background())

	for i := 0; i < 2; i++ {
		if _, err := store.List(ListQuery{Type: "session", Status: "in_progress"}); err != nil {
			t.Fatalf("List #%d: %v", i, err)
		}
	}
	if reopens != 1 {
		t.Fatalf("reopens = %d, want 1: the test exercises a reconnect onto the changed setting", reopens)
	}
	if !slices.Equal(sent, []string{"session", "session"}) {
		t.Fatalf("sent types %q, want the type read at open pushed on both lists", sent)
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
		Live:          rng.Intn(2) == 0,
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
// lose. A Live "open" is answered over the rows whose RAW status is open
// (nativeListPushesRawOpen).
func TestNativeDoltStoreListPushdownAnswersTheUnpushedQuery(t *testing.T) {
	rng := rand.New(rand.NewSource(20261010))
	for round := 0; round < 40; round++ {
		rows := randomPushdownFixture(rng, 60)
		reader := filteringReader{rows: rows}
		store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{list: reader.list})

		all := make([]Bead, 0, len(rows))
		rawOpen := make(map[string]bool, len(rows))
		for _, issue := range rows {
			bead, err := beadFromNativeIssue(cloneNativeIssueForTest(issue))
			if err != nil {
				t.Fatalf("beadFromNativeIssue: %v", err)
			}
			all = append(all, bead)
			rawOpen[issue.ID] = issue.Status == beadslib.StatusOpen
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
			candidates := all
			if nativeListPushesRawOpen(query) {
				candidates = nil
				for _, b := range all {
					if rawOpen[b.ID] {
						candidates = append(candidates, b)
					}
				}
			}
			want := ApplyListQuery(candidates, query)
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

// sr-i54yd: a seat whose only assigned beads are parked (blocked, deferred)
// holds no live work. The Live read every seat-work gate issues must not
// answer them as "open", or the seat never drains and never takes a config
// change; the cached (non-Live) read keeps gc's folded "open".
func TestNativeDoltStoreLiveOpenExcludesRawParkedStatuses(t *testing.T) {
	reader := filteringReader{rows: []*beadslib.Issue{
		{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: "task", Assignee: "worker"},
		{ID: "gc-blocked", Title: "blocked", Status: beadslib.StatusBlocked, IssueType: "task", Assignee: "worker"},
		{ID: "gc-deferred", Title: "deferred", Status: beadslib.StatusDeferred, IssueType: "task", Assignee: "worker"},
	}}
	store := newNativeDoltStoreForTest(&nativeDoltReaderSpy{list: reader.list})

	live, err := store.List(ListQuery{Assignee: "worker", Status: "open", TierMode: TierBoth, Live: true})
	if err != nil {
		t.Fatalf("Live List: %v", err)
	}
	if got := sortedBeadIDs(live); !slices.Equal(got, []string{"gc-open"}) {
		t.Fatalf("Live List(open) = %v, want only the raw-open row", got)
	}

	folded, err := store.List(ListQuery{Assignee: "worker", Status: "open", TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := sortedBeadIDs(folded)
	slices.Sort(got)
	if want := []string{"gc-blocked", "gc-deferred", "gc-open"}; !slices.Equal(got, want) {
		t.Fatalf("List(open) = %v, want gc's folded open %v", got, want)
	}
}

// countingNativeRows is a native reader over rows another process may change
// between calls: list answers as filteringReader does, and get counts the
// point reads a CachingStore above it pays.
type countingNativeRows struct {
	rows []*beadslib.Issue
	gets int
}

func (r *countingNativeRows) store() *NativeDoltStore {
	return newNativeDoltStoreForTest(&nativeDoltReaderSpy{list: r.list, get: r.get})
}

func (r *countingNativeRows) list(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	return filteringReader{rows: r.rows}.list(ctx, req)
}

func (r *countingNativeRows) get(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	r.gets++
	for _, issue := range r.rows {
		if issue.ID == req.ID {
			return &issueops.IssueDetails{Issue: *cloneNativeIssueForTest(issue)}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", issueops.ErrNotFound, req.ID)
}

func (r *countingNativeRows) setStatus(t *testing.T, id string, status beadslib.Status) {
	t.Helper()
	for _, issue := range r.rows {
		if issue.ID == id {
			issue.Status = status
			return
		}
	}
	t.Fatalf("no row %s", id)
}

func newParkedRowsForTest() *countingNativeRows {
	return &countingNativeRows{rows: []*beadslib.Issue{
		{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: "task"},
		{ID: "gc-blocked", Title: "blocked", Status: beadslib.StatusBlocked, IssueType: "task"},
		{ID: "gc-deferred", Title: "deferred", Status: beadslib.StatusDeferred, IssueType: "task"},
		{ID: "gc-hooked", Title: "hooked", Status: "hooked", IssueType: "task"},
	}}
}

// A CachingStore over a native store must not pay for the parked rows a Live
// "open" omits by design. The cache holds them folded to "open", so its
// stale-row sweep took each one for a row another process had changed and
// read it back, on every call: the read folded it to "open" again, so the
// sweep never converged (PR #7809 review).
func TestCachingStoreOverNativeLiveOpenSkipsParkedRows(t *testing.T) {
	primed := func(t *testing.T, extra ...*beadslib.Issue) (*countingNativeRows, *CachingStore) {
		t.Helper()
		reader := newParkedRowsForTest()
		reader.rows = append(reader.rows, extra...)
		cache := NewCachingStoreForTest(reader.store(), nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		reader.gets = 0
		return reader, cache
	}
	// live and cached list one gc status ("" for every nonclosed one), Live or
	// from the cache.
	live := func(t *testing.T, cache *CachingStore, status string) []string {
		t.Helper()
		items, err := cache.List(ListQuery{Status: status, TierMode: TierBoth, Live: true, AllowScan: true})
		if err != nil {
			t.Fatalf("Live List(%q): %v", status, err)
		}
		return sortedBeadIDs(items)
	}
	cached := func(t *testing.T, cache *CachingStore, status string) []string {
		t.Helper()
		items, err := cache.List(ListQuery{Status: status, TierMode: TierBoth, AllowScan: true})
		if err != nil {
			t.Fatalf("List(%q): %v", status, err)
		}
		return sortedBeadIDs(items)
	}

	t.Run("parked rows cost no read", func(t *testing.T) {
		reader, cache := primed(t)
		for call := 1; call <= 3; call++ {
			if got := live(t, cache, "open"); !slices.Equal(got, []string{"gc-open"}) {
				t.Fatalf("call %d: Live List(open) = %v, want only the raw-open row", call, got)
			}
			if reader.gets != 0 {
				t.Fatalf("call %d: %d backing Gets, want 0", call, reader.gets)
			}
		}
		if got, want := cached(t, cache, "open"), []string{"gc-blocked", "gc-deferred", "gc-hooked", "gc-open"}; !slices.Equal(got, want) {
			t.Fatalf("cached List(open) = %v, want gc's folded open %v", got, want)
		}
	})

	t.Run("a raw-open row another process closed is still caught", func(t *testing.T) {
		reader, cache := primed(t)
		reader.setStatus(t, "gc-open", beadslib.StatusClosed)
		if got := live(t, cache, "open"); len(got) != 0 {
			t.Fatalf("Live List(open) = %v, want none", got)
		}
		if reader.gets != 1 {
			t.Fatalf("%d backing Gets, want 1 for the row that left the page", reader.gets)
		}
		if got, want := cached(t, cache, "open"), []string{"gc-blocked", "gc-deferred", "gc-hooked"}; !slices.Equal(got, want) {
			t.Fatalf("cached List(open) = %v, want %v: the close was not installed", got, want)
		}
	})

	t.Run("a raw-open row another process parked costs one read", func(t *testing.T) {
		reader, cache := primed(t)
		reader.setStatus(t, "gc-open", beadslib.StatusBlocked)
		for call := 1; call <= 3; call++ {
			if got := live(t, cache, "open"); len(got) != 0 {
				t.Fatalf("call %d: Live List(open) = %v, want none", call, got)
			}
		}
		if reader.gets != 1 {
			t.Fatalf("%d backing Gets over 3 calls, want 1: the first read records the parked status", reader.gets)
		}
	})

	t.Run("a parked row another process closed is left to reconcile", func(t *testing.T) {
		reader, cache := primed(t)
		reader.setStatus(t, "gc-blocked", beadslib.StatusClosed)
		if got := live(t, cache, "open"); !slices.Equal(got, []string{"gc-open"}) {
			t.Fatalf("Live List(open) = %v, want only the raw-open row", got)
		}
		if reader.gets != 0 {
			t.Fatalf("%d backing Gets, want 0: the sweep skips a row last read parked", reader.gets)
		}
		cache.ReconcileNowForTest()
		if got, want := cached(t, cache, "open"), []string{"gc-deferred", "gc-hooked", "gc-open"}; !slices.Equal(got, want) {
			t.Fatalf("cached List(open) after reconcile = %v, want %v", got, want)
		}
	})

	t.Run("a parked row another process claimed is left to reconcile", func(t *testing.T) {
		reader, cache := primed(t)
		reader.setStatus(t, "gc-blocked", beadslib.StatusInProgress)
		if got := live(t, cache, "open"); !slices.Equal(got, []string{"gc-open"}) {
			t.Fatalf("Live List(open) = %v, want only the raw-open row", got)
		}
		if reader.gets != 0 {
			t.Fatalf("%d backing Gets, want 0: the sweep skips a row last read parked", reader.gets)
		}
		cache.ReconcileNowForTest()
		if got := cached(t, cache, "in_progress"); !slices.Equal(got, []string{"gc-blocked"}) {
			t.Fatalf("cached List(in_progress) after reconcile = %v, want the claimed row", got)
		}
	})

	// The sweep skips a row only when the Live read names a status and the row
	// was last read with another one. A row recorded with the named status, and
	// any row under a status-less Live read, is still read back when it leaves
	// the page.
	t.Run("a Live in_progress read still catches an in_progress row another process closed", func(t *testing.T) {
		reader, cache := primed(t, &beadslib.Issue{ID: "gc-claimed", Title: "claimed", Status: beadslib.StatusInProgress, IssueType: "task"})
		reader.setStatus(t, "gc-claimed", beadslib.StatusClosed)
		if got := live(t, cache, "in_progress"); len(got) != 0 {
			t.Fatalf("Live List(in_progress) = %v, want none", got)
		}
		if reader.gets != 1 {
			t.Fatalf("%d backing Gets, want 1 for the row that left the page", reader.gets)
		}
		if got := cached(t, cache, "in_progress"); len(got) != 0 {
			t.Fatalf("cached List(in_progress) = %v, want none: the close was not installed", got)
		}
	})

	t.Run("a status-less Live read still catches a row another process closed", func(t *testing.T) {
		reader, cache := primed(t)
		reader.setStatus(t, "gc-open", beadslib.StatusClosed)
		want := []string{"gc-blocked", "gc-deferred", "gc-hooked"}
		if got := live(t, cache, ""); !slices.Equal(got, want) {
			t.Fatalf("Live List() = %v, want %v", got, want)
		}
		if reader.gets != 1 {
			t.Fatalf("%d backing Gets, want 1 for the row that left the page", reader.gets)
		}
		if got := cached(t, cache, ""); !slices.Equal(got, want) {
			t.Fatalf("cached List() = %v, want %v: the close was not installed", got, want)
		}
	})

	// A non-Live read the cache cannot serve falls back to a Live one, which
	// is bd's raw open, so the answer depends on cache health (ga-4w9wpf).
	t.Run("a non-Live open read is folded warm and raw cold", func(t *testing.T) {
		reader := newParkedRowsForTest()
		cache := NewCachingStoreForTest(reader.store(), nil)
		if got := cached(t, cache, "open"); !slices.Equal(got, []string{"gc-open"}) {
			t.Fatalf("cold List(open) = %v, want bd's raw open from the Live fallback", got)
		}
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if got, want := cached(t, cache, "open"), []string{"gc-blocked", "gc-deferred", "gc-hooked", "gc-open"}; !slices.Equal(got, want) {
			t.Fatalf("warm List(open) = %v, want gc's folded open %v", got, want)
		}
	})
}

// An explicit transition supersedes the status a store recorded, as it does
// the indefinite deferral.
func TestSetBeadStatusClearsSourceStatus(t *testing.T) {
	t.Parallel()
	b := Bead{ID: "gc-1", Status: "open", SourceStatus: "blocked"}
	setBeadStatus(&b, "open")
	if b.SourceStatus != "" {
		t.Fatalf("SourceStatus after setBeadStatus(open) = %q, want empty", b.SourceStatus)
	}
}

// An event's status may be bd's raw one or gc's folded one, so merging it
// drops the recorded status until the next backing read; an event without a
// status keeps it.
func TestMergeCacheEventPatchSourceStatus(t *testing.T) {
	t.Parallel()
	base := Bead{ID: "gc-1", Title: "task", Status: "open", SourceStatus: "blocked"}
	for _, tt := range []struct{ name, payload, want string }{
		{"folded open", `{"id":"gc-1","status":"open"}`, ""},
		{"raw blocked", `{"id":"gc-1","status":"blocked"}`, ""},
		{"no status", `{"id":"gc-1","title":"renamed"}`, "blocked"},
	} {
		patch, fields := decodeCloseReasonPatch(t, json.RawMessage(tt.payload))
		if got := mergeCacheEventPatch(base, patch, fields).SourceStatus; got != tt.want {
			t.Errorf("%s: merged SourceStatus = %q, want %q", tt.name, got, tt.want)
		}
	}
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
