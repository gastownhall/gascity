package worker

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/pricing"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/usage"
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
// (session.MetadataKeyInvocationUsageCursor), and — when the handle has a live
// usage sink — emits one model usage.Fact per entry to that sink (the data
// behind gc costs and external aggregators). Both consume the same extracted
// per-invocation usage and the same cursor, so token metrics and model facts
// stay aligned. It is called at
// prompt-operation (message/nudge) finish: prompt submission returns at
// keystroke-delivery time, so the transcript tail at that point holds
// previously COMPLETED invocations — the turn this operation triggers is
// recorded by the next prompt operation on the session. Entries beyond the
// extractor's scan window (a 64KB tail for claude and codex) or after the
// final prompt op of a session go unrecorded.
//
// Coverage is per transcript provider family, driven by the
// invocationUsageSpecs registry, with per-family discovery bounds:
//
//   - claude: Manager.TranscriptPath (session-key stat or ambiguity-guarded
//     project-slug listing — cheap) + the Claude JSONL tail extractor.
//   - codex: identity-first. When the session bead carries a session_key
//     (captured from the codex hook; codex rollout filenames end in the
//     same uuid), the rollout is resolved by that suffix via
//     sessionlog.FindCodexSessionFileByID over the day dirs between bead
//     creation and the wake anchor — this is what keeps telemetry alive for
//     resumed sessions, whose rollout filename timestamp is the FIRST start
//     and predates every later wake. A keyed miss records nothing: a
//     window-found rollout with a different suffix would be misattribution.
//     Without a session_key (fresh start before the hook fires),
//     sessionlog.FindCodexSessionFileNear anchored at the session's
//     last_woke_at metadata (falling back to bead creation time) opens only
//     the rollout day directories intersecting the anchor window. Neither
//     route uses Manager.TranscriptPath, whose codex route walks the full
//     date tree (multi-second scans inside a prompt operation). Un-keyed
//     sessions whose rollout began outside the window (for example
//     crash-adopted sessions whose last_woke_at was cleared, or codex CLIs
//     running under a different local timezone than this process — rollout
//     filenames are local-time and parsed in gc's time.Local) silently
//     record nothing: bounded best-effort by design.
//
// Families without a registered spec are skipped before any discovery —
// their workdir-based fallbacks walk real session stores and no usage
// extractor exists for their transcript formats.
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
// Accepted as best-effort. RuntimeHandle prompt ops are permanently out of
// scope (decided in ga-tkvb31, not a pending gap): runtime-only sessions
// have no transcript adapter, no session bead for the cursor, and no agent
// identity, and will not gain bead-backed identity just for telemetry.
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
	// Provider-family (not role-name) gate: see the doc comment above. The
	// normalized family keys the gate, the telemetry label, and the pricing
	// lookup below, so the recorded provider can never drift from the family
	// that gated the record.
	providerFamily := invocationUsageFamily(transcriptProvider)
	spec, ok := invocationUsageSpecs[providerFamily]
	if !ok {
		return
	}
	path := spec.discover(h, id, b)
	if path == "" {
		return
	}
	usages, err := spec.extract(h.adapter, path)
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
	// Model usage facts flow to the configured usage sink (gc costs / external
	// aggregators), independent of the metrics above and of operation-event
	// recording: a sink-only handle (CLI factory path) still emits. Resolved once
	// per loop because the gate is per-handle.
	emitFacts := h.usageFactRecordingEnabled()
	now := time.Now().UTC()
	for _, u := range pending {
		labels := telemetry.InvocationLabels{
			AgentName: agentName,
			Model:     u.Model,
			Provider:  providerFamily,
		}
		telemetry.RecordInvocationTokens(ctx, labels,
			int64(u.InputTokens), int64(u.OutputTokens),
			int64(u.CacheReadTokens), int64(u.CacheCreationTokens))
		cost, priced := h.pricing.Estimate(providerFamily, u.Model, pricing.Usage{
			PromptTokens:        u.InputTokens,
			CompletionTokens:    u.OutputTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheCreationTokens: u.CacheCreationTokens,
		})
		if priced {
			telemetry.RecordInvocationCostEstimate(ctx, labels, cost)
		}
		if emitFacts {
			h.recordModelUsageFact(modelUsageFact(u, b, id, info.SessionName, providerFamily, cost, priced, now))
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

// modelUsageFact builds a model usage Fact from one transcript invocation. The
// run id is resolved from the session bead through the shared
// beadmeta.ResolveRunID — the same resolver the compute-fact emitter uses — so a
// run's model and compute facts carry the same RunID and group together in
// gc costs. The dedup identity is the invocation's provider message id (or the
// transcript entry uuid when none), so the best-effort cursor races noted on
// recordInvocationTelemetry collapse a re-recorded invocation to one fact at the
// sink via IdempotencyKey. Unpriced is true exactly when the pricing registry
// had no entry for the (family, model) pair; cost is then left zero and must be
// read as "not measured", never as a free invocation.
func modelUsageFact(u sessionlog.TailUsage, bead beads.Bead, sessionID, worker, providerFamily string, cost float64, priced bool, now time.Time) usage.Fact {
	runID := beadmeta.ResolveRunID(bead.Metadata, bead.ID, sessionID)
	reqID := usageIdentity(u)
	if !priced {
		cost = 0
	}
	return usage.Fact{
		RunID:               runID,
		Worker:              strings.TrimSpace(worker),
		Kind:                usage.KindModel,
		Model:               strings.TrimSpace(u.Model),
		Provider:            strings.TrimSpace(providerFamily),
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		CostUSDEstimate:     cost,
		Unpriced:            !priced,
		UpstreamReqID:       reqID,
		At:                  now.UnixMilli(),
		IdempotencyKey:      usage.ModelIdempotencyKey(runID, reqID),
	}
}

// invocationUsageSpec binds one transcript provider family to its bounded
// transcript discovery and its usage extractor. discover returns "" (and
// extract is then never called) when its family's attribution bound finds
// no transcript — but the strength of that bound varies by family. The
// codex route is identity-bound (session_key matched against the rollout
// filename uuid) or wake-window+cwd bounded, and the claude route is
// session-keyed with an ambiguity-guarded same-workdir fallback. All errors
// are swallowed so telemetry never affects operations.
type invocationUsageSpec struct {
	discover func(h *SessionHandle, id string, b beads.Bead) string
	extract  func(a SessionLogAdapter, path string) ([]sessionlog.TailUsage, error)
}

// codexInvocationDiscoveryWindow bounds how far after the wake anchor a
// codex rollout's filename timestamp may fall and still be attributed to
// the session. The rollout is created when the codex process starts, which
// follows the wake within seconds; the window absorbs slow starts without
// re-opening the unbounded date-tree search.
const codexInvocationDiscoveryWindow = 10 * time.Minute

// invocationUsageSpecs registers the transcript provider families covered
// by invocation telemetry. Families absent from this map are skipped before
// any transcript discovery runs.
var invocationUsageSpecs = map[string]invocationUsageSpec{
	"claude": {
		discover: discoverInvocationTranscriptViaManager,
		extract:  SessionLogAdapter.TailUsage,
	},
	"codex": {
		discover: discoverCodexInvocationTranscript,
		extract:  SessionLogAdapter.CodexTailUsage,
	},
}

// invocationUsageFamily resolves a provider string to its registered
// invocation-usage family key: claude-family providers (including
// claude-eco) match by name, codex resolves through sessionlog.ProviderFamily,
// and everything else returns "" (unregistered).
func invocationUsageFamily(provider string) string {
	if strings.Contains(strings.ToLower(provider), "claude") {
		return "claude"
	}
	if sessionlog.ProviderFamily(provider) == "codex" {
		return "codex"
	}
	return ""
}

// InvocationUsageFamily resolves the provider's invocation-usage family and
// reports whether the worker has a per-invocation token/cost extractor
// registered for it. It is the canonical query for invocation-telemetry
// support: the worker conformance suite uses it so usage coverage stays
// aligned with invocationUsageSpecs — adding a family there forces a
// conformance decision rather than leaving a silent gap.
func InvocationUsageFamily(provider string) (family string, supported bool) {
	family = invocationUsageFamily(provider)
	_, supported = invocationUsageSpecs[family]
	return family, supported
}

// discoverInvocationTranscriptViaManager resolves the transcript through
// Manager.TranscriptPath — safe for families whose route there is cheap
// (claude keyed lookup). Errors are swallowed.
func discoverInvocationTranscriptViaManager(h *SessionHandle, id string, _ beads.Bead) string {
	path, err := h.manager.TranscriptPath(id, h.adapter.SearchPaths)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(path)
}

// discoverCodexInvocationTranscript resolves a codex rollout. Identity
// first: when the bead carries the codex session_key (the rollout filename
// uuid suffix), the rollout is keyed by that suffix between bead creation
// and the wake anchor — resumed sessions append to the ORIGINAL rollout
// whose filename timestamp predates every later wake, so only the keyed
// lookup finds them. A keyed miss returns "" with NO window fallback: a
// window-found rollout with a different suffix would be misattribution.
// Without a session_key (fresh start before the hook captures it), the
// bounded wake-anchored window lookup runs: the anchor is the session's
// last_woke_at metadata (set by reconciler wakes), falling back to bead
// creation time for directly-created sessions. Ambiguous or out-of-window
// rollouts yield "" — telemetry silently records nothing rather than
// misattributing.
func discoverCodexInvocationTranscript(h *SessionHandle, _ string, b beads.Bead) string {
	anchor := b.CreatedAt
	if woke, err := time.Parse(time.RFC3339, strings.TrimSpace(b.Metadata["last_woke_at"])); err == nil {
		anchor = woke
	}
	workDir := contract.WorkerDirFromMetadata(b.Metadata)
	if sessionKey := strings.TrimSpace(b.Metadata["session_key"]); sessionKey != "" {
		return sessionlog.FindCodexSessionFileByID(
			h.adapter.SearchPaths, workDir, sessionKey, b.CreatedAt, anchor)
	}
	return sessionlog.FindCodexSessionFileNear(
		h.adapter.SearchPaths,
		workDir,
		anchor,
		codexInvocationDiscoveryWindow,
	)
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
