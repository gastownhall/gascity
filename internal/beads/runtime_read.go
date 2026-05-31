package beads

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ReadClass names the runtime contract for a bead read. Hot runtime classes
// must not fall back to hydrated Beads CLI reads when cache or indexed SQL is
// unavailable.
type ReadClass string

const (
	// ReadClassHotAuthoritative is for controller/session decisions that must
	// be correct before taking action but still cannot use hydrated CLI
	// fallback.
	ReadClassHotAuthoritative ReadClass = "hot-authoritative"
	// ReadClassHotDegradedOK is for runtime summaries and demand hints that
	// can return partial/degraded state.
	ReadClassHotDegradedOK ReadClass = "hot-degraded-ok"
	// ReadClassForegroundAuthoritative is for operator commands that may use
	// the Beads CLI fallback path.
	ReadClassForegroundAuthoritative ReadClass = "foreground-authoritative"
	// ReadClassMaintenance is for serialized non-hot maintenance work.
	ReadClassMaintenance ReadClass = "maintenance"
)

// Runtime read budgets. These are deliberately below the controller's default
// five-second tick budget and below the Dolt MySQL transport timeout.
const (
	// HotAuthoritativeBudget bounds a hot authoritative caller.
	HotAuthoritativeBudget = 3500 * time.Millisecond
	// HotDegradedOKBudget bounds a hot degraded-ok caller.
	HotDegradedOKBudget = 2 * time.Second
	// MaintenanceReadBudget is the default SQL health/read probe budget for
	// maintenance class reads.
	MaintenanceReadBudget = 30 * time.Second
)

// ReadPolicy declares how a runtime caller is allowed to read bead state.
type ReadPolicy struct {
	Class         ReadClass
	Caller        string
	Timeout       time.Duration
	MaxRows       int
	AllowFallback bool
}

// RuntimeReadPolicy returns a policy with the documented default budget and
// fallback behavior for class.
func RuntimeReadPolicy(class ReadClass, caller string) ReadPolicy {
	p := ReadPolicy{Class: class, Caller: caller}
	switch class {
	case ReadClassHotAuthoritative:
		p.Timeout = HotAuthoritativeBudget
		p.MaxRows = 500
	case ReadClassHotDegradedOK:
		p.Timeout = HotDegradedOKBudget
		p.MaxRows = 250
	case ReadClassMaintenance:
		p.Timeout = MaintenanceReadBudget
		p.AllowFallback = true
	default:
		p.Class = ReadClassForegroundAuthoritative
		p.AllowFallback = true
	}
	return p
}

// DegradedReadError reports a bounded hot-read degradation. Callers should
// defer unsafe actions or surface partial status instead of retrying through bd
// CLI fallback.
type DegradedReadError struct {
	Class     ReadClass
	Caller    string
	Operation string
	Route     string
	Coverage  string
	Err       error
}

func (e *DegradedReadError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := "degraded runtime read"
	if e.Caller != "" {
		msg += " caller=" + e.Caller
	}
	if e.Operation != "" {
		msg += " op=" + e.Operation
	}
	if e.Class != "" {
		msg += " class=" + string(e.Class)
	}
	if e.Route != "" {
		msg += " route=" + e.Route
	}
	if e.Coverage != "" {
		msg += " coverage=" + e.Coverage
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *DegradedReadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsDegradedRead reports whether err is a runtime degraded-read error.
func IsDegradedRead(err error) bool {
	var degraded *DegradedReadError
	return errors.As(err, &degraded)
}

type runtimeLister interface {
	RuntimeList(context.Context, ListQuery, ReadPolicy) ([]Bead, error)
}

type runtimeReadyer interface {
	RuntimeReady(context.Context, ReadyQuery, ReadPolicy) ([]Bead, error)
}

// RuntimeList executes a list query under policy. Foreground/maintenance
// policies preserve the existing authoritative store behavior. Hot policies use
// cache or indexed routes only and return DegradedReadError instead of falling
// through to hydrated bd list/query.
func RuntimeList(ctx context.Context, store Store, query ListQuery, policy ReadPolicy) ([]Bead, error) {
	if store == nil {
		return nil, degradedRead(policy, "list", "none", "", errors.New("nil bead store"))
	}
	policy = normalizeReadPolicy(policy)
	if policy.AllowFallback {
		return store.List(query)
	}
	if runtimeStore, ok := store.(runtimeLister); ok {
		return runtimeStore.RuntimeList(ctx, query, policy)
	}
	return store.List(query)
}

// RuntimeReady executes a ready-work lookup under policy. Hot policies use the
// cache read model when available; they never call bd ready.
func RuntimeReady(ctx context.Context, store Store, query ReadyQuery, policy ReadPolicy) ([]Bead, error) {
	if store == nil {
		return nil, degradedRead(policy, "ready", "none", "", errors.New("nil bead store"))
	}
	policy = normalizeReadPolicy(policy)
	if policy.AllowFallback {
		return readyWithQuery(store, query)
	}
	if runtimeStore, ok := store.(runtimeReadyer); ok {
		return runtimeStore.RuntimeReady(ctx, query, policy)
	}
	return readyWithQuery(store, query)
}

func readyWithQuery(store Store, query ReadyQuery) ([]Bead, error) {
	if query == (ReadyQuery{}) {
		return store.Ready()
	}
	return store.Ready(query)
}

func normalizeReadPolicy(policy ReadPolicy) ReadPolicy {
	if policy.Class == "" {
		policy = RuntimeReadPolicy(ReadClassHotDegradedOK, policy.Caller)
	}
	defaulted := RuntimeReadPolicy(policy.Class, policy.Caller)
	if policy.Timeout <= 0 {
		policy.Timeout = defaulted.Timeout
	}
	if policy.MaxRows <= 0 {
		policy.MaxRows = defaulted.MaxRows
	}
	policy.AllowFallback = policy.AllowFallback || defaulted.AllowFallback
	return policy
}

func contextWithReadPolicy(ctx context.Context, policy ReadPolicy) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= policy.Timeout {
			return context.WithCancel(ctx)
		}
	}
	if policy.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, policy.Timeout)
}

func degradedRead(policy ReadPolicy, op, route, coverage string, err error) error {
	if err == nil {
		err = errors.New("runtime read degraded")
	}
	return &DegradedReadError{
		Class:     policy.Class,
		Caller:    policy.Caller,
		Operation: op,
		Route:     route,
		Coverage:  coverage,
		Err:       err,
	}
}

func enforceRuntimeRowCap(items []Bead, policy ReadPolicy, op, route string) ([]Bead, error) {
	if policy.MaxRows <= 0 || len(items) < policy.MaxRows {
		return items, nil
	}
	return items, degradedRead(policy, op, route, "row-cap", fmt.Errorf("runtime read returned %d rows at cap %d", len(items), policy.MaxRows))
}
