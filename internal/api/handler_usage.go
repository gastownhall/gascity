package api

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/usage"
)

// usageRecentWindow is the trailing window for the "recent" aggregation
// block — sized so dashboard rate gauges can seed from it before live
// events accumulate.
const usageRecentWindow = 5 * time.Minute

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
	Unpriced            int     `json:"unpriced" doc:"Facts with unknown pricing — cost not measured, not free."`
}

// UsageSessionRecent is one session's model usage inside the recent window —
// the per-session feed the dashboard's throughput meters render.
type UsageSessionRecent struct {
	Session             string  `json:"session" doc:"Session (worker) name the facts were attributed to."`
	SessionID           string  `json:"session_id,omitempty" doc:"Session bead id, when attributed."`
	InputTokens         int     `json:"input_tokens" doc:"Prompt tokens in the window."`
	OutputTokens        int     `json:"output_tokens" doc:"Completion tokens in the window."`
	CacheReadTokens     int     `json:"cache_read_tokens" doc:"Prompt-cache read tokens in the window."`
	CacheCreationTokens int     `json:"cache_creation_tokens" doc:"Prompt-cache creation tokens in the window."`
	CostUSDEstimate     float64 `json:"cost_usd_estimate" doc:"List-price estimate for the window; decision-support only."`
}

// usageBySessionCap bounds the per-session breakdown so a huge fleet cannot
// balloon the body; sessions are ranked by window token volume first.
const usageBySessionCap = 24

// UsageBody is the JSON body for GET /v0/city/{cityName}/usage.
type UsageBody struct {
	Totals           UsageTotals          `json:"totals" doc:"All recorded usage for this city."`
	Today            UsageTotals          `json:"today" doc:"Usage since local midnight on the supervisor host."`
	Recent           UsageTotals          `json:"recent" doc:"Usage in the trailing recent window."`
	RecentBySession  []UsageSessionRecent `json:"recent_by_session,omitempty" doc:"Recent-window model usage per session, largest token volume first."`
	RecentWindowSecs int                  `json:"recent_window_secs" doc:"Length of the recent window in seconds."`
	Warnings         []string             `json:"warnings,omitempty" doc:"Malformed usage records skipped during the read."`
}

// UsageOutput is the Huma output envelope for GET /v0/city/{cityName}/usage.
type UsageOutput struct {
	Body UsageBody
}

// humaHandleUsage serves aggregated token/cost usage for one city, read
// from the city's .gc/usage.jsonl fact log. ReadFacts loads the whole log
// per call, so bodies are memoized on the shared time-bucket response
// cache the same way /status is.
func (s *Server) humaHandleUsage(_ context.Context, _ *UsageInput) (*UsageOutput, error) {
	bucket := responseCacheTimeBucket(time.Now())
	if body, ok := cachedResponseAs[UsageBody](s, "usage", bucket); ok {
		return &UsageOutput{Body: body}, nil
	}
	path := filepath.Join(s.state.CityPath(), ".gc", "usage.jsonl")
	facts, warnings, err := usage.ReadFacts(path)
	if err != nil {
		return nil, fmt.Errorf("reading usage facts %s: %w", path, err)
	}
	body := buildUsageBody(facts, warnings, time.Now())
	s.storeResponse("usage", bucket, body)
	return &UsageOutput{Body: body}, nil
}

// buildUsageBody aggregates raw facts into the all-time, since-local-midnight,
// and trailing-window blocks the dashboard renders. Window membership is by
// the fact's emitter-stamped At timestamp (unix millis) against now's locale.
func buildUsageBody(facts []usage.Fact, warnings []string, now time.Time) UsageBody {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	recentFrom := now.Add(-usageRecentWindow)
	body := UsageBody{
		RecentWindowSecs: int(usageRecentWindow / time.Second),
		Warnings:         warnings,
	}
	var totals, today, recent usage.Totals
	// Per-session facts roll through the same canonical fold as the window
	// blocks so the two surfaces cannot disagree (an unpriced fact's cost
	// never counts as spend, per usage.Totals semantics).
	type sessionAccum struct {
		sessionID string
		totals    usage.Totals
	}
	bySession := make(map[string]*sessionAccum)
	for _, f := range facts {
		at := time.UnixMilli(f.At)
		totals.Add(f)
		if !at.Before(midnight) {
			today.Add(f)
		}
		if !at.Before(recentFrom) {
			recent.Add(f)
			if f.Kind == usage.KindModel && f.Worker != "" {
				a, ok := bySession[f.Worker]
				if !ok {
					a = &sessionAccum{sessionID: f.SessionID}
					bySession[f.Worker] = a
				}
				a.totals.Add(f)
			}
		}
	}
	body.Totals = usageTotalsBody(totals)
	body.Today = usageTotalsBody(today)
	body.Recent = usageTotalsBody(recent)
	for worker, a := range bySession {
		body.RecentBySession = append(body.RecentBySession, UsageSessionRecent{
			Session:             worker,
			SessionID:           a.sessionID,
			InputTokens:         a.totals.InputTokens,
			OutputTokens:        a.totals.OutputTokens,
			CacheReadTokens:     a.totals.CacheReadTokens,
			CacheCreationTokens: a.totals.CacheCreationTokens,
			CostUSDEstimate:     a.totals.CostUSDEstimate,
		})
	}
	slices.SortFunc(body.RecentBySession, func(a, b UsageSessionRecent) int {
		ta := a.InputTokens + a.OutputTokens + a.CacheReadTokens + a.CacheCreationTokens
		tb := b.InputTokens + b.OutputTokens + b.CacheReadTokens + b.CacheCreationTokens
		if ta != tb {
			return tb - ta
		}
		return strings.Compare(a.Session, b.Session)
	})
	if len(body.RecentBySession) > usageBySessionCap {
		body.RecentBySession = body.RecentBySession[:usageBySessionCap]
	}
	return body
}

// usageTotalsBody projects the canonical usage fold onto the wire shape.
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
