## Summary

`internal/runtime/tmux/tmux.go`'s `NudgeSession` discarded the `confirmed`
bool from `submitEnterAndConfirm` (`if _, err := submitEnterAndConfirm(...);
err != nil {...}`) and always reported clean delivery (`nil`) whenever the
Enter send itself didn't error — even when the busy-confirm loop burned its
full budget and never observed the agent go busy, i.e. the message may still
be sitting drafted-but-unsubmitted in the pane. This is ra-3x46cy finding 1
(PROVEN by code read): the queue-ack path (`tryDeliverQueuedNudgesByPoller`)
and the idle-claim backstop's attempt counter both treat a nil error as
"delivered," so an unconfirmed submit was silently swallowed instead of
retried — the root cause of the 15-minute nudge stall the bead observed live.

## Fix

`NudgeSession` now captures `confirmed` and, when false, returns a typed
sentinel error (`ErrNudgeSubmitUnconfirmed`, wrapped with the session name)
instead of nil. Callers already propagate `NudgeSession`'s error verbatim up
through `Provider.Nudge`/`NudgeNow` (no wrapping in between), so a
retry-capable caller now correctly sees a non-nil error and does not ack the
queue item or advance its attempt counter. `delivered` (which gates the poke
timestamp used for session-activity discounting) is still set on any
error-free Enter delivery, confirmed or not — that accounting is unrelated to
this fix's scope (see ra-3x46cy finding 3).

Two pre-existing tests turned out to rely on the exact bug this patch fixes —
both nudge a `claude`-provider pane whose fake command (`cat -v`) can never
emit a busy indicator, so `confirmed` was always false and they only passed
because `NudgeSession` used to swallow that into `nil`:
- `TestNudgeSessionSkipsEscapeForClaude` (tmux_test.go)
- `TestNudgePokeRealTmux`'s "never-busy claude nudge" subtest
  (nudge_poke_integration_test.go, gated behind `GC_TMUX_INTEGRATION=1`)

Both now explicitly tolerate `ErrNudgeSubmitUnconfirmed` as the correct,
expected outcome for their never-busy fake panes, with a comment explaining
why.

## Test

New: `TestNudgeSessionReturnsUnconfirmedErrorWhenNeverBusyForClaude`
(nudge_submit_confirm_integration_test.go) — a fake `claude`-family binary
that never prints a busy indicator (`GC_TEST_BUSY_AFTER=100`, far beyond the
confirm budget). Proven fail-before (`NudgeSession` returned nil) / pass-after
(`errors.Is(err, ErrNudgeSubmitUnconfirmed)`).

```
go test -tags integration ./internal/runtime/tmux/... -run 'TestNudgeSession|TestSubmitEnterAndConfirm' -v
... (all PASS, 20 tests)

go test ./internal/runtime/tmux/...
ok  	github.com/gastownhall/gascity/internal/runtime/tmux

GC_TMUX_INTEGRATION=1 go test -tags integration ./internal/runtime/tmux/... -run TestNudgePokeRealTmux -v
--- PASS: TestNudgePokeRealTmux (all 5 subtests)
```

Full integration suite (`GC_TMUX_INTEGRATION=1 go test -tags integration
./internal/runtime/tmux/...`) also run; two failures are pre-existing,
unrelated environment flakes independent of this change:
`TestNudgeSessionConfirmsSubmitForClaude` (passes reliably in isolation —
reruns clean 3/3 — flakes only under the full batch's concurrent tmux
sessions) and `TestGetKeyBinding_CapturesDefaultBinding{,WithArgs}` (depends
on this machine's default tmux key-binding config, unrelated to nudging).

Source bead: ra-3x46cy (finding 1).
