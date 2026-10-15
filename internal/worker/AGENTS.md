# internal/worker — change guide

`internal/worker/handle.go` is the canonical boundary for session creation
and lifecycle operations. Callers outside `internal/session` reach sessions
through `worker.Handle`, not through `session.Manager`.

## Active migration: worker boundary

Started `12a0a848` on Apr 17 2026, in progress. New code on affected paths
must take the canonical route, not the legacy route.

Production `cmd/gc/*.go` files must route through `worker.Handle` — enforced
by `TestGCNonTestFilesStayOnWorkerBoundary` in
`cmd/gc/worker_boundary_import_test.go`, which forbids non-test files from
importing `session.NewManagerWithOptions(`, `worker.SessionHandle`,
`sessionlog`, and similar bypass paths in `cmd/gc`. The remaining
manager-construction/direct-create bypasses are split by category:
`internal/api/session_manager.go` constructs `session.Manager` values for API
handlers. (`internal/api/session_resolution.go`'s named-session create was
converted to the worker boundary — it now routes through
`worker.Handle.Create(ctx, worker.CreateModeStarted)` via
`newResolvedWorkerSessionHandle`, no longer calling `mgr.CreateSession(...)`
directly.) Session creation goes through the single
`Manager.CreateSession(ctx, session.CreateOptions{...})` entry point
(`NewManagerWithOptions` is the sole Manager constructor). This list is not a
sessionlog read-site inventory; stream and transcript readers in
`internal/api/` and `internal/session/` still read session logs directly.
Package-internal helpers in `internal/session/` may construct and use
`session.Manager`; tests may construct it directly. Do not add new non-test
direct `session.Manager.CreateSession` call sites outside the worker boundary.

## Bounded observation

`worker.ObserveBounded` is how a caller waits on an observation (a handle read,
a runtime probe, a metadata read) without waiting forever. The call under the
wait can hang on a wedged tmux subprocess or a stalled store, and nothing
cancels it, so the wait is raced against the caller's context and abandoned
when the context ends. Take the bound from the caller's own context.

- An abandoned wait is an error wrapping both `runtime.ErrRuntimeUnavailable`
  and the context error. It is never a confirmed absence: callers must treat it
  as "do not know" (`SESSION-RUNTIME-009`), not as `ErrSessionNotFound`.
- An answer that lands as the context ends is used, not dropped.
- At most one observation per key is in flight. While an earlier one has not
  returned, a new call gets `ErrObservationOutstanding` (also
  `runtime.ErrRuntimeUnavailable`) and starts nothing, so a wedged session
  costs one goroutine, not one per call.
- The key names what is observed: one session in one city. Every caller that
  observes the same session must derive the key the same way, or the
  one-in-flight limit no longer holds. `cmd/gc` derives it in
  `postStartObservationKey`.

## Codex activity

Codex activity comes from matching native turn lifecycle events, not assistant
text or token usage. History and tail observations must use the same provider
parser. Large tool output must not erase the latest turn boundary. A torn final
record cannot report idle, since it may be the next turn's start, but an open
turn still reports in-turn. A turn start more than 16 MiB before the end of the
rollout reports unknown instead of rescanning the whole rollout on every poll.

## History identity

`HistorySnapshot.GCSessionID` is the durable Gas City session bead ID. Unless
the caller supplies one, the logical conversation ID is
`session.Info.LogicalConversationID`: the bead's current continuation epoch,
`<bead-id>#<epoch>`, and for a provider with a session-id flag also the key
each new conversation runs under, `<bead-id>#<epoch>/<session-key>`. A fresh
reset starts a new conversation and a resumed restart keeps it (WC-CONT-002,
WC-CONT-001), so a snapshot a long-lived handle cached before a fresh reset is
not stitched in front of the new conversation. A read can still return the
abandoned conversation under the new conversation's identity, and history then
shows it in front of the new one: for a provider without a session-id flag,
before the new transcript exists or after a fresh wake that keeps the epoch;
for one with a flag, before a start the Manager launched without a key has its
key written, or from an ACP capture, which the epoch alone scopes
(`SESSION-HISTORY-CONVERSATION-EPOCH`). Native resume-key discovery must not
replace either ID. Provider identity belongs in `ProviderSessionID`; a late
hook must not make an unchanged transcript appear to be a different stream.
The one exception is a provider with a session-id flag whose start the Manager
launched without its key: its identity takes the key once one is written,
normally by the provider's hook.
