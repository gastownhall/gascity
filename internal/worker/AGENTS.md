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
  costs one goroutine per key, not one per call.
- The key names what is observed, one session in one city, within a caller
  namespace. Every caller in a namespace must derive the key the same way, or
  the one-in-flight limit no longer holds. `cmd/gc` has two namespaces, so a
  wedged session can hold one goroutine in each: session lifecycle keys by
  session name (`postStartObservationKey`), and the conversation lane's
  executor keys by session bead ID under the `lane` prefix
  (`laneObservationKey`), so a wedged lane probe never blocks a lifecycle
  observation.
- `worker.ObserveShared` is the form for reads several callers make of one key
  at once (turn reads: `CurrentTurn`, `WatchTurns` and its session reload, the
  idle overlay). A call
  made while an observation of the key is outstanding waits for that answer
  within its own context instead of getting `ErrObservationOutstanding`, so
  concurrent healthy readers all get the answer and a wedged session still
  leaks one goroutine. A key is observed through one form, never both, and
  with one type: a joiner of another type's observation gets an unavailable
  error, not its answer. Turn keys (`turnObservationKey`) name the city
  directory and the session, for a `SessionHandle` and a `RuntimeHandle` alike
  (`RuntimeHandleConfig.CityPath`). A turn read made under the session
  mutation lock (the turn-scoped interrupt's check, and its settle after the
  keys) joins no shared read: its session load and runtime probes go through
  `ObserveBounded` under a `locked-` turn key of their own
  (`observeTurnRead`), so a shared read that began before the lock, holding
  the session as it was then, never answers it. Its failure direction is the
  opposite of a watcher's: a watcher leaves a turn open on an idle reading it
  did not get, while the check refuses as unknown (`idleUnknown`), since only
  a busy reading counts as the turn running, and the settle counts it a
  failed read, never the turn's end nor the turn running: a settle whose
  reads all failed, after family waits that passed or could not answer, is
  `session.TurnEndUnreadError`, which withholds a restart on failure. That
  includes `ErrObservationOutstanding`, which every later check's own
  reading gets at once while one an earlier check left wedged is still out.
  A read that decides which turn to act on, such as the lane interrupt's
  target, is `TurnHandle.FreshTurn`: `CurrentTurn` with its session load and
  runtime probes its own, under `fresh-` turn keys, so a shared read that
  began before the turn now running never aims it at the turn before, and a
  store that stalls leaves it unanswered when its context ends. Fresh reads
  of one session do not share: one made while another is in flight, healthy
  or wedged, gets `ErrObservationOutstanding` at once, so its callers
  serialize them per session (the lane does, under its own key). It keeps
  `CurrentTurn`'s failure direction, an idle reading it did not get leaving
  the turn open.

## Turn sources

A session's turns come from the routed runtime's protocol, else from the
transcript of its current conversation, else nowhere (`TurnCapabilities`).
The transcript tier is composed from fixed traits only: the routed backend
runs the agent on this host (`runtime.LocalTranscriptProvider`), the session
has a work directory, and the family has an exact by-key transcript lookup
(`session.HasKeyedTranscriptLookup`). The read resolves only the file the
session key finds and that records that conversation, never the work-directory
fallback `Manager.TranscriptPath` serves for history. A session without a key
yet, or whose transcript is not written yet, reads as
`runtime.ErrRuntimeUnavailable`, not as a different capability.
`TurnCapabilities.Replace` is internal/session's answer
(`session.RuntimeTurnTraits.Replace`): what interrupt_now does to the
session's turn, decided by the predicates interrupt_now runs on. The worker
never derives it from a family table. A `RuntimeHandle` has no deliver-now,
so it reports `none`.

Identity is judged per turn, not from sessionlog's `MissingIDs`
(`turnHistoryFromScan`). A turn without an id whose start lies inside the scan
is `runtime.ErrTurnIdentityUnsupported`. A scan that begins inside a turn (a
window cut in its tail, or a Claude conversation opening with a turn run from
queued input) is the window, not the family: the head end is dropped, and an
open head turn reads as unavailable. `WatchTurns` republishes an error when its
class changes, so a consumer hears unsupported or not found mid-streak. Every
watch read, protocol or transcript and whatever woke it, reloads the session
first, so a session whose row closes reads as not found from the next read on,
even while its runtime still answers or its transcript is still written. A
read's session reload and runtime probes (the protocol history, the idle
snapshot) run within `turnWatchReadBound` and through `ObserveShared`, so a
stalled store or runtime probe reads as unavailable and never holds a watch
past its context. A transcript read's file I/O (resolving the transcript,
`os.Stat`, the follower's read) takes no context: a hung filesystem holds a
transcript watch, and its close on cancel, until that I/O returns.

The transcript tier is never `Lossless`: a read reports at most
`sessionlog.MaxRecentTurns` ended turns from a bounded window, so turns that
end between two reads can fall out of it. `CurrentTurn` scans the transcript's
last 16 MiB (`sessionlog.ScanTurns`) while `WatchTurns` folds what was appended
since its last rescan (`sessionlog.TurnFollower`), so the two can disagree
about a turn that began before that window.

## Keyed deliveries

A request's `DeliveryKey` (`NudgeRequest`, `MessageRequest`) reaches only a
runtime that deduplicates keyed deliveries (`runtime.KeyedNudgeProvider` at
the leaf a composite routes to). Default and immediate delivery carry it;
wait-idle delivery and a message the session queues do not. A `RuntimeHandle`
reports the runtime's receipt on `NudgeResult.Receipt`.

`Redelivery` says an earlier attempt under the key may have landed. The
handle looks the delivery up (`runtime.LookupDelivery`,
`session.Manager.LookupDelivery`) before any effect: for `Message`, before
`Manager.Submit` and so before any turn sighting or interrupt. A delivery the
runtime holds is never sent again: the result is `Replayed`, with the held
receipt on a nudge. A lookup that cannot tell fails the request wrapping
`runtime.ErrNudgeNotSent`, with nothing sent. A request without `Redelivery`
looks nothing up.

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

## Worker sites

A city directory holding `.gc/control-plane.toml` is a worker site: a box
`gc connect` attached to a remote controller's ledgers (DES-CN D12, §5 item
6). On a worker site no `gc` process constructs a provider that can start,
stop, type into or annotate a runtime; dials a controller, supervisor or wake
socket; or reads controller-local state as a fallback. `cmd/gc/control_plane.go`
resolves the plane once per city path per process (`controlPlaneFor`), and the
seams consult it:

- Presence decides. A descriptor that exists but does not decode (a newer
  gc's, say), or one gc cannot stat, fails every command except
  `gc connect *`, `gc doctor`, `gc version` and `gc help`, and every seam
  below treats it as a worker site, so even those commands cannot act or
  dial. Commands that find their city from the session environment rather
  than the context resolver (`currentSessionRuntimeTarget`, the
  `gc nudge drain` fast path) take the same gate (`gateControlPlaneAt`).
- A seam that knows its city decides from that city's own descriptor. Only
  the invocation's own context resolution sets the process flag
  `workerSiteProcess`, which the city-less senders read (`dialGCSocket`,
  and a seam handed an empty city path). A multi-city host, the supervisor,
  never sets it by looking a city up, so one worker-site city cannot turn
  the others into worker sites. Nor does an advisory resolution (eager
  pack-command discovery, `withAdvisoryControlPlane`): it runs before cobra
  parses `--city`, so the city it finds need not be the one the command
  targets.
- `resolveWorkerSpec` returns `guard.New()`, so the runtime under every
  handle a box builds refuses each operation with
  `guard.ErrControllerOwnedRuntime`.
- Socket dials go through `dialGCSocket` (city-less: the supervisor socket),
  which refuses on the process flag, or `dialCitySocket` (one city's wake
  socket, and gc stop's controller socket), which refuses a worker-site
  city's dial and dials a Local city's socket whichever city the invocation
  resolved; both refuse with `errWorkerSiteNoController`. The controller-stop
  client fails closed on that refusal (`controllerStopOutcomeInvalid`): a
  box has no controller socket, and its absence is no proof that the remote
  controller is gone. `pokeController`, `pokeControllerForRestart`,
  `reloadControllerConfig`, `pokeControlDispatch` and `enqueueController`
  return `errWorkerSiteNoController` without reaching a sender, and
  `cityUsesManagedReconciler` answers managed without a dial.
- Session commands key their worker-site arm on the site, not on a nil poke
  (`controllerWillReconcile`), because no poke succeeds in a box.
  `gc session new` and the named and template materializers create a
  deferred session and print the `errWorkerSiteNoController` notice instead
  of enqueueing it; `gc session new` does not attach. They check a
  session's transport against configuration alone
  (`validateSessionTransportAt`): the remote controller's runtime routes the
  session, not the box's guard, so ACP sessions are recorded too.
  `gc session suspend` writes the operator hold and reports managed mode.
  `gc session reset` refuses before it writes
  (`errWorkerSiteResetNeedsController`): its breaker clear needs the
  controller socket.
- `gc runtime request-restart` and a self `gc handoff` record the restart as
  the session bead's marker (`restart_requested` and
  `continuation_reset_pending`), which both reconcilers read. The guard holds
  no runtime flag to set (`restartFlagNotApplicable`), so on a box the marker
  is the whole request and is mandatory for every session, not only a pinned
  one. A handoff by a named session the controller does not restart clears
  only that marker. `gc runtime drain` and `undrain` refuse before they
  resolve the target or write anything (`errWorkerSiteDrainNeedsController`):
  the agent learns of a drain from the runtime drain flag, which only the
  controller's runtime holds, and the row's operator mark only explains an
  ack. `gc handoff --target` refuses the same way
  (`errWorkerSiteHandoffTargetNeedsController`): it stops the target's
  runtime.
- A poke that follows a recorded change prints the `errWorkerSiteNoController`
  notice in place of its poke-failed warning (`noteWorkerSitePoke`): session
  wake, wait, pin and unpin, `gc sling`'s pool wake poke, `gc convoy poke`,
  `gc runtime drain-ack`, `gc runtime request-restart` and a self
  `gc handoff`. None claims it poked a controller.
- No nudge text leaves a box. The queue `openNudgeQueue` builds is the
  box-local state file, which the remote controller never reads, and the
  guard types into no runtime. `gc session nudge` and `gc session submit`
  refuse before they resolve the target or write anything
  (`errWorkerSiteDeliveryNeedsController`): `gc session wake` records the
  wake, and `gc mail send` carries the text. The courtesy nudges of
  `gc sling` and mail notify queue nothing and start no poller: they record
  only the session's wake (`requestWorkerSiteNudgeWake`, never over an
  operator hold), since the routed bead or the mail is on the ledger either
  way, and their notice (`workerSiteNudgeNotice`) promises the remote
  controller's reconcile only when a wake was recorded. Mail notify that
  records none (a held or running recipient, or one with no session bead)
  returns `errWorkerSiteNotifyNotDelivered`, so `gc mail` reports `notified`
  false. The hook-claim continuation self-nudge
  (`hookContinuationNudgeEnqueue`) queues nothing. This holds until
  `openNudgeQueue` can select the beads queue (ga-w0hgvz), which CN-3b's
  mint requires of a connect-path box.
- The session catalog (`workerSessionCatalogWithConfig`) reports the
  persisted state: the guard's `IsRunning` is not the remote runtime's
  liveness, so the live overlay would show every active session asleep.
- Fallback reads of controller-local state refuse with
  `errWorkerSiteLocalState`: the local events log, which holds only what the
  box wrote (`gc events`, and `gc analyze-reliability` on the city it
  resolves; a file named by `--events` is still read), and any class whose
  configured binding is not the binding of the descriptor ledger whose
  `classes` list it. `gc status`' store-health block opens no events log on
  a box (`openStoreHealthEvents`), so it reports no last maintenance. The
  one-shot storage funnel (`resolveCLIStorageRoutes`) takes that verdict
  before it opens or creates a binding, as a standing refusal, so the by-id
  resolver and the graph binding refuse too while work ids still resolve to
  work.
- `gc start`, `gc restart`, `gc stop`, `gc register` (and `gc init`'s
  registration), `gc supervisor run`, `start` and `install` refuse with
  `errWorkerSiteRefusesController`, and the supervisor's per-city start
  (`startOneCity`) refuses a worker-site city whatever city it was started in.
  The supervisor files that per-city refusal as an init failure, with no
  category of its own: it lists the city as `init_failed` with the refusal as
  its error, retries it on the init-failure backoff, and fails a pending
  `city.create` request with `city_init_failed`.
- Under v2 the drain-ack's row commit is the ack; the guard's refusal to
  mirror it into the env flag is a warning, and it reports that the remote
  controller stops the session at its next reconcile. Under legacy there is
  no in-box drain-ack (D-DRAINACK) and the guard's refusal stays loud.

Not covered here: a work store on `[beads] provider = "file"` or `exec:`
opens on a worker site. No connect-path box has one, because CN-3b's mint
refuses such a work binding (`mint_binding_unreachable`). The class check
compares binding names only, not provider or endpoint: a box `[storage]`
binding that carries a descriptor ledger's name opens whatever the box
configures for it, a local SQLite file or a managed bd that starts a local
Dolt server included. CN-3b's mint is what keeps a box's bindings on the
descriptor ledgers.

New code on a worker-reachable path must take these seams, not dial or
construct a provider directly. These still dial directly and are converted
by CN-1b2:

- `sendControllerCommandWithTimeouts` (`cmd/gc/controller.go`), and with it
  the callers that send through it directly, such as `controllerAlive`.
- `supervisorAliveAtPathUntil`, `stopSupervisorViaSocketJSON` and
  `reloadSupervisorJSON` (`cmd/gc/cmd_supervisor.go`). The last two sit
  behind `runningSupervisorSocket`'s probe, as does `reloadSupervisorNoWait`,
  whose own dial already goes through `dialGCSocket`.

CN-1b2 also converts the commands that still open the box's own events log
as the city's, which `gc events` refuses (`refuseLocalEventsOnWorkerSite`):
`gc order check` and `gc order run`, whose event gates and cursors read it
(`openCityEventsProvider`, `cmd/gc/cmd_order.go`), and the
`gc convoy control --serve` watcher (`openCityEventsReader`,
`cmd/gc/dispatch_runtime.go`).

Worker sites exist in production only once the sidecar is written (CN-2),
which waits for CN-1b2.

## Injected lookups

The worker reads work beads and other state it does not own through lookups
injected as func values (for example `FactoryConfig.ResolveBead`), and never
enumerates stores itself.
