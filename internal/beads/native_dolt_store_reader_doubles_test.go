package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The reader role over the in-package doubles' raw hooks.
//
// The store's three issue reads moved onto issueops.Reader because every other
// shape of SearchIssues is off the v0 served surface. The doubles' fixtures
// describe BEHAVIOR — these rows exist, this read fails, this context has a
// deadline — so the adapters below reproduce the ROLE over the same
// searchIssues/getReadyWork hooks rather than restating a hundred facts as
// reader fixtures.
//
// Reproducing the role means reproducing what the role DECIDES, not just where
// it dispatches. Three of those decisions are the reason this adapter is a
// faithful oracle rather than a pass-through, and each one is a defect the
// store would otherwise ship undetected:
//
//   - THE LIMIT DEFAULT. A nil ListRequest.Limit is fifty rows and a nil
//     ReadyRequest.Limit is a hundred (workapi.DefaultListLimit /
//     DefaultReadyLimit), never "unlimited". A store that forgot to state the
//     limit would truncate every unbounded read.
//   - THE DEFAULT SUPPRESSIONS. workapi.BuildListFilter hides templates, gates,
//     the configured infra types (agent, role and message by default) and the
//     whole ephemeral plane unless the request lifts each one, and hides
//     closed/pinned rows unless it drops the status exclusions. The raw search
//     the store used to call hid none of them. THIS ORACLE REPRODUCES ALL OF
//     THEM BUT THE TEMPLATE ONE: suppressedListTypeForTest below drops gate and
//     infra-typed rows and the plane bit drops wisps, but nothing here consults
//     is_template, so a request that forgot IncludeTemplates would still see a
//     template row through this double. The store states IncludeTemplates and
//     nativeListReadRequest's shape assertion is what holds it there; a
//     behavioral kill for that one member is a real-backend test, not this one.
//   - READY IS OPEN WORK. workapi.BuildReadyFilter stamps StatusOpen for every
//     caller and the wire publishes no status parameter, so a deferred row is
//     not reachable through this door at all — and the backends run a lazy
//     defer-wake sweep before the read that flips an EXPIRED time-bound
//     deferral to open first (internal/storage/{dolt,embeddeddolt}/queries.go,
//     issueops.WakeExpiredDefersInTx). Both halves are modeled, because the
//     visibility rule gc depends on is the difference between them.

// rawReadStorage is the slice of beadslib.Storage the reader adapter needs.
type rawReadStorage interface {
	GetIssue(context.Context, string) (*beadslib.Issue, error)
	SearchIssues(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error)
	GetReadyWork(context.Context, beadslib.WorkFilter) ([]*beadslib.Issue, error)
}

type rawIssueReader struct{ raw rawReadStorage }

// List reproduces workapi.BuildListFilter's disposition of the request over the
// double's search hook, so a request that failed to lift a suppression loses
// the same rows here that it would lose against a real backend.
func (r rawIssueReader) List(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	filter := beadslib.IssueFilter{
		Limit:               workapiListLimitForTest(req),
		SortBy:              req.SortBy,
		SortDesc:            req.Reverse,
		MetadataFields:      req.MetadataFields,
		CreatedBefore:       req.CreatedBefore,
		CreatedAfter:        req.CreatedAfter,
		IncludeDependencies: true,
	}
	if req.Status != "" {
		status := beadslib.Status(req.Status)
		filter.Status = &status
	} else if !req.AllFlag {
		filter.ExcludeStatus = []beadslib.Status{beadslib.StatusClosed, beadslib.Status("pinned")}
	}
	if req.IssueType != "" {
		issueType := beadslib.IssueType(req.IssueType)
		filter.IssueType = &issueType
	}
	if len(req.Labels) > 0 {
		filter.Labels = req.Labels
	}
	if req.Assignee != "" {
		filter.Assignee = &req.Assignee
	}
	if req.ParentID != "" {
		filter.ParentID = &req.ParentID
	}
	if !req.IncludeEphemeral && !req.IncludeInfra && !req.IncludeAllTypes {
		// The plane bit: a default listing does not read the wisps table at all.
		filter.SkipWisps = true
	}

	var idSet map[string]bool
	if req.IDFilter != "" {
		filter.IDs = strings.Split(req.IDFilter, ",")
		idSet = make(map[string]bool, len(filter.IDs))
		for _, id := range filter.IDs {
			idSet[id] = true
		}
	}

	issues, err := r.raw.SearchIssues(ctx, "", filter)
	if err != nil {
		return issueops.IssuePage{}, err
	}
	page := make([]*issueops.IssueWithCounts, 0, len(issues))
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		// Some doubles' search hooks ignore the filter, so the id set is
		// applied here too, the way the role applies it.
		if idSet != nil && !idSet[issue.ID] {
			continue
		}
		if suppressedListTypeForTest(req, issue) {
			continue
		}
		if filter.SkipWisps && (issue.Ephemeral || issue.NoHistory) {
			continue
		}
		row := &issueops.IssueWithCounts{Issue: issue}
		for _, dep := range issue.Dependencies {
			if dep != nil && dep.Type == beadslib.DepParentChild {
				parent := dep.DependsOnID
				row.Parent = &parent
				break
			}
		}
		page = append(page, row)
	}
	if filter.Limit > 0 && len(page) > filter.Limit {
		return issueops.IssuePage{Items: page[:filter.Limit], HasMore: true}, nil
	}
	return issueops.IssuePage{Items: page}, nil
}

// Ready reproduces workapi.BuildReadyFilter: open work only, with the lazy
// defer-wake sweep the backends run ahead of the read.
func (r rawIssueReader) Ready(ctx context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
	filter := beadslib.WorkFilter{
		Status:           beadslib.StatusOpen,
		Type:             req.IssueType,
		Limit:            workapiReadyLimitForTest(req),
		Unassigned:       req.Unassigned,
		Labels:           req.Labels,
		IncludeDeferred:  req.IncludeDeferred,
		IncludeEphemeral: req.IncludeEphemeral,
		HasMetadataKey:   req.HasMetadataKey,
	}
	if req.Assignee != "" && !req.Unassigned {
		assignee := req.Assignee
		filter.Assignee = &assignee
	}
	if req.ParentID != "" {
		parent := req.ParentID
		filter.ParentID = &parent
	}
	issues, err := r.raw.GetReadyWork(ctx, filter)
	if err != nil {
		return issueops.IssuePage{}, err
	}
	page := make([]*issueops.IssueWithCounts, 0, len(issues))
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		page = append(page, &issueops.IssueWithCounts{Issue: issue})
	}
	if filter.Limit > 0 && len(page) > filter.Limit {
		return issueops.IssuePage{Items: page[:filter.Limit], HasMore: true}, nil
	}
	return issueops.IssuePage{Items: page}, nil
}

// Get answers the detail view from whichever hook the double set: the exact-ids
// search shape when it has one (which is what the store's own Get used before
// the re-point), otherwise the plain row read.
//
// The Revision it publishes is the row's RowVersion, which is what
// types.NewIssueDetails projects on a LOCAL backend. The wire's own projection —
// the token arriving in `revision` with the embedded RowVersion left at zero —
// is a separate case, pinned directly in the read-role tests.
func (r rawIssueReader) Get(ctx context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	issue, err := r.getIssue(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("%w: %s", issueops.ErrNotFound, req.ID)
	}
	details := &issueops.IssueDetails{Issue: *issue, Labels: issue.Labels, Revision: strconv.FormatInt(issue.RowVersion, 10)}
	for _, dep := range issue.Dependencies {
		if dep == nil {
			continue
		}
		details.Dependencies = append(details.Dependencies, &beadslib.IssueWithDependencyMetadata{
			Issue:          beadslib.Issue{ID: dep.DependsOnID},
			DependencyType: dep.Type,
		})
	}
	return details, nil
}

// getIssue reads the row the detail view is built from.
//
// The ROW read comes first, and the order is the whole correctness of this
// adapter's miss. A double that owns real rows — the MemStore-backed one, and
// everything embedding it — answers a genuine absence here, which is what the
// role must report; falling through to a search hook that ignores its id filter
// would hand back somebody else's bead. A spy that set only a search fixture
// answers (nil, nil) and falls through, so its fixture still describes the row.
func (r rawIssueReader) getIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	issue, err := r.raw.GetIssue(ctx, id)
	switch {
	case err != nil && !errors.Is(err, ErrNotFound):
		return nil, err
	case err != nil:
		return nil, nil // the row read is authoritative about its own miss
	case issue != nil:
		return issue, nil
	}
	issues, err := r.raw.SearchIssues(ctx, id, beadslib.IssueFilter{IDs: []string{id}, IncludeDependencies: true})
	if err != nil {
		return nil, err
	}
	for _, found := range issues {
		if found != nil && found.ID == id {
			return found, nil
		}
	}
	if len(issues) > 0 {
		// A search fixture that answers unconditionally is describing "this row
		// exists"; honor that rather than reporting a miss it did not mean.
		return issues[0], nil
	}
	return nil, nil
}

// suppressedListTypeForTest is workapi.applyTypeSuppressions' TYPE half: the
// gate exclusion and the configured infra set, each lifted by its own flag.
func suppressedListTypeForTest(req issueops.ListRequest, issue *beadslib.Issue) bool {
	if req.IncludeAllTypes {
		return false
	}
	switch string(issue.IssueType) {
	case "gate":
		return !req.IncludeGates && req.IssueType != "gate"
	case "agent", "role", "message":
		return !req.IncludeInfra
	}
	return false
}

func workapiListLimitForTest(req issueops.ListRequest) int {
	if req.Limit == nil {
		return 50 // workapi.DefaultListLimit
	}
	return *req.Limit
}

func workapiReadyLimitForTest(req issueops.ReadyRequest) int {
	if req.Limit == nil {
		return 100 // workapi.DefaultReadyLimit
	}
	return *req.Limit
}

// readyWorkFixtureForTest answers a WorkFilter from a fixed row set the way a
// backend does: the lazy defer-wake sweep first, then the ready predicate.
// Shared by the package's tests because the deferred-visibility rule is the one
// ready semantic that only shows up when both halves run.
func readyWorkFixtureForTest(issues []*beadslib.Issue, filter beadslib.WorkFilter) []*beadslib.Issue {
	return readyWorkFixtureWithDeferredChildrenForTest(issues, nil, filter)
}

// readyWorkFixtureWithDeferredChildrenForTest adds the input the ready WHERE
// clause folds in beside the row predicate: the ids of children of
// future-deferred parents (sqlbuild.ReadyWorkWhereInputs.DeferredChildIDs),
// which the query excludes ONLY while IncludeDeferred is off. It is a separate
// entry point because computing that set takes queries a fixture has no graph
// for — which is exactly the shape of the real thing, where the execution
// context computes it and hands it to the builder.
func readyWorkFixtureWithDeferredChildrenForTest(issues []*beadslib.Issue, deferredChildren []string, filter beadslib.WorkFilter) []*beadslib.Issue {
	now := time.Now().UTC()
	var out []*beadslib.Issue
	for _, issue := range issues {
		row := cloneNativeIssueForTest(issue)
		if !filter.IncludeDeferred && slices.Contains(deferredChildren, row.ID) {
			continue
		}
		// issueops.WakeExpiredDefersInTx: status='deferred' AND defer_until IS
		// NOT NULL AND defer_until <= now becomes open, in its own write
		// transaction, before the ready read runs.
		if row.Status == beadslib.StatusDeferred && row.DeferUntil != nil && !row.DeferUntil.After(now) {
			row.Status = beadslib.StatusOpen
		}
		if filter.Status != "" && row.Status != filter.Status {
			continue
		}
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, row.Status) {
			continue
		}
		if !filter.IncludeEphemeral && row.Ephemeral {
			continue
		}
		if !filter.IncludeDeferred && row.DeferUntil != nil && row.DeferUntil.After(now) {
			continue
		}
		if filter.Assignee != nil && row.Assignee != *filter.Assignee {
			continue
		}
		out = append(out, row)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out
}

func (s *nativeDoltStorageSpy) IssueReader() (issueops.Reader, error) {
	return rawIssueReader{raw: s}, nil
}

func (s *nativeDoltMemStorage) IssueReader() (issueops.Reader, error) {
	return rawIssueReader{raw: s}, nil
}

// The doubles that OVERRIDE a raw read get their own accessor, for the reason
// embedding exists to make awkward and that rawBatchApplier already ran into:
// the adapter resolves its hooks from the value it was handed, and one built
// from the embedded mem storage would call the inner GetReadyWork rather than
// the override — turning an is_blocked test into a test of nothing.

func (s *nativeBlockedColumnStorage) IssueReader() (issueops.Reader, error) {
	return rawIssueReader{raw: s}, nil
}

func (s *nativeProjectionlessStorage) IssueReader() (issueops.Reader, error) {
	return rawIssueReader{raw: s}, nil
}

var (
	_ rawReadStorage = (*nativeDoltStorageSpy)(nil)
	_ rawReadStorage = (*nativeDoltMemStorage)(nil)
)

// nativeDoltReaderSpy is the double for the re-point's OWN tests: it holds the
// role directly, so a test can assert what the store ASKED for rather than what
// the adapter above translated it into.
type nativeDoltReaderSpy struct {
	beadslib.Storage
	list  func(context.Context, issueops.ListRequest) (issueops.IssuePage, error)
	ready func(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error)
	get   func(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error)
}

func (s *nativeDoltReaderSpy) IssueReader() (issueops.Reader, error) { return s, nil }

func (s *nativeDoltReaderSpy) List(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	if s.list == nil {
		return issueops.IssuePage{}, nil
	}
	return s.list(ctx, req)
}

func (s *nativeDoltReaderSpy) Ready(ctx context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
	if s.ready == nil {
		return issueops.IssuePage{}, nil
	}
	return s.ready(ctx, req)
}

func (s *nativeDoltReaderSpy) Get(ctx context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	if s.get == nil {
		return nil, fmt.Errorf("%w: %s", issueops.ErrNotFound, req.ID)
	}
	return s.get(ctx, req)
}

var _ issueops.Reader = (*nativeDoltReaderSpy)(nil)
