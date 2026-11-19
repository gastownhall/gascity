# Session Module

Read `REQUIREMENTS.md` before changing session behavior, tests, or callers that
depend on session lifecycle semantics. Read `PLAN.md` before changing session
ownership boundaries, deciders, command APIs, events, or extraction sequencing.
(`DESIGN.md` is the long-form architecture document; it has not landed yet and
is iterating under design-review bead `ga-unpr2y`.)

## Purpose

`internal/session` owns the session primitive: start, stop, prompt, observe,
identify, and project provider-backed sessions. Session state is bead-backed;
runtime state is observed through providers; work must survive session churn.

## Reconcile Rule

When code, tests, or docs disagree with `REQUIREMENTS.md`, do not silently pick
one. Reconcile them in the same change:

- If the requirement is right, update code and add or adjust tests.
- If current behavior is right, update `REQUIREMENTS.md` and cite proof.
- If the mismatch is ambiguous, stop and ask for the product rule.

New session behavior needs at least one scenario row in `REQUIREMENTS.md` with
evidence from tests, source, an issue, or a commit.

## Boundaries

- Use `ProjectLifecycle` and lifecycle helper APIs for read-side decisions.
- Inside `internal/session`, lifecycle transition/patch helpers may build
  mutations. Outside `internal/session`, call session-owned command APIs instead
  of applying patch maps or writing lifecycle metadata directly.
- Treat duplicated session decisions in API, CLI, worker, or runtime adapters as
  extraction candidates. Prefer moving the rule into `internal/session` and
  leaving the adapter to gather facts, execute actions, or render responses.
- Extract decisions one cluster at a time. Do not introduce a broad session
  facade before a narrow contract and tests prove it.
- Do not add production session lifecycle or identity metadata writes outside
  `internal/session`. Existing external `SetMetadata*`/bead mutation sites are
  extraction candidates unless they are tests, fixtures, or migration/doctor
  repair code.
- Keep provider-specific runtime behavior behind `internal/runtime`.
- An operator's submitted message under lane authority (message in `[ops]
  authoritative`: POST /submit and /messages, and `gc session submit`'s
  local fallback) is accepted only through `lanesubmit.Accept`
  (`internal/session/lanesubmit`); the projections keep only their answers.
  None of them previews that op, reads the op under its key, proposes it or
  resumes the session for it itself (SESSION-START-025). The one exception
  is `gc session submit` while its own config does not list message and an
  API's lane host still does (a rollback before the controller restarts):
  it answers from the API's lane op as #8399 did, until OJ-21.
- Deferred submits go through the Manager's `DeferredSubmitQueue`
  (`WithDeferredSubmitQueue`); no non-test session file touches the
  state.json nudge queue directly. Enforced by
  `TestSessionReachesTheNudgeQueueOnlyThroughDeferredSubmitQueue`.
- The v2 stop request's keys (`stop_request_keys.go`) are read by cmd/gc's
  `activeStop`, inside the reconciler, by cmd/gc's `v2SignaledUnacked`, for
  legacy's rollback guard, and by `ReadStopRequest`, for the content gate,
  whose one caller is the conversation lane's row read. `ReadStopRequest`
  takes the reading of the controller's latched mode: residue
  (`StopRequestsIgnored`) under legacy, live under v2. The controller's lane
  host reads its own latch; every reader outside the controller (the CLI's
  inline pass, the prompt hook) asks the running controller
  (`reconciler-mode` on the controller socket), never the on-disk config,
  and reads live when the mode is unknown. Only cmd/gc's
  `conversation_lane_view.go` turns a mode into a reading
  (`laneStopReading`, `controllerStopReading`); no other caller names
  `StopRequestsLive` or `StopRequestsIgnored`, and only the controller's
  lane host start, with its own latch, calls `laneStopReading`. The drain-protocol notice
  (`GateContentDrainNotice`) is exempt from `stop_requested`, as from
  `draining`, permanently, for the conversation lane's executors (the
  controller's lane host and the CLI's inline pass), which report it as
  `Probe.ContentAdmit`; the prompt hook's in-band pass admits no class, so
  it holds the notice for the lane host. The single reader, the single
  reading chooser and the key owners are guarded by
  `TestStopRequestKeysAreNewKeys` and
  `TestSessionReadsTheStopRequestOnlyInReadStopRequest`; the reader's rule
  by `TestReadStopRequestAgreesWithActiveStop`; the published mode by
  `TestQueryControllerReconcilerModeReadsTheControllersAnswer`.
- Keep production CLI session creation and lifecycle operations on the
  `internal/worker/handle.go` boundary unless root `AGENTS.md` names an active
  exception.
- Do not make ordinary config names or `template:<name>` values act like live
  session targets.

## Startup Fencing Pitfalls

- `Store.Tx` groups rollback writes but cannot read, so it does not fence a
  stale creating snapshot against completion. `WithPendingCreateRollback` and
  `CommitStartedIfCurrent` share `WithSessionMutationLock` with incarnation
  allocation. Keep the fresh read and write inside that lock; provider calls
  and retired-session cleanup stay outside. This is an in-process fence, not
  protection against arbitrary external bead writers.
- The mutation lock's acquire can be timed. `WithSessionMutationLockContext`
  tries the lock first, then waits no longer than its context; a wait that
  ends with the lock still held returns `ErrSessionMutationBusy` (wrapping the
  context's error). A busy result means `fn` never ran and never will, so the
  caller may retry the whole operation. The context bounds only the wait, not
  `fn`. `CommitStartedIfCurrent`, `WithPendingCreateRollback`,
  `RollbackPendingCreateAtomically` (and the `...Under` variants of the first
  and third) take it as their first parameter. A `Manager` or `Store` method that takes a lock-bounding
  `ctx` puts it first and says in its doc comment what it bounds; do not add a
  parallel `...Ctx` variant.
- The reconciler's advisory heal (`ApplyPatchIfLifecycleUnchanged`) decides
  from its tick snapshot. Without conditional writes its re-check fences
  nothing unless the check and the write share the row's mutation lock with the
  in-process operator verbs, so it runs both inside that lock, tried through
  `WithSessionMutationLockContext` with an already-done context: a free lock is
  taken, a held one makes the heal skip (write nothing, `(false, nil)`) without
  waiting. Never make it wait, since `Manager.Suspend` holds the lock through
  the provider stop. Another process's writer (the CLI's managed
  `gc session suspend` takes no mutation lock) is fenced only by a conditional
  writer. `ApplyPatchIfLifecycleUnchangedUnder` runs under a caller-held lock
  and must not take it again (ga-axgaemw).
- `testing/synctest.Wait` does not consider `sync.Mutex.Lock` durably blocked.
  A test that pauses a lock holder on a channel and waits for a competing
  mutex waiter will hang. The context-aware waiter is durably blocked, since
  the lock is a channel, so it can be driven under `synctest` with a
  `context.WithTimeout`. `store_start_fence_test.go` checks the shared lock
  directly at both read and write boundaries instead of relying on timing.
- `awake` is not proof that creation completed: the reconciler can heal a
  running, still-claimed creation to awake before provider Start returns
  (issue #6104 / PR #6105). Rollback must allow that state when its claim and
  identity remain current; active or a cleared claim still blocks rollback.
- A final atomic start commit may reject after the async refresh succeeded.
  Propagate that stale verdict to the provider-aware caller and clean up using
  the attempted identity outside the mutation lock. Persistence failure alone
  must not trigger runtime cleanup.

When a session has a persisted native conversation key and its provider family
resolves transcripts by that key (`transcript.SupportsIDLookup`),
`Manager.TranscriptPathClassified` waits for that exact transcript. Another
conversation in the same work directory is not a fallback. Nor is it for a
provider with a session-id flag whose row is `creating` without a key: that
start's conversation has no key yet. Families without `SupportsIDLookup`
(Codex, Gemini, OpenCode, MiMo, ZCode) still fall back to work-directory
discovery after a keyed miss, as does `SessionLogAdapter.DiscoverTranscript`;
see `SESSION-HISTORY-EXACT-KEY`. OpenCode and ZCode still prefer a keyed hit
(the mirrored export is named for the session key) before that fallback. The
model-usage paths in `internal/worker` resolve OpenCode by key only and never
take the fallback.

## Manual Close During Reconciliation

A desired manual entry retains a previously discovered conversation; it is not
a create request. If a close wins before synchronization refreshes the store,
leave that conversation terminal instead of recreating its alias and runtime.

## Committed conversation recovery

Use the session-owned `PreserveConversationOnRuntimeLoss` policy when healing
a lost process, and `HasStartedManualConversation` where a reaper, a wake
failure or churn would discard a conversation. A committed manual conversation
(`session_origin=manual` or the legacy manual marker, with a `session_key` and
the `started_config_hash` it started under) keeps its provider key and start
configuration through the heal, the stuck-creating, pre-boot and dead-runtime
corpse reapers, wake failure and churn; retry and quarantine accounting still
apply. A failed resume of a conversation the policy covers that has both a
`session_key` and a `started_config_hash`, always-named ones included, returns
`ErrConversationRetained` wrapping the start's own error, its pane output
redacted against the launch's environment, instead of starting fresh, so the
reconciler's resume backstop can still classify a provider refusal from its
pane text; a first start whose key was only preallocated still takes the
stale-key retry. That keeps the session manager from starting it fresh, not
the reconcilers: their wake-failure reset keeps an always-named conversation
only while their transcript probe reads it present, and clears it otherwise.
A committed manual conversation is still reset when a start is prepared if
its provider's keyed transcript is probeable and reads absent (the stale-key
clear, `session.conversation_reset{reason:stale-key}`). While it is kept, the
resume backstop (a refusal, or the third resume-shaped failure in a row) is
its only automatic reset, whatever the provider; `gc session reset` is the
explicit one.
