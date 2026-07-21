package worker

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
	// Record the transcript-session correlation sidecar on the same
	// post-successful-turn beat as the usage telemetry below. Deferred at the top
	// so it runs on every return path — this function has several early returns
	// (suppressed events, no transcript, no new usage) that are unrelated to
	// whether the sidecar should be written, and the Message/Nudge callers expect
	// the write on every successful turn. Best-effort and a no-op unless
	// correlation is armed; it uses its own guard, not invTelemetryMu.
	defer h.writeTranscriptSessionMeta()

	if operationEventsSuppressed(ctx) {
		return
	}
	id := h.currentSessionID()
	if id == "" {
		return
	}
	h.invTelemetryMu.Lock()
	defer h.invTelemetryMu.Unlock()

	info, pr, err := sessionRecordViaManager(h.manager, id)
	if err != nil {
		return
	}
	// Provider-family (not role-name) gate: see the doc comment above. Resolve
	// through the canonical builtin_ancestor → provider_kind → provider ladder
	// (session.ProviderFamilyFromInfo) rather than the local provider_kind →
	// provider two-step, so a wrapped/manifold provider whose raw name does not
	// itself contain "codex"/"claude" (e.g. "mc-codex-wrap" with builtin_ancestor
	// "codex") still resolves to its builtin family instead of silently missing
	// the gate. The normalized family keys the gate, the telemetry label, and the
	// pricing lookup below, so the recorded provider can never drift from the
	// family that gated the record — and the prompt-op seam and the
	// controller-tick sweep resolve family identically.
	providerFamily := invocationUsageFamilyFromInfo(info)
	spec, ok := invocationUsageSpecs[providerFamily]
	if !ok {
		slog.Debug("invocation telemetry: unregistered provider family; skipping",
			slog.String("session_id", id),
			slog.String("provider", strings.TrimSpace(info.Provider)),
			slog.String("provider_kind", strings.TrimSpace(info.ProviderKind)),
			slog.String("builtin_ancestor", strings.TrimSpace(info.BuiltinAncestor)))
		return
	}
	path := spec.discover(h, id, info.CreatedAt, pr.Metadata)
	if path == "" {
		slog.Debug("invocation telemetry: no transcript discovered; skipping",
			slog.String("session_id", id), slog.String("provider", providerFamily))
		return
	}
	usages, err := spec.extract(h.adapter, path)
	if err != nil {
		slog.Debug("invocation telemetry: usage extraction failed; skipping",
			slog.String("session_id", id), slog.String("provider", providerFamily), slog.Any("error", err))
		return
	}
	if len(usages) == 0 {
		slog.Debug("invocation telemetry: transcript carried no usage entries; skipping",
			slog.String("session_id", id), slog.String("provider", providerFamily))
		return
	}
	cursor := strings.TrimSpace(pr.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor])
	pending := usagesAfterCursor(usages, cursor)
	if len(pending) == 0 {
		slog.Debug("invocation telemetry: no new invocations since cursor; skipping",
			slog.String("session_id", id), slog.String("provider", providerFamily))
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
			h.recordModelUsageFact(modelUsageFact(u, pr.Metadata, id, id, info.SessionName, providerFamily, cost, priced, now))
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
// gc costs. The session bead id is carried verbatim as SessionID (the join key to
// the manifold spend plane's EIA session_id and to recall transcripts), distinct
// from the resolved RunID and from Worker (the session name). StepID carries the
// session's gc.active_work_bead when present, and is empty only for ad-hoc,
// manual, or idle sessions. The dedup identity is the invocation's provider message id (or the
// transcript entry uuid when none), so the best-effort cursor races noted on
// recordInvocationTelemetry collapse a re-recorded invocation to one fact at the
// sink via IdempotencyKey. Unpriced is true exactly when the pricing registry
// had no entry for the (family, model) pair; cost is then left zero and must be
// read as "not measured", never as a free invocation.
func modelUsageFact(u sessionlog.TailUsage, meta map[string]string, beadID, sessionID, worker, providerFamily string, cost float64, priced bool, now time.Time) usage.Fact {
	// beadID and sessionID are the session bead id and the run-id fallback — the
	// same fields the retired ResolveRunID(bead.Metadata, bead.ID, sessionID) read
	// from the raw bead. At the sole production call site they are equal (the
	// handle's currentSessionID == the session bead id); the params stay distinct
	// so the run-chain precedence contract is preserved verbatim.
	runID := beadmeta.ResolveRunID(meta, beadID, sessionID)
	// The run STEP: the session's current work bead's gc.step_id, stamped at the claim
	// hook (gc.active_work_bead). Read from the SAME session-bead snapshot as runID so
	// StepID always names a step under this RunID. Empty when the session isn't on a
	// formula work bead (ad-hoc/manual/idle) — run-level attribution, matching events.
	stepID := strings.TrimSpace(meta[beadmeta.ActiveWorkBeadMetadataKey])
	reqID := usageIdentity(u)
	if !priced {
		cost = 0
	}
	return usage.Fact{
		RunID:               runID,
		SessionID:           strings.TrimSpace(sessionID),
		StepID:              stepID,
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
	discover func(h *SessionHandle, id string, createdAt time.Time, meta map[string]string) string
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

// invocationUsageFamilyFromInfo resolves a session's invocation-usage family
// through the canonical provider-family ladder (builtin_ancestor → provider_kind
// → provider, via session.ProviderFamilyFromInfo) and then normalizes the result
// to a registered family bucket via invocationUsageFamily. Routing through the
// canonical ladder is what lets a wrapped/manifold provider whose raw name does
// not itself contain "codex"/"claude" still resolve to its builtin family, so
// the prompt-op seam and the controller-tick sweep agree on family.
func invocationUsageFamilyFromInfo(info sessionpkg.Info) string {
	return invocationUsageFamily(sessionpkg.ProviderFamilyFromInfo(info, ""))
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
func discoverInvocationTranscriptViaManager(h *SessionHandle, id string, _ time.Time, _ map[string]string) string {
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
func discoverCodexInvocationTranscript(h *SessionHandle, _ string, createdAt time.Time, meta map[string]string) string {
	anchor := createdAt
	if woke, err := time.Parse(time.RFC3339, strings.TrimSpace(meta["last_woke_at"])); err == nil {
		anchor = woke
	}
	workDir := contract.WorkerDirFromMetadata(meta)
	if sessionKey := strings.TrimSpace(meta["session_key"]); sessionKey != "" {
		return sessionlog.FindCodexSessionFileByID(
			h.adapter.SearchPaths, workDir, sessionKey, createdAt, anchor)
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

// usagesSinceCursor is the end-of-interval sweep's fold: it returns every window
// entry strictly after the cursor identity. Unlike usagesAfterCursor (the
// prompt-op seam's conservative newest-only fallback), when the cursor is empty
// it returns ALL window entries — the sweep's whole job is to recover the
// interval's trailing invocations that the prompt-op seam never recorded,
// bounded only by the extractor's tail window. When the cursor is present but
// has scrolled out of the window it also returns all window entries: read-time
// IdempotencyKey dedup (usage.ReadFacts) collapses any overlap with
// already-recorded facts, so recovering the whole bounded tail cannot
// double-count.
func usagesSinceCursor(usages []sessionlog.TailUsage, cursor string) []sessionlog.TailUsage {
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
	return usages
}

// SweepSessionModelUsage is the controller-tick counterpart to the prompt-op
// recordInvocationTelemetry seam: it recovers a terminal session's trailing
// model-token usage that no prompt operation ever recorded. A pool-routed,
// hook-self-driven agent gets one claim nudge and then self-drives, so every
// invocation after its last prompt op — the common case for the whole awake
// interval — would otherwise go unrecorded (documented at
// recordInvocationTelemetry). Called beside the compute-fact emitter for each
// session whose awake interval just ended, it discovers the session's
// transcript, extracts per-invocation usage after the persisted invocation-usage
// cursor, emits one model usage.Fact per new entry to the factory's usage sink
// (and mirrors the prompt-op seam's gc.agent.tokens.* / cost OTel metrics per
// invocation), and advances the cursor.
//
// It is best-effort. The returned settled reports whether the interval is fully
// accounted for and needs no retry: true when the transcript was read (even if
// nothing new was pending) OR the miss is permanent (unregistered family, or a
// terminal codex session that never captured a session_key); false when the miss
// is transient (no transcript discovered yet, an extraction error, or a sink
// Record failure) so the caller should retry on a later tick. err is reserved for
// a sink Record failure; the cursor is then advanced only through the last
// successfully recorded entry so the retry resumes at the gap rather than
// skipping it. Every gate is slog.Debug'd so a fleet-wide zero is attributable in
// the field instead of silently swallowed.
//
// Coverage ceiling (known, intentional — do NOT widen): discovery and extraction
// read only the extractor's bounded transcript tail (a 64KB window per family),
// so a very long autonomous interval recovers only its final few invocations —
// earlier model/cost facts of that interval are lost. Widening the tail re-opens
// the unbounded-scan and misattribution risks that bound exists to prevent;
// fuller recovery is a separate, deliberate change.
//
// Overlap with the prompt-op seam is safe: both stamp usage.ModelIdempotencyKey,
// which usage.ReadFacts collapses, so an invocation recorded by both beats folds
// to one fact. meta is the fresh session-bead metadata (RunID/StepID resolution,
// the session_key, work dir, and cursor all read from it), and now stamps the
// emitted facts.
func (f *Factory) SweepSessionModelUsage(ctx context.Context, id string, meta map[string]string, now time.Time) (emitted int, settled bool, err error) {
	id = strings.TrimSpace(id)
	if f == nil || id == "" || meta == nil {
		return 0, true, nil
	}
	sink := f.usageSink
	if sink == nil || sink == usage.Discard {
		return 0, true, nil
	}
	// Canonical builtin_ancestor → provider_kind → provider ladder, normalized to
	// a registered family bucket — the metadata twin of invocationUsageFamilyFromInfo,
	// so the sweep and the prompt-op seam never disagree on family.
	family := invocationUsageFamily(sessionpkg.ProviderFamilyFromMetadata(meta, ""))
	if _, ok := invocationUsageSpecs[family]; !ok {
		// Permanent: an unregistered family will never gain an extractor by retrying.
		slog.Debug("model-usage sweep: unregistered provider family; skipping",
			slog.String("session_id", id), slog.String("provider", strings.TrimSpace(meta["provider"])))
		return 0, true, nil
	}
	if family == "codex" && strings.TrimSpace(meta["session_key"]) == "" {
		// Permanent: a terminal codex session that never captured its session_key
		// (SessionStart hook bypassed) has no keyed rollout to find, and no later
		// hook will fire while it is asleep — retrying cannot help.
		slog.Debug("model-usage sweep: codex session has no session_key; settling",
			slog.String("session_id", id))
		return 0, true, nil
	}
	path := f.discoverSweepTranscript(family, id, meta, now)
	if path == "" {
		// Transient: the rollout may not be flushed yet at interval end, so leave the
		// interval unsettled for a retry on a later tick.
		slog.Debug("model-usage sweep: no transcript discovered; will retry",
			slog.String("session_id", id), slog.String("provider", family))
		return 0, false, nil
	}
	usages, extractErr := f.Adapter().InvocationUsage(family, path)
	if extractErr != nil {
		// Transient: a torn mid-write tail can fail the parse; retry on a later tick.
		slog.Debug("model-usage sweep: usage extraction failed; will retry",
			slog.String("session_id", id), slog.String("provider", family), slog.Any("error", extractErr))
		return 0, false, nil
	}
	cursor := strings.TrimSpace(meta[sessionpkg.MetadataKeyInvocationUsageCursor])
	pending := usagesSinceCursor(usages, cursor)
	if len(pending) == 0 {
		// Swept: the transcript was read and the cursor is already current.
		return 0, true, nil
	}
	registry := f.pricing
	if registry == nil {
		registry = defaultPricingRegistry()
	}
	workerName := strings.TrimSpace(meta["session_name"])
	agentName := sweepAgentName(meta)
	lastRecorded := ""
	for _, u := range pending {
		cost, priced := registry.Estimate(family, u.Model, pricing.Usage{
			PromptTokens:        u.InputTokens,
			CompletionTokens:    u.OutputTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheCreationTokens: u.CacheCreationTokens,
		})
		// Mirror the prompt-op seam's OTel metrics so the agents this sweep recovers
		// facts for also get live gc.agent.tokens.* / cost series — the family keys
		// the label identically to the emitted fact's Provider so metrics and facts
		// agree. Metrics fire before the sink write: they must be recorded even if the
		// sink Record below fails.
		labels := telemetry.InvocationLabels{
			AgentName: agentName,
			Model:     u.Model,
			Provider:  family,
		}
		telemetry.RecordInvocationTokens(ctx, labels,
			int64(u.InputTokens), int64(u.OutputTokens),
			int64(u.CacheReadTokens), int64(u.CacheCreationTokens))
		if priced {
			telemetry.RecordInvocationCostEstimate(ctx, labels, cost)
		}
		fact := modelUsageFact(u, meta, id, id, workerName, family, cost, priced, now)
		if recErr := sink.Record(ctx, fact); recErr != nil {
			// Stop at the first failure and advance the cursor only through the last
			// success, so the next sweep resumes here instead of skipping the gap.
			err = recErr
			break
		}
		emitted++
		lastRecorded = usageIdentity(u)
	}
	if lastRecorded != "" {
		if cursorErr := f.manager.PersistInvocationUsageCursor(id, lastRecorded); cursorErr != nil {
			slog.Debug("model-usage sweep: persisting invocation usage cursor failed; next sweep may re-record",
				slog.String("session_id", id), slog.Any("error", cursorErr))
		}
	}
	// Settled only when the whole pending batch reached the sink: a sink Record
	// failure (err != nil) leaves the interval unsettled so the caller retries the
	// remainder — the cursor advanced only through the last success, and the
	// IdempotencyKey collapses any already-recorded fact on the retry.
	return emitted, err == nil, err
}

// discoverSweepTranscript resolves the transcript for the end-of-interval model
// sweep: claude through the manager's keyed lookup, codex through the captured
// session_key matched against the rollout filename suffix. The codex lookup is
// bounded to the awake interval's day window via
// sessionlog.FindCodexSessionFileByID(notBefore=awake_started_at,
// notAfter=slept_at||now) — NOT the unbounded date-tree walk — because this runs
// on the SYNCHRONOUS controller reconcile tick, where a full codex-history scan
// per terminal session per tick could stall the tick. FindCodexSessionFileByID
// still resolves resumed sessions whose rollout predates this interval: the
// keyed lookup also scans the session UUID's own creation day (UUIDv7 hint ±2
// days), which is where a resumed rollout was first written. A codex session
// with no captured session_key yields "": the sweep has no window fallback,
// matching the keyed-miss-records-nothing contract of the prompt-op codex
// discovery.
func (f *Factory) discoverSweepTranscript(family, id string, meta map[string]string, now time.Time) string {
	switch family {
	case "codex":
		key := strings.TrimSpace(meta["session_key"])
		if key == "" {
			slog.Debug("model-usage sweep: codex session has no session_key; skipping",
				slog.String("session_id", id))
			return ""
		}
		notBefore, notAfter := sweepIntervalWindow(meta, now)
		return sessionlog.FindCodexSessionFileByID(
			f.searchPaths, contract.WorkerDirFromMetadata(meta), key, notBefore, notAfter)
	default:
		path, terr := f.manager.TranscriptPath(id, f.searchPaths)
		if terr != nil {
			return ""
		}
		return strings.TrimSpace(path)
	}
}

// sweepAgentName resolves the metric AgentName label for a swept session from
// its metadata, mirroring the prompt-op seam's agent_name → alias → session_name
// fallback so the gc.agent.tokens.* series carry the same identity from either
// emission beat.
func sweepAgentName(meta map[string]string) string {
	for _, k := range []string{"agent_name", "alias", "session_name"} {
		if v := strings.TrimSpace(meta[k]); v != "" {
			return v
		}
	}
	return ""
}

// sweepIntervalWindow derives the codex discovery date window from a terminal
// session's awake interval: [awake_started_at, slept_at]. When slept_at is
// missing or not after the start (stale for non-sleep terminal states), it falls
// back to now as the interval end, so notAfter is always set and never reversed —
// FindCodexSessionFileByID requires a non-zero notAfter and refuses a reversed
// range. The finder pads both ends by a local day, so an off-by-seconds bound
// cannot drop the rollout's day directory.
func sweepIntervalWindow(meta map[string]string, now time.Time) (notBefore, notAfter time.Time) {
	notAfter = now
	if started, err := time.Parse(time.RFC3339, strings.TrimSpace(meta["awake_started_at"])); err == nil {
		notBefore = started
	}
	if sleptRaw := strings.TrimSpace(meta["slept_at"]); sleptRaw != "" {
		if slept, err := time.Parse(time.RFC3339, sleptRaw); err == nil && slept.After(notBefore) {
			notAfter = slept
		}
	}
	return notBefore, notAfter
}
