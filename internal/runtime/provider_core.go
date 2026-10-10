package runtime

import (
	"errors"
	"fmt"
)

// PartialListError reports that ListRunning returned best-effort results while
// one or more backends failed. Callers may continue using the returned names
// slice, but should surface the degraded backend error to operators.
type PartialListError struct {
	Err error
	// ServerAbsent reports that the backend's runtime server was not running
	// at all. An absent server is still a failed observation, not proof that
	// zero sessions exist (see gastownhall/gascity#4082), so this never
	// relaxes the fail-safe on its own - it only lets callers holding
	// independent proof of death distinguish the two failure shapes.
	//
	// Never set on a merged multi-backend result: the absence of one backend
	// says nothing about its siblings.
	ServerAbsent bool
}

// Error returns the aggregated backend failure message.
func (e *PartialListError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// Unwrap exposes the aggregated backend failure.
func (e *PartialListError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// BackendError carries provider/backend context for aggregated failures.
type BackendError struct {
	Label string
	Err   error
}

// BackendListResult captures one backend's ListRunning result.
type BackendListResult struct {
	Label string
	Names []string
	Err   error
}

// IsRuntimeServerAbsent reports whether err is a [PartialListError] whose
// consulted backend was not running at all, as opposed to a server that was up
// and answered incompletely. Reap paths holding independent proof of death use
// it to act instead of deferring forever.
//
// It deliberately does NOT unwrap: only an error returned directly by a single
// backend can assert absence. A composite provider joins its backends' errors
// (and returns a bare join when every backend fails), so any traversing check
// would report absence whenever ANY backend was absent - including when a
// sibling backend is healthy, or merely failed for an unrelated reason, while
// still holding live sessions. Wrapping therefore degrades to "not absent",
// which is the fail-safe answer.
func IsRuntimeServerAbsent(err error) bool {
	target, ok := err.(*PartialListError) //nolint:errorlint // the non-traversing assertion is the point: see the doc comment above — errors.As would report absence whenever ANY joined backend was absent, including when a healthy sibling still holds live sessions
	return ok && target.ServerAbsent
}

// IsPartialListError reports whether err represents a degraded-but-usable
// ListRunning result from one or more failed backends.
func IsPartialListError(err error) bool {
	var target *PartialListError
	return errors.As(err, &target)
}

// DeadRuntimeSessionChecker is an optional provider capability for destructive
// cleanup paths that need positive proof a visible runtime artifact is dead.
// A false result means either the session is live, absent, or unsupported by
// the backend; a non-nil error means liveness could not be confirmed.
type DeadRuntimeSessionChecker interface {
	// IsDeadRuntimeSession reports whether name is visible but confirmed dead.
	IsDeadRuntimeSession(name string) (bool, error)
}

// MergeBackendListResults merges provider ListRunning results. On partial
// backend failure it returns the best-effort merged names plus a
// [PartialListError] so callers can continue with partial results while still
// surfacing backend degradation. Only a total failure returns no names.
func MergeBackendListResults(results ...BackendListResult) ([]string, error) {
	merged := make([]string, 0)
	failures := make([]error, 0, len(results))
	failed := 0

	for _, result := range results {
		merged = append(merged, result.Names...)
	}

	for _, result := range results {
		if result.Err == nil {
			continue
		}
		failed++
		failures = append(failures, fmt.Errorf("%s backend: %w", result.Label, result.Err))
	}

	if len(failures) == 0 {
		return merged, nil
	}
	if len(merged) > 0 {
		return merged, &PartialListError{Err: errors.Join(failures...)}
	}
	if failed == len(results) {
		return nil, errors.Join(failures...)
	}
	return merged, &PartialListError{Err: errors.Join(failures...)}
}

// ListingAttestation is an optional provider capability declaring that an
// error-free [Provider.ListRunning] result is complete: every running session
// matching the prefix is listed, so a name absent from it is not running.
// Providers that can silently omit a live session (a missed probe, a remote
// failure read as zero sessions, an unresolved binding) must not declare it.
// A composite attests only when every backend does.
type ListingAttestation interface {
	ListRunningComplete() bool
}

// ListRunningAttested reports whether an error-free ListRunning result from sp
// may be read as proof of absence. A provider that does not implement
// [ListingAttestation] is unattested.
func ListRunningAttested(sp Provider) bool {
	a, ok := sp.(ListingAttestation)
	return ok && a.ListRunningComplete()
}

// BackendListingProvider is an optional capability of composite providers
// that exposes each backend's ListRunning result separately, so callers can
// judge each backend's listing on its own (its error, its [PartialListError]
// ServerAbsent flag, its [ListingAttestation]). [MergeBackendListings] over
// the result is exactly what the composite's ListRunning returns.
//
// The listing does not recurse. A backend may itself be a composite (auto
// over hybrid); its entry then carries that composite's merged result, from
// one ListRunning call per leaf backend. A caller that wants the nested
// breakdown must not also call the nested ListRunningByBackend for the same
// observation: that lists the nested leaves a second time, at a different
// instant, and the two answers need not agree. It walks [BackendsProvider]
// instead and lists each leaf itself.
type BackendListingProvider interface {
	ListRunningByBackend(prefix string) []BackendListing
}

// BackendListing is one backend's ListRunning result inside a composite
// provider. Provider is the backend itself, so callers can recurse into a
// nested composite or ask the backend for its own optional capabilities.
type BackendListing struct {
	Label    string
	Provider Provider
	Names    []string
	Err      error
}

// BackendsProvider is an optional capability of composite providers that
// names their backends without listing them, in the order
// [BackendListingProvider.ListRunningByBackend] lists them. It lets a caller
// walk nested composites and list every leaf backend exactly once, which
// ListRunningByBackend alone cannot: a nested composite's entry there is
// already that composite's merged listing.
type BackendsProvider interface {
	Backends() []Backend
}

// Backend is one labeled backend of a composite provider.
type Backend struct {
	Label    string
	Provider Provider
}

// ListBackends calls ListRunning once on each backend, in order. Composites
// implement ListRunningByBackend with it, so the per-backend listing agrees
// with [BackendsProvider.Backends] by construction.
func ListBackends(backends []Backend, prefix string) []BackendListing {
	listings := make([]BackendListing, 0, len(backends))
	for _, b := range backends {
		names, err := b.Provider.ListRunning(prefix)
		listings = append(listings, BackendListing{Label: b.Label, Provider: b.Provider, Names: names, Err: err})
	}
	return listings
}

// MergeBackendListings merges per-backend listings exactly as
// [MergeBackendListResults] merges the same labels, names and errors.
func MergeBackendListings(listings []BackendListing) ([]string, error) {
	results := make([]BackendListResult, 0, len(listings))
	for _, l := range listings {
		results = append(results, BackendListResult{Label: l.Label, Names: l.Names, Err: l.Err})
	}
	return MergeBackendListResults(results...)
}

// MergeBackendStopErrors standardizes multi-backend Stop semantics.
// Any successful stop wins. If every backend reports the session as gone,
// Stop remains idempotent and returns nil.
func MergeBackendStopErrors(results ...BackendError) error {
	failures := make([]error, 0, len(results))
	allGone := len(results) > 0

	for _, result := range results {
		if result.Err == nil {
			return nil
		}
		if !IsSessionGone(result.Err) {
			allGone = false
		}
		failures = append(failures, fmt.Errorf("%s backend: %w", result.Label, result.Err))
	}

	if len(failures) == 0 || allGone {
		return nil
	}
	return errors.Join(failures...)
}
