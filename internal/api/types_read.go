package api

import (
	"net/http"
	"strconv"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file hosts stable, CLI-facing read-path envelope types that are
// shared across per-domain migrations.
//
// Domain-specific CLI types (SessionSummary, MailHeader, RigView, ...)
// live alongside their decode_<domain>.go translator in this package so
// cmd/gc/ never imports internal/api/genclient directly. A new per-file
// migration typically adds:
//
//   - internal/api/decode_<domain>.go  — translators from genclient response
//                                        types to CLI-facing types, plus
//                                        small, focused unit tests.
//   - internal/api/client.go           — one typed read wrapper per endpoint
//                                        returning (<CLIType>, float64 age, error).
//   - cmd/gc/cmd_<domain>.go           — routes via apiClient(cityPath);
//                                        logs route=... on every exit path.
//
// The CacheAge float64 returned from each wrapper is the supervisor's
// CachingStore age in seconds at the time of the read, sourced from the
// X-GC-Cache-Age-S response header (populated by handlers via the
// cacheAgeSeconds helper). CLI callers surface this as _cache_age_s on
// the --json envelope and as a stale-read banner on human output when
// the age crosses 30 s.

// CachedRead is a convenience wrapper returned by read wrappers so the
// two cache-age-bearing return values stay co-located with the payload.
// Per-domain wrappers may return CachedRead[[]SessionSummary],
// CachedRead[MailHeader], and so on. A zero AgeSeconds means the server
// did not surface a cache age (non-caching store or fallback path).
type CachedRead[T any] struct {
	Body       T
	AgeSeconds float64
}

// cacheAgeHeader is the wire name of the X-GC-Cache-Age-S response header,
// set by read handlers via the cacheAgeSeconds helper and consumed by CLI
// wrappers through cacheAgeFromResponse.
const cacheAgeHeader = "X-GC-Cache-Age-S"

// StatusView is the CLI-facing shape for `gc status` and `gc rig status`.
// It mirrors the subset of StatusBody fields the CLI formatter reads so
// cmd/gc/ never imports genclient directly. The detail slices (Agents,
// Rigs, NamedSessions) are pre-filtered by the server to match what the
// fallback snapshot would collect. Controller authority is resolved
// locally by the CLI (controllerStatusForCity) because the server
// handling the request IS the controller — no wire field is needed.
type StatusView struct {
	CityName      string
	CityPath      string
	Version       string
	UptimeSec     int
	Suspended     bool
	Agents        []StatusAgentView
	Rigs          []StatusRigView
	NamedSessions []StatusNamedSessionView
	SessionCounts StatusSessionCountsView
	StoreHealth   *StatusStoreHealthView
	Beads         *beads.BeadsDiagnostic
	Summary       StatusSummaryView
}

// StatusAgentView is the CLI-facing per-agent row.
type StatusAgentView struct {
	Name          string
	QualifiedName string
	Scope         string
	Running       bool
	Suspended     bool
	SessionName   string
	GroupName     string
	ScaleLabel    string
	Expanded      bool
}

// StatusRigView is the CLI-facing per-rig row.
type StatusRigView struct {
	Name      string
	Path      string
	Suspended bool
}

// StatusNamedSessionView is the CLI-facing named-session row.
type StatusNamedSessionView struct {
	Identity string
	Status   string
	Mode     string
}

// StatusSessionCountsView mirrors the "Sessions: N active, M suspended"
// line appended to `gc status` text output.
type StatusSessionCountsView struct {
	Active    int
	Suspended int
}

// StatusStoreHealthView mirrors the CLI's StoreHealth struct. Values
// render into the store health block appended to text output; fallback
// callers read the same shape from the local event log.
type StatusStoreHealthView struct {
	Path         string
	SizeBytes    int64
	LiveRows     int
	RatioMB      float64
	Warning      bool
	ThresholdMB  float64
	LastGCAt     string
	LastGCStatus string
}

// StatusSummaryView captures the aggregate counts the renderer uses for the
// "N/M agents running" and "Sessions: ..." lines.
type StatusSummaryView struct {
	TotalAgents   int
	RunningAgents int
}

// cacheAgeFromResponse extracts the CachingStore age from the response's
// X-GC-Cache-Age-S header. Returns 0 when the response is nil, the header
// is absent, or the value fails to parse. The header value is a float64
// second count; fallback paths omit the header and naturally yield 0.
func cacheAgeFromResponse(r *http.Response) float64 {
	if r == nil {
		return 0
	}
	v := r.Header.Get(cacheAgeHeader)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}
