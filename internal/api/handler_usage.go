package api

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/usage"
)

const (
	usageRecentWindow = 5 * time.Minute
	usageReadMaxBytes = 16 << 20
	usageBySessionCap = 24
	usageCacheMaxAge  = 10 * time.Second
)

// UsageInput is the Huma input for GET /v0/city/{cityName}/usage.
type UsageInput struct {
	CityScope
}

// UsageTotals aggregates usage facts over one time window.
type UsageTotals struct {
	Invocations         int     `json:"invocations" doc:"Model facts (LLM invocations) in the window."`
	ComputeFacts        int     `json:"compute_facts" doc:"Compute (wall-clock) facts in the window."`
	InputTokens         int     `json:"input_tokens" doc:"Prompt tokens."`
	OutputTokens        int     `json:"output_tokens" doc:"Completion tokens."`
	CacheReadTokens     int     `json:"cache_read_tokens" doc:"Prompt-cache read tokens."`
	CacheCreationTokens int     `json:"cache_creation_tokens" doc:"Prompt-cache creation tokens."`
	WallSeconds         float64 `json:"wall_seconds" doc:"Compute wall-clock seconds."`
	CostUSDEstimate     float64 `json:"cost_usd_estimate" doc:"List-price estimate; decision-support only, never an authoritative charge."`
	Unpriced            int     `json:"unpriced" doc:"Facts with unknown pricing; their cost is not included in the estimate."`
}

// UsageSessionRecent is one session's model usage inside the recent window.
type UsageSessionRecent struct {
	Session             string  `json:"session" doc:"Session (worker) name the facts were attributed to."`
	SessionID           string  `json:"session_id,omitempty" doc:"Session bead id, when attributed."`
	InputTokens         int     `json:"input_tokens" doc:"Prompt tokens in the window."`
	OutputTokens        int     `json:"output_tokens" doc:"Completion tokens in the window."`
	CacheReadTokens     int     `json:"cache_read_tokens" doc:"Prompt-cache read tokens in the window."`
	CacheCreationTokens int     `json:"cache_creation_tokens" doc:"Prompt-cache creation tokens in the window."`
	CostUSDEstimate     float64 `json:"cost_usd_estimate" doc:"List-price estimate for the window."`
	Unpriced            int     `json:"unpriced" doc:"Facts in this window whose price is unknown."`
}

// UsageSource identifies whether the response reflects the local estimate log.
type UsageSource string

const (
	// UsageSourceLocalEstimate reports facts read from the local estimate log.
	UsageSourceLocalEstimate UsageSource = "local_estimate"
	// UsageSourceUnavailable reports that this supervisor has no local recorder.
	UsageSourceUnavailable UsageSource = "unavailable"
)

// UsageBody is the bounded city telemetry returned by GET /usage. Today and
// recent are exact when Partial is false and lower-bound observations when it
// is true.
type UsageBody struct {
	Available        bool                 `json:"available" doc:"True when this city is configured to record local usage estimates."`
	Recording        bool                 `json:"recording" doc:"True when new facts are currently being written to the local estimate log."`
	Source           UsageSource          `json:"source" enum:"local_estimate,unavailable" doc:"Source of this usage reading."`
	Today            UsageTotals          `json:"today" doc:"Usage since local midnight on the supervisor host."`
	Recent           UsageTotals          `json:"recent" doc:"Usage in the trailing recent window."`
	RecentBySession  []UsageSessionRecent `json:"recent_by_session,omitempty" doc:"Recent model usage per session, largest token volume first."`
	RecentWindowSecs int                  `json:"recent_window_secs" doc:"Length of the recent window in seconds."`
	ObservedFrom     string               `json:"observed_from,omitempty" doc:"RFC3339 timestamp of the oldest fact included in this bounded read."`
	UpdatedAt        string               `json:"updated_at" doc:"RFC3339 time at which the aggregate was built."`
	Partial          bool                 `json:"partial,omitempty" doc:"True when the bounded reader skipped history or malformed records."`
	PartialReasons   []string             `json:"partial_reasons,omitempty" doc:"Path-sanitized reasons the aggregate may be incomplete."`
}

// UsageOutput is the Huma output envelope for GET /v0/city/{cityName}/usage.
type UsageOutput struct {
	Body UsageBody
}

func (s *Server) humaHandleUsage(_ context.Context, _ *UsageInput) (*UsageOutput, error) {
	if !usage.IsLocalSink(s.state.UsageSink()) {
		return &UsageOutput{Body: UsageBody{
			Source:           UsageSourceUnavailable,
			UpdatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
			RecentWindowSecs: int(usageRecentWindow / time.Second),
		}}, nil
	}
	if body, ok := cachedResponseWithinAgeAs[UsageBody](s, "usage", usageCacheMaxAge); ok {
		return &UsageOutput{Body: body}, nil
	}
	path := filepath.Join(s.state.CityPath(), ".gc", "usage.jsonl")
	facts, report, err := usage.ReadRecentFacts(path, usageReadMaxBytes)
	if err != nil {
		slog.Error("usage telemetry read failed", "error", err)
		return nil, apierr.ServiceUnavailable.Msg("usage telemetry is unavailable")
	}
	body := buildUsageBody(facts, report, time.Now())
	s.storeResponse("usage", 0, body)
	return &UsageOutput{Body: body}, nil
}

func buildUsageBody(facts []usage.Fact, report usage.RecentReadReport, now time.Time) UsageBody {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	recentFrom := now.Add(-usageRecentWindow)
	body := UsageBody{
		Available:        true,
		Recording:        true,
		Source:           UsageSourceLocalEstimate,
		RecentWindowSecs: int(usageRecentWindow / time.Second),
		UpdatedAt:        now.UTC().Format(time.RFC3339Nano),
		Partial:          report.Truncated || report.RecordLimited || report.Malformed > 0 || report.Oversized > 0,
	}
	if report.Truncated {
		body.PartialReasons = append(body.PartialReasons, "usage history exceeded the dashboard read limit")
	}
	if report.RecordLimited {
		body.PartialReasons = append(body.PartialReasons, "usage record count exceeded the dashboard decode limit")
	}
	if report.Malformed > 0 {
		body.PartialReasons = append(body.PartialReasons, fmt.Sprintf("%d malformed usage record(s) were skipped", report.Malformed))
	}
	if report.Oversized > 0 {
		body.PartialReasons = append(body.PartialReasons, fmt.Sprintf("%d oversized usage record(s) were skipped", report.Oversized))
	}

	type sessionAccum struct {
		worker    string
		sessionID string
		totals    usage.Totals
	}
	bySession := make(map[string]*sessionAccum)
	var today, recent usage.Totals
	var oldest time.Time
	invalid := 0
	for _, fact := range facts {
		if !validUsageFact(fact, now) {
			invalid++
			continue
		}
		at := time.UnixMilli(fact.At)
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
		if !at.Before(midnight) && !at.After(now) {
			today.Add(fact)
		}
		if at.Before(recentFrom) || at.After(now) {
			continue
		}
		recent.Add(fact)
		if fact.Kind != usage.KindModel || strings.TrimSpace(fact.Worker) == "" {
			continue
		}
		worker := strings.TrimSpace(fact.Worker)
		key := "worker:" + worker
		if sessionID := strings.TrimSpace(fact.SessionID); sessionID != "" {
			key = "session:" + sessionID
		}
		acc := bySession[key]
		if acc == nil {
			acc = &sessionAccum{worker: worker, sessionID: fact.SessionID}
			bySession[key] = acc
		}
		acc.totals.Add(fact)
	}
	if invalid > 0 {
		body.Partial = true
		body.PartialReasons = append(body.PartialReasons, fmt.Sprintf("%d invalid usage record(s) were skipped", invalid))
	}
	if !oldest.IsZero() {
		body.ObservedFrom = oldest.UTC().Format(time.RFC3339Nano)
	}
	body.Today = usageTotalsBody(today)
	body.Recent = usageTotalsBody(recent)
	for _, acc := range bySession {
		body.RecentBySession = append(body.RecentBySession, UsageSessionRecent{
			Session:             acc.worker,
			SessionID:           acc.sessionID,
			InputTokens:         acc.totals.InputTokens,
			OutputTokens:        acc.totals.OutputTokens,
			CacheReadTokens:     acc.totals.CacheReadTokens,
			CacheCreationTokens: acc.totals.CacheCreationTokens,
			CostUSDEstimate:     acc.totals.CostUSDEstimate,
			Unpriced:            acc.totals.Unpriced,
		})
	}
	slices.SortFunc(body.RecentBySession, func(a, b UsageSessionRecent) int {
		aTokens := usageSessionTokens(a)
		bTokens := usageSessionTokens(b)
		if aTokens != bTokens {
			return cmp.Compare(bTokens, aTokens)
		}
		if byID := strings.Compare(a.SessionID, b.SessionID); byID != 0 {
			return byID
		}
		return strings.Compare(a.Session, b.Session)
	})
	if len(body.RecentBySession) > usageBySessionCap {
		body.RecentBySession = body.RecentBySession[:usageBySessionCap]
	}
	return body
}

func validUsageFact(fact usage.Fact, now time.Time) bool {
	if fact.Kind != usage.KindModel && fact.Kind != usage.KindCompute {
		return false
	}
	if fact.At < 0 || time.UnixMilli(fact.At).After(now.Add(time.Minute)) {
		return false
	}
	if fact.InputTokens < 0 || fact.OutputTokens < 0 || fact.CacheReadTokens < 0 || fact.CacheCreationTokens < 0 {
		return false
	}
	if fact.WallSeconds < 0 || math.IsNaN(fact.WallSeconds) || math.IsInf(fact.WallSeconds, 0) {
		return false
	}
	if fact.CostUSDEstimate < 0 || math.IsNaN(fact.CostUSDEstimate) || math.IsInf(fact.CostUSDEstimate, 0) {
		return false
	}
	return len(fact.SessionID) <= 512 && len(fact.Worker) <= 512 && len(fact.IdempotencyKey) <= 1024
}

func usageSessionTokens(session UsageSessionRecent) int {
	total := 0
	for _, value := range []int{session.InputTokens, session.OutputTokens, session.CacheReadTokens, session.CacheCreationTokens} {
		if value > math.MaxInt-total {
			return math.MaxInt
		}
		total += value
	}
	return total
}

func usageTotalsBody(t usage.Totals) UsageTotals {
	return UsageTotals{
		Invocations:         t.Invocations,
		ComputeFacts:        t.ComputeFacts,
		InputTokens:         t.InputTokens,
		OutputTokens:        t.OutputTokens,
		CacheReadTokens:     t.CacheReadTokens,
		CacheCreationTokens: t.CacheCreationTokens,
		WallSeconds:         t.WallSeconds,
		CostUSDEstimate:     t.CostUSDEstimate,
		Unpriced:            t.Unpriced,
	}
}
