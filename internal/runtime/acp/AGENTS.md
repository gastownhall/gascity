# ACP Startup

- The startup sentinel reports alive before the ACP handshake completes.
  Seed ownership through `runtime.SplitEnvForMetaSeed` before publishing the
  sentinel, not after handshake. Child environment alone cannot satisfy
  `GetMeta`, which reads private sidecars.
- A cancelled startup owns its reservation until its process and writes drain.
  `Stop` cancels and waits for that completion. Releasing the name early lets
  old socket or metadata cleanup destroy a retry's state.
- The per-name lifecycle file is a kernel-held coordination lock, not a process
  status record. Keep its inode across starts; unlinking it splits contenders
  across different locks. It also fences different Provider instances while a
  failed startup has no live socket but still has cleanup pending.
- Closing the Unix listener unlinks its socket. Do not unlink the socket again
  after Close: another provider may already have bound a replacement.
- A dead in-memory connection can coexist with a live replacement owned by
  another provider. Its stale `Stop` must not remove the replacement's metadata.
  It must still evict its own dead entry and report success: a tracked dead
  entry makes `IsRunning` short-circuit on it and call the live replacement
  dead, and a non-gone error on that steady state is re-logged every tick by
  callers that branch only on `IsSessionGone`.
- Release the lifecycle lock after committing the real connection, before the
  initial nudge. A blocked stdin write needs Stop to acquire that lock and close
  the pipe. Bind initial delivery to the committed connection, not another
  name lookup that could select a replacement after a concurrent stop/retry.
- Handshake cancellation must close the startup-owned stdin pipe as well as
  cancel response waits. Otherwise a backpressured write holds the lifecycle
  lock indefinitely and prevents Stop from killing the process. The handshake's
  `context.AfterFunc` callback closes that pipe without locks; disarm and join
  it before transferring the connection so late cancellation cannot close a
  successful session or interfere with retry cleanup.

# ACP Turn Events

- Take the turn-event hub lock on its own or while holding `sc.mu`; never
  acquire `sc.mu` while holding it. Turn events are published under `sc.mu`,
  so the reverse order deadlocks against a publisher.
- Publishing never blocks on a subscriber. An event that does not fit in a
  subscriber's channel is dropped and counted; a blocking send would stall
  the JSON-RPC read loop or a Nudge behind one slow consumer.
- Log dropped events only after releasing `sc.mu`: take the count under the
  lock with `takeLostTurnEventsLocked`, then call `logLostTurnEvents`. A
  stderr write under the lock lets a blocked stderr stall every user of the
  connection. The same holds for `stdinMu`: `setActivePrompt` hands its
  count back instead of logging it, and Nudge logs it once the prompt write
  has released `stdinMu` (`TestTurnEventsOpeningATurnHandsBackTheLoss`).
- The stream is best-effort; the session transcript is the durable record.
  Consumers must tolerate a started event without its completed event, and
  the reverse.

# ACP Interrupt

- Interrupt's session/cancel and fallback SIGINT are keyed to the idle channel
  of the turn it was called for, never to the current turn. Nudge starts the
  next turn only after that channel closes, so a closed channel means the turn
  is over and both are skipped. A cancel guard that re-reads `sc.idleCh` or
  calls `isBusy()` instead sees the next turn and cancels it.
  `TestInterruptCancelNeverFollowsTheNextPrompt` covers both a finished and an
  in-flight next turn.
- Interrupt must stay bounded when the agent stops reading stdin, so it
  never takes `stdinMu` itself: its writes run on a goroutine, and
  session/cancel checks the channel under `stdinMu` together with its write.
  The SIGINT re-check and the signal run together under `sc.mu`
  (`TestInterruptSendsNoSIGINTAfterTheTurnEnds`,
  `TestInterruptTurnSIGINTNeverReachesTheNextTurn`): a turn ends and the next
  opens only under `sc.mu`, so the signal goes out while the turn is still
  the agent's and before the next prompt is written. `kill(2)` does not
  block, so holding `sc.mu` across it is bounded; never hold `sc.mu` across
  stdin I/O. The residual is the agent's own: how long it takes to act on a
  SIGINT it received during the turn.
- Nudge holds `stdinMu` from `setActivePrompt` through the prompt write
  (lock order `stdinMu` before `sc.mu`; nothing takes `stdinMu` under
  `sc.mu`). A turn's record is published from the moment it opens, so a
  session/cancel aimed at it waits for its prompt instead of reaching the
  agent first and being ignored
  (`TestInterruptTurnCancelFollowsItsOwnPrompt`).
- A turn that ends on its own cancels the permission requests it still
  holds: `replyCanceled` writes those replies from a goroutine that takes
  `stdinMu` separately, so they are unordered relative to the next prompt,
  and Nudge can write that prompt first.
- `InterruptTurn` is the turn-scoped path (`turn_identity.go`). In one
  critical section of `sc.mu` it checks that the named turn is
  `currentTurn` and takes that turn's idle channel and held permission
  requests; on a mismatch it returns `*runtime.TurnNotCurrentError` and
  sends nothing, not even permission replies. The cancel, settle and SIGINT
  fallback then run on a goroutine through `cancelAndSettle`, the loop
  `Interrupt` shares, keyed to that channel. A check and a channel taken in
  separate critical sections can cancel the next turn;
  `TestInterruptTurnNeverCancelsTheNextTurn` interleaves the two 1,000 times
  (probabilistic: it catches a split critical section in roughly 4 of 10
  `-race` runs and essentially never without, and no CI lane runs `-race`),
  and
  `TestInterruptTurnCancelNeverFollowsTheNextPrompt` pins the window after
  the critical section deterministically.
- ACP still reports `keystroke` mode, so `internal/session` never consults
  this fence: `StopTurn` and `interrupt_now` keep using `Interrupt`.
- Another process asks the owner over the control socket: `turn` answers
  `idle`, `running <id>`, `starting`, `gone` or `unavailable`;
  `interrupt-turn <id>` answers `ok`, `stale <id>`, `idle`, `starting`,
  `gone` or `unavailable`. Only a session that is gone answers `gone` (a
  caller reads it as the turn having ended); any other failed read answers
  `unavailable`. `starting` reads as `ErrSessionInitializing` wrapping
  `ErrRuntimeUnavailable`. An id that is not `runtime.IsTurnIdentity`, or
  holds a space, reads as not current without the `interrupt-turn` command. An owner that
  predates them closes the connection unanswered; the caller reports
  `runtime.ErrTurnFenceUnavailable` and never falls back to the untargeted
  `interrupt` command. A turn read that fails is an error, never idle.
- The connection keeps the last `turnHistoryLen` (8) finished turns for
  `TurnHistory`; only the owner has them, so another process's
  `TurnCapabilities` are not lossless.
