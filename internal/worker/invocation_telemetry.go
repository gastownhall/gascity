package worker

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/pricing"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/telemetry"
)

var (
	defaultPricingOnce sync.Once
	defaultPricing     *pricing.Registry
)

// defaultPricingRegistry lazily builds the shipped-defaults pricing registry
// for handles constructed without an explicit registry, so bare factories
// still estimate cost.
func defaultPricingRegistry() *pricing.Registry {
	defaultPricingOnce.Do(func() {
		defaultPricing = pricing.BuildRegistry(nil, nil)
	})
	return defaultPricing
}

// recordInvocationTelemetry emits gc.agent.tokens.* and
// gc.agent.invocation.cost_usd for usage-bearing transcript entries that
// completed since the session's persisted invocation-usage cursor
// (session.MetadataKeyInvocationUsageCursor). It is called at
// prompt-operation (message/nudge) finish: prompt submission returns at
// keystroke-delivery time, so the transcript tail at that point holds
// previously COMPLETED invocations — the turn this operation triggers is
// recorded by the next prompt operation on the session. Entries beyond the
// 64KB tail window or after the final prompt op of a session go unrecorded.
//
// Coverage is gated to claude-family transcripts: that is the only format
// ExtractTailUsage parses today, and its discovery (session-key stat or
// project-slug listing via Manager.TranscriptPath, ambiguity-guarded) is
// cheap. Other families are skipped before any discovery — their
// workdir-based fallbacks walk real date-tree session stores (multi-second
// scans inside a prompt operation) and their transcript formats would yield
// no usage entries anyway.
//
// Cost is skipped entirely (not zero-filled) when the pricing registry has
// no entry for the (provider family, model) pair, so missing pricing data is
// never mistaken for free usage. gc.agent.invocation.latency_ms is
// intentionally NOT recorded here: no measured per-invocation latency source
// exists, and the wrapping operation's DurationMs is explicitly excluded by
// RecordInvocationLatency's contract.
//
// Best-effort by design: all errors are swallowed so telemetry never affects
// operations. The persisted cursor (the message identity of the last
// recorded invocation) dedupes across prompt-operation boundaries, but the
// read-record-persist sequence is not atomic: concurrent prompt ops on the
// same session — whether in separate processes or on separate handles in one
// process (the API server constructs a fresh handle per request) — can each
// read the same stale cursor and double-record the pending batch.
// invTelemetryMu only serializes ops that share a single handle instance.
// Accepted as best-effort. RuntimeHandle prompt ops are not covered:
// runtime-only sessions have no transcript adapter, no session bead for the
// cursor, and no agent identity.
func (h *SessionHandle) recordInvocationTelemetry(ctx context.Context) {
	if operationEventsSuppressed(ctx) {
		return
	}
	id := h.currentSessionID()
	if id == "" {
		return
	}
	h.invTelemetryMu.Lock()
	defer h.invTelemetryMu.Unlock()

	info, b, err := h.manager.GetWithBead(id)
	if err != nil {
		return
	}
	transcriptProvider := strings.TrimSpace(b.Metadata["provider_kind"])
	if transcriptProvider == "" {
		transcriptProvider = strings.TrimSpace(info.Provider)
	}
	// Provider-name (not role-name) gate: see the doc comment above.
	if !strings.Contains(strings.ToLower(transcriptProvider), "claude") {
		return
	}
	path, err := h.manager.TranscriptPath(id, h.adapter.SearchPaths)
	if err != nil || strings.TrimSpace(path) == "" {
		return
	}
	usages, err := h.adapter.TailUsage(path)
	if err != nil || len(usages) == 0 {
		return
	}
	cursor := strings.TrimSpace(b.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor])
	pending := usagesAfterCursor(usages, cursor)
	if len(pending) == 0 {
		return
	}

	agentName := strings.TrimSpace(info.AgentName)
	if agentName == "" {
		agentName = strings.TrimSpace(info.Alias)
	}
	if agentName == "" {
		agentName = strings.TrimSpace(info.SessionName)
	}
	// The provider label and pricing key must be the provider family (for
	// example "claude"), never the profile string ("claude/tmux-cli").
	providerFamily := profileFamily(h.session.Profile)
	if providerFamily == "" {
		providerFamily = strings.TrimSpace(info.Provider)
	}
	if providerFamily == "" {
		providerFamily = strings.TrimSpace(h.session.Provider)
	}

	for _, u := range pending {
		labels := telemetry.InvocationLabels{
			AgentName: agentName,
			Model:     u.Model,
			Provider:  providerFamily,
		}
		telemetry.RecordInvocationTokens(ctx, labels,
			int64(u.InputTokens), int64(u.OutputTokens),
			int64(u.CacheReadTokens), int64(u.CacheCreationTokens))
		if cost, ok := h.pricing.Estimate(providerFamily, u.Model, pricing.Usage{
			PromptTokens:        u.InputTokens,
			CompletionTokens:    u.OutputTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheCreationTokens: u.CacheCreationTokens,
		}); ok {
			telemetry.RecordInvocationCostEstimate(ctx, labels, cost)
		}
	}
	// Best-effort: a failed cursor write means the next prompt op may
	// re-record these entries, which the residual-race note above covers.
	// Debug-logged so a persistently failing store is diagnosable.
	if err := h.manager.PersistInvocationUsageCursor(id, usageIdentity(pending[len(pending)-1])); err != nil {
		slog.Debug("persisting invocation usage cursor failed; next prompt op may re-record",
			slog.String("session_id", id), slog.Any("error", err))
	}
}

// usageIdentity returns the dedup identity of one invocation: the provider
// message id when present (shared by every content-block entry of one API
// response, stable across prompt-operation boundaries), falling back to the
// transcript entry uuid for entries without one.
func usageIdentity(u sessionlog.TailUsage) string {
	if u.MessageID != "" {
		return u.MessageID
	}
	return u.EntryUUID
}

// usagesAfterCursor returns entries strictly after the cursor identity when
// the cursor is present in the tail window. Matching on the message identity
// (not the entry uuid) keeps an invocation single-counted even when its
// content-block entries straddle a prompt-operation boundary: late blocks of
// an already-recorded message collapse to the cursor identity and are
// excluded. When the cursor is empty or has scrolled out of the window it
// conservatively returns only the newest entry — never re-counting a
// historical tail in bulk, at the cost of possible undercounting.
func usagesAfterCursor(usages []sessionlog.TailUsage, cursor string) []sessionlog.TailUsage {
	if len(usages) == 0 {
		return nil
	}
	if cursor != "" {
		for i := len(usages) - 1; i >= 0; i-- {
			if usageIdentity(usages[i]) == cursor {
				return usages[i+1:]
			}
		}
	}
	return usages[len(usages)-1:]
}
