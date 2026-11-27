# Session lifecycle history: storage proof and integration inventory

Status: prerequisite evidence for [#9537](https://github.com/gastownhall/gascity/issues/9537).
Lifecycle capture and `gc session history` are not implemented by this slice.
The inventory describes the carrier's base, `6dce31bbb68e31a95482815050a0ace951be47ce`.

An export must distinguish an empty history from missing evidence. Current
session rows and best-effort events cannot provide that distinction. History
needs a retained journal coupled to the session's incarnation fence, an ordered
commit cursor, and explicit gaps for anything the participating writers cannot
prove. Exact physical process lifetimes are outside this proposal.

## Storage protocol proposed for review

Use the existing `beads.GuardedBatcher` capability, resolved through
`GuardedBatcherFor` on the session-class store. Do not reach around a wrapper or
substitute `Store.Tx`: the latter does not promise cross-backend atomicity or
revision-guarded reads.

One lifecycle commit contains three items in one guarded batch:

1. Create an immutable retained journal record under a stable transition ID.
2. Update a scope cursor from explicit sequence N to N+1, guarded by the cursor
   row's revision.
3. Update the session generation/lifecycle/lease fields, guarded by the revision
   read when validating the incarnation and lease.

Revisions are opaque equality tokens. N is an application sequence stored on the
cursor; neither revisions nor timestamps supply its ordering. Different sessions
share that scope's cursor, so concurrent commits cannot allocate the same N.
A losing writer re-reads and validates both premises before retrying. A pinned
create and stable transition ID resolve a lost acknowledgement by read-back;
replaying the batch never consumes another sequence or mints again.

Journal rows use the retained storage tier and no session label or parent edge.
Session deletion must leave them and the cursor intact. They remain subject to
explicit journal retention; storage-tier separation alone does not protect
against arbitrary deletion or prove completeness.

The shared guarded-batch conformance suite now exercises:

- A stale session fence as the final item: neither the earlier record create nor
  cursor update escapes the refused transaction.
- A successful triple write, replay refusal, and journal survival after session
  deletion.
- Two different sessions racing for one cursor: exactly one receives sequence 1;
  the loser leaves its session and record untouched, then receives sequence 2
  only after reading the committed cursor.

`TestHistoryStorageCommitSurvivesReopen` independently closes and reopens file and
SQLite stores and checks all three persisted rows. These are storage proofs;
they do not run the proposed lifecycle protocol or certify a coverage barrier.

## Backend capability boundary

| Backend or wrapper | Atomic fence + record + cursor | Durability / remaining evidence |
|---|---|---|
| `MemStore` | Shared conformance proof | Volatile; ineligible for durable history |
| `FileStore` | Shared conformance and reopen proof | One locked reload/apply/save/rename; no power-loss acknowledgement guarantee established by this proof |
| Writable revision-bearing `SQLiteStore` | Shared conformance and reopen proof | One `BEGIN IMMEDIATE` transaction; process/power-loss fault injection remains before a durable-history claim |
| Native Dolt with a batch applier | Shared conformance on a contract double and a passing real-storage integration proof | Real facade transaction evidence must be distinguished from the unit double; restart/fault injection remains |
| `CachingStore` over capable memory/file/SQLite/native leaves | Shared proof reads back through the wrapper | Capability and durability follow the backing store; ambiguous errors require fresh read-back |
| `ProxiedStore` with a capable native write leaf | Shared proof on a double with separate read/write views | Does not certify the default bd write leaf or a real proxy restart |
| `BdStore`, exec provider | No guarded-batch capability | Unsupported for the proposed atomic protocol, even if a single-row CAS is available |
| Default proxy with a bd write leaf; demoted proxy | Capability discovery refuses | Unsupported; never bypass the proxy to write the read-only native leaf |
| Read-only/legacy-revisionless SQLite, `DoltliteReadStore` | Capability discovery refuses | Unsupported |
| Typed class stores | Forward capability explicitly | The session, record and cursor must reside in the same physical store/namespace |

The existing unsupported-store discovery tests remain authoritative for the
refusals in this table. A cache or in-memory double passing the proof does not
make its backend durable. Capability discovery must report unsupported rather
than starting a partially fenced journal.

## Lifecycle integration inventory

Every row below currently writes **no durable lifecycle journal**. The proposed
participation column distinguishes the intended journal integration from paths
that must hold coverage unknown until they participate. A best-effort event is
never a substitute for either.

| Path | Existing boundary | Proposed participation / explicit gap |
|---|---|---|
| Manual first start | `internal/session/manager.go`: `createStarted`, `createdIncarnation`, provider `Start`, `confirmStartedRuntimeMetadata` | Create session + pending attempt + cursor atomically under a pinned session ID; confirmed/failed/uncertain result under that incarnation. Current minted-ID `Create` needs integration, not a later journal append |
| Pending session creation | `internal/session/create.go`: `CreateSessionInfo`; pending-create preparation and rollback in the session store | Record creation without asserting process start; the later mint owns the attempt. Rollback must not erase it |
| Manual wake / resume | `internal/session/chat.go`: `ensureRunningOn`, `mintStart`, `ensureRunningRuntimeOnly` | Couple `MintIncarnation` to pending record/cursor; distinguish convergence on an already-running process from a launch |
| Stale resume-key fallback | `internal/session/chat.go`: `retryFreshStartAfterStaleKey` | Each retry uses its own mint/attempt; failed readiness with a possibly live process remains uncertain |
| Confirmation and unmint | `internal/session/incarnation.go`: `ConfirmIncarnation`, `unmintIncarnation`; `chat.go`: `confirmLiveSessionState` | Fence the immutable transition to the attempted incarnation; restoring mutable generation must not remove attempt identity or allow a transition-ID collision |
| Legacy controller wake | `cmd/gc/session_wake.go`: `preWakeCommit`; `session_lifecycle_parallel.go`: `startPreparedStartCandidate`, commit/restore/capacity-refusal paths | `preWakeCommit` reaches `MintIncarnation`; resolved launches reach `worker.Handle`. Pair commit/undo with history, including stale results and takeover |
| v2 controller launch | `cmd/gc/reconcile_effect_launch.go`: direct `leaf.Start`; preparation, `commitStep` and settlement | Must join the same pending-intent protocol before the direct effect and fenced result after it; journal gaps until integrated |
| Ordinary start commit | `internal/session/store.go`: `CommitStartedIfCurrentUnder`; controller start settlement | Co-commit confirmation only while lease/incarnation is still current; a stale completion cannot journal a newer incarnation |
| Manual suspend/idle/shutdown stop | `internal/session/manager.go`: `Suspend`, `tearDownRuntimeForSuspend`; worker shutdown/idle variants; worker stop variants | Durable stop intent before side effect; observation-based gone or uncertain afterward. `SleepPatch` is not exact exit evidence |
| Operator kill | `internal/session/manager.go`: `Kill`; `kill_fence.go`: `KillFencedReport`, `KillRuntimeGone`; `internal/worker/operator_kill.go` | Record request under kill/incarnation fence; a successful cleanup call alone does not establish a physical exit time |
| Manual close | `internal/session/manager.go`: `CloseDetailed`; `Store.CommitClose` | Keep stop intent distinct from terminal row commit; a stop followed by failed/stale close leaves an open uncertain result |
| Replacement / interrupted turn restart | `internal/session/submit.go`: `interruptAndSubmitLocked`, `restartAndSendLocked`; `stop_turn_with.go`: `restartAfterFailedInterruptLocked` | Record stop and following mint as separate linked facts; soft turn interrupts that do not replace the process are not incarnation ends |
| Start cleanup | `manager.go`: failed-create and leased-liveness cleanup; `chat.go`: failed/converged starts | Link cleanup to attempted incarnation, preserve uncertainty when the process cannot be attributed or absence cannot be confirmed |
| Legacy stale-start / pending rollback cleanup | `cmd/gc/session_async_start_drift.go`, `session_lifecycle_parallel.go`: `stopStaleAsyncStartRuntime`, `pendingCreateRuntimeClearedForRollback` | Record attribution, requested stop and absence observations; rollback cannot silently restore history |
| Legacy fenced stop / reaping | `cmd/gc/session_beads.go`: `cleanDeadRuntimeCorpse`, `stopStillBoundClosedRuntimeLeased`, `stopRuntimeBeforeSessionBeadMutationInfo`, close/reap helpers | Journal under the same fence where a session can be identified; otherwise an unattributed-runtime coverage gap |
| v2 destructive effects | `cmd/gc/reconcile_session_fence.go`: `stopFenced`, `confirmStopped` | Journal stop intent/result under its existing verdict and live identity check; unknown probes leave uncertainty |
| Ownerless and detached process cleanup | `cmd/gc/runtime_ownerless_reap.go`: `reapOwnerlessRuntime`; `session_beads.go`: `sweepProcessTableOrphans` | Explicit unattributed-runtime/process gap; never invent a session/account/generation |
| Provider swap | `cmd/gc/provider_swap.go`: `stopProviderSwapRuntimes` directly stops old leaves and emits events without row mutation | Add scoped intent/result participation; otherwise a provider-swap gap, including partial/failed stops |
| City stop and controller shutdown | `cmd/gc/controller.go`: graceful-exit and survivor-stop passes; `cmd_stop.go`: stop waves/fallback | Cover direct cleanup, graceful observed disappearance, unknown states and row-less targets; stop intent precedes any signal/kill |
| Bead-less runtime handle | `internal/worker/runtime_handle.go`: `StartResolved`, `stopUnderLease`, `Stop`, `Kill`, `Close` | No durable session incarnation exists: explicit runtime-only coverage gap, or a separately approved migration into the journal scope |
| Recovery / discovery / heal | Controller reconciliation in `session_reconcile.go`, `session_reconciler.go`; session heal and rollback store methods | Activation/restart barrier plus carry-in of discovered live or possibly live runtimes; original start/account/end remain unknown |
| Provider adapters and composites | `internal/runtime`: auto/hybrid routing; SSH start-failure cleanup; provider-internal process setup/cleanup | One semantic operation at its owning caller, no double counting wrapper delegation; unobservable provider-side side effects remain uncertain |
| Account reference mutation | `usage_account_ref`, defined by the open [#9534](https://github.com/gastownhall/gascity/pull/9534) | Snapshot presence and opaque value at mint. Mid-incarnation/external mutation needs an explicit fenced switch protocol or account-attribution gap |
| Session/wisp prune and journal compaction | `Manager.PruneDetailed`, bead cleanup, backend retention | Independent retained journal with atomic floor/checkpoint; missing records and crossing the floor produce gaps |

Inventory searches included provider `Start`/`Stop`, `StopForCleanup`,
`MintIncarnation`, confirmation/commit, unmint/restore, rollback and destructive
fences in `cmd/gc`, `internal/session`, `internal/worker` and `internal/runtime`.
OS child processes for controller/order/credential infrastructure and timer
`Stop` calls are not agent-runtime incarnations. Re-inventory against the final
implementation base before claiming all writers participate.

## Decisions and evidence still required

The issue asks maintainers to approve opt-in coverage and the transaction/recovery
design before implementation. The proposed mode remains off by default and never
blocks a launch on a history failure. Approval of this three-item guarded-batch
protocol, backend durability scope, and recovery handling is still required.

A logical committed cursor proves record order only. A later implementation
must register/fence every in-scope writer, resolve or gap all earlier intents,
and commit a barrier in that same stream before publishing any completeness
claim. Wall-clock ordering, missing writers/scopes, corruption/deletion,
backend outages, late observations and unbounded endpoints must remain explicit.

Before any export claim, add the failure-injection, takeover/stale-writer,
activation/carry-in, account immutability, retention/checkpoint, stable snapshot,
clock regression and missing-scope proofs listed in #9537. File/SQLite reopening
and the real native transaction proof are prerequisites, not those acceptance
proofs. A journal failure that cannot itself persist a gap must freeze the last
proven barrier; recovery records the conservatively unknown span. No watermark
may be synthesized from `now` or the largest recorded timestamp.

## Validation of this prerequisite slice

The guarded-batch conformance tests pass on all unit-tier backends/wrappers,
and both new `session_history_*` legs pass under
`TestRealNativeDoltStoreGuardedBatchConformance` with actual native storage
(no skip). The dedicated file/SQLite reopen test passes as well.

```sh
bazel test //internal/beads:beads_test \
  --test_filter='Test.*GuardedBatchConformance|TestHistoryStorageCommitSurvivesReopen|TestGuardedBatcherFor|TestProxied.*Guarded|TestSQLiteStoreGuardedBatchRefuses' \
  --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all
bazel test --config=integration //internal/beads:beads_test \
  --test_filter='^TestRealNativeDoltStoreGuardedBatchConformance' \
  --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all
bazel test //test/docsync:docsync_test
```

This does not pass the full feature's acceptance plan: lifecycle participation,
coverage barriers, the exporter and failure recovery are still unimplemented.
