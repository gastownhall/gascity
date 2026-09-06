// Package sessionoutcomes answers the recurring "is session start
// healthy" operator question: session-start outcomes broken out by
// template and provider over time, from worker.operation events.
// Introduced by issue #5852 as the third of four `gc analyze`
// subcommands reading events.jsonl.
//
// The motivating scenario is a provider outage: every session-start
// attempt fails, and the failures cluster tightly in duration_ms because
// they all fail at the same stage (e.g. an auth handshake) rather than
// timing out independently. That signature — a bucket where every
// attempt failed and duration_ms barely varies — is exactly what an
// eyeballed grep of worker.operation events would look for; Analyze
// computes it directly as Group.PossibleOutage.
//
// The package is a pure-data layer: it parses events.Event slices into
// a grouped, time-bucketed report. The CLI (cmd/gc/cmd_analyze_sessions.go)
// handles IO, filtering, and presentation — the same split reliability
// (internal/reliability) and beadthroughput (internal/beadthroughput)
// established.
package sessionoutcomes

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// unknownDim is the group label for a missing template or provider —
// best-effort fields on WorkerOperationEventPayload (see its doc comment
// in internal/api/event_payloads.go): absence means "not observed", not
// "no template"/"no provider". Kept visually distinct from a real empty
// string is not possible over the wire (both decode to ""), so this
// package reports the wire value as-is and groups empty strings under
// this label rather than a blank bucket that a table would render as
// nothing.
const unknownDim = "unknown"

// startOperations are the worker.operation "operation" values that
// represent a session-start attempt. Both exist in production: "start"
// is the direct path (SessionHandle.Start); "start_resolved" is the
// migration-bridge path for callers that already resolved
// provider-specific runtime config (SessionHandle.StartResolved). Both
// terminate the same way (event.finish(err)) and carry the same payload
// shape, so both count as session-start attempts here.
var startOperations = map[string]struct{}{
	"start":          {},
	"start_resolved": {},
}

// Window restricts the events considered to a time range. Zero-valued
// fields disable the corresponding bound. Mirrors reliability.Window /
// beadthroughput.Window; kept as a separate type so this package has no
// dependency on either.
type Window struct {
	Since time.Time
	Until time.Time
}

// Contains reports whether ts is within the window. A zero-valued bound
// disables that side of the check.
func (w Window) Contains(ts time.Time) bool {
	if !w.Since.IsZero() && ts.Before(w.Since) {
		return false
	}
	if !w.Until.IsZero() && ts.After(w.Until) {
		return false
	}
	return true
}

// Filter narrows the grouped report to a specific template and/or
// provider. Empty fields disable the corresponding filter. Matching is
// case-insensitive, consistent with reliability's Model/Rig filters.
type Filter struct {
	Template string
	Provider string
}

func (f Filter) matches(key GroupKey) bool {
	if f.Template != "" && !strings.EqualFold(f.Template, key.Template) {
		return false
	}
	if f.Provider != "" && !strings.EqualFold(f.Provider, key.Provider) {
		return false
	}
	return true
}

// GroupKey is the (template, provider, bucket) tuple the report groups
// by. BucketStart is the start of the fixed-width time bucket the
// attempt's started_at falls into (see Analyze's bucket parameter) — the
// "over time" dimension issue #5852 asked for, so a provider outage
// shows up as one or more buckets with a bad signature rather than being
// averaged away across the whole window.
type GroupKey struct {
	Template    string    `json:"template"`
	Provider    string    `json:"provider"`
	BucketStart time.Time `json:"bucket_start"`
}

// Group reports session-start outcome counts for one (template,
// provider, bucket) bucket, plus the duration statistics needed to spot
// a tight-cluster-of-failures signature.
type Group struct {
	Key       GroupKey `json:"key"`
	Started   int      `json:"started"`
	Succeeded int      `json:"succeeded"`
	Failed    int      `json:"failed"`
	// Other counts attempts whose result is neither "succeeded" nor
	// "failed" — any future result value the CLI's caller hasn't
	// accounted for. Kept separate so Started == Succeeded+Failed+Other
	// always holds and a new result value can't silently vanish.
	Other int `json:"other"`

	// MinDurationMs and MaxDurationMs are the observed duration_ms
	// range across the group's attempts. Both are 0 when Started is
	// zero.
	MinDurationMs int64 `json:"min_duration_ms"`
	MaxDurationMs int64 `json:"max_duration_ms"`

	sumDurationMs int64
}

// FailureRate returns Failed / Started, or 0 if Started is zero.
// Returned as a fraction (0.05 = 5%).
func (g Group) FailureRate() float64 {
	if g.Started == 0 {
		return 0
	}
	return float64(g.Failed) / float64(g.Started)
}

// AvgDurationMs returns the mean duration_ms across the group's
// attempts, or 0 if Started is zero. Serialized alongside the raw
// counts so JSON consumers don't have to recompute it from sums this
// type doesn't otherwise expose.
func (g Group) AvgDurationMs() float64 {
	if g.Started == 0 {
		return 0
	}
	return float64(g.sumDurationMs) / float64(g.Started)
}

// MarshalJSON emits Group's fields plus the AvgDurationMs() method
// result, so JSON consumers get the mean without recomputing it from
// Started/duration sums this type doesn't otherwise expose.
func (g Group) MarshalJSON() ([]byte, error) {
	type alias Group
	return json.Marshal(struct {
		alias
		AvgDurationMs  float64 `json:"avg_duration_ms"`
		PossibleOutage bool    `json:"possible_outage"`
	}{alias: alias(g), AvgDurationMs: g.AvgDurationMs(), PossibleOutage: g.PossibleOutage()})
}

// outageMinAttempts is the minimum sample size before a
// fully-failed bucket is called a possible outage rather than noise
// from a single flaky attempt.
const outageMinAttempts = 3

// outageMaxSpreadMs is the maximum duration_ms spread (max-min) within
// a fully-failed bucket that still counts as "tight" — attempts failing
// independently (timeouts, retries) spread out; attempts failing at the
// same stage of the same broken dependency (e.g. an auth handshake to a
// downed provider) do not.
const outageMaxSpreadMs = 500

// PossibleOutage reports whether this group matches the "provider
// outage" signature from issue #5852: every attempt in the bucket
// failed, there are enough attempts for the pattern to be meaningful,
// and duration_ms is clustered tight rather than spread out.
func (g Group) PossibleOutage() bool {
	if g.Started < outageMinAttempts || g.Failed != g.Started {
		return false
	}
	return g.MaxDurationMs-g.MinDurationMs <= outageMaxSpreadMs
}

// Instrumentation summarizes whether the source event stream can
// support the requested session-outcome dimensions.
type Instrumentation struct {
	StartAttempts   int `json:"start_attempts"`
	MissingTemplate int `json:"missing_template"`
	MissingProvider int `json:"missing_provider"`
}

// Report is the top-level result of an analysis pass.
type Report struct {
	Window          Window          `json:"-"`
	Filter          Filter          `json:"-"`
	Bucket          time.Duration   `json:"-"`
	Groups          []Group         `json:"groups"`
	Total           Group           `json:"total"`
	Skipped         int             `json:"skipped"` // worker.operation events that didn't decode
	Instrumentation Instrumentation `json:"instrumentation"`
}

// workerOperationPayload is the minimal structural subset of
// api.WorkerOperationEventPayload this package consumes. Decoupled from
// the api package to avoid a downstream import, the same choice
// reliability.workerOperationPayload made.
type workerOperationPayload struct {
	Operation  string `json:"operation"`
	Result     string `json:"result"`
	Provider   string `json:"provider"`
	Template   string `json:"template"`
	DurationMs int64  `json:"duration_ms"`
}

// Analyze produces a session-outcomes report from the supplied events.
//
// Only worker.operation events whose "operation" field is a session-start
// attempt (see startOperations) are considered. Events outside the
// window are dropped silently. Events that fail to decode, or whose
// payload is not a session-start operation, are dropped; a decode
// failure on an otherwise-eligible worker.operation event counts toward
// Report.Skipped so a malformed payload is visible rather than silently
// absorbed. bucket must be positive; Analyze bucketizes on
// started_at truncated to the bucket boundary (UTC).
func Analyze(es []events.Event, win Window, bucket time.Duration, flt Filter) Report {
	if bucket <= 0 {
		bucket = time.Hour
	}
	groups := make(map[GroupKey]*Group)
	report := Report{Window: win, Filter: flt, Bucket: bucket}

	groupFor := func(key GroupKey) *Group {
		g, ok := groups[key]
		if !ok {
			g = &Group{Key: key}
			groups[key] = g
		}
		return g
	}

	total := &Group{}

	for _, e := range es {
		if e.Type != events.WorkerOperation {
			continue
		}
		if !win.Contains(e.Ts) {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p workerOperationPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			report.Skipped++
			continue
		}
		if _, ok := startOperations[p.Operation]; !ok {
			continue
		}

		report.Instrumentation.StartAttempts++
		if strings.TrimSpace(p.Template) == "" {
			report.Instrumentation.MissingTemplate++
		}
		if strings.TrimSpace(p.Provider) == "" {
			report.Instrumentation.MissingProvider++
		}

		template := orUnknown(p.Template)
		provider := orUnknown(p.Provider)
		key := GroupKey{Template: template, Provider: provider, BucketStart: e.Ts.UTC().Truncate(bucket)}
		if !flt.matches(key) {
			continue
		}
		g := groupFor(key)
		applyAttempt(g, p)
		applyAttempt(total, p)
	}

	report.Groups = sortedGroups(groups)
	report.Total = *total
	return report
}

// applyAttempt folds one session-start attempt's outcome and duration
// into g.
func applyAttempt(g *Group, p workerOperationPayload) {
	g.Started++
	switch p.Result {
	case "succeeded":
		g.Succeeded++
	case "failed":
		g.Failed++
	default:
		g.Other++
	}
	g.sumDurationMs += p.DurationMs
	if g.Started == 1 {
		g.MinDurationMs = p.DurationMs
		g.MaxDurationMs = p.DurationMs
		return
	}
	if p.DurationMs < g.MinDurationMs {
		g.MinDurationMs = p.DurationMs
	}
	if p.DurationMs > g.MaxDurationMs {
		g.MaxDurationMs = p.DurationMs
	}
}

// orUnknown returns unknownDim when s is empty (after trimming),
// otherwise s.
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return unknownDim
	}
	return s
}

// sortedGroups returns the report groups sorted deterministically:
// ascending bucket start (chronological, matching the "over time" ask),
// then ascending template/provider for stable reading within a bucket.
func sortedGroups(groups map[GroupKey]*Group) []Group {
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Key.BucketStart.Equal(out[j].Key.BucketStart) {
			return out[i].Key.BucketStart.Before(out[j].Key.BucketStart)
		}
		if out[i].Key.Template != out[j].Key.Template {
			return out[i].Key.Template < out[j].Key.Template
		}
		return out[i].Key.Provider < out[j].Key.Provider
	})
	return out
}
