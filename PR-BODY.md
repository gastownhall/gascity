## Summary

`internal/runtime/tmux/tmux.go`'s `NudgeSession` never cleared pending pane
input before pasting a new message — unlike `SendKeysReplace`, which sends
`C-u` first specifically for this reason. This is ra-3x46cy finding 2 (strong
hypothesis, with the code's own doc comment naming it as a known-unaddressed
upstream defect: `sendKeysLiteralWithRetry`'s comment cites upstream #1216
"Nudge delivery reliability (input collision — NOT addressed here)"). If an
earlier nudge left undelivered text sitting in the input box (the exact
failure mode of finding 1 — a lost submit Enter), a later, unrelated nudge
attempt against the same session would paste more text on top of it instead
of replacing it, producing one unsubmittable multi-fragment draft. The live
specimen showed three independent nudge subsystems (an idle-claim backstop,
two queued alias-nudges) hitting the same single-pane session inside one
incident window — genuine input-collision exposure.

## Fix

`NudgeSession` now sends `C-u` (with a short settle delay) immediately before
the literal paste, mirroring `SendKeysReplace`'s clear-before-paste pattern.
This is option (a) from the bead's fix shape — the cheapest, most surgical of
the two options offered, and upstream-worthy per the code's own citation of
#1216.

## Test

New: `TestNudgeSessionClearsPendingInputBeforePaste`
(nudge_clear_before_paste_integration_test.go). Types a leftover,
unsubmitted draft into a real tmux pane (`cat -v`, which echoes back exactly
what was submitted once Enter lands), then calls `NudgeSession` with a fresh
message and asserts the echoed line is the fresh message alone — not the
leftover draft concatenated with it. Proven fail-before (echoed
`leftover-draftfresh-message`) / pass-after (echoed `fresh-message`).

```
go test -tags integration ./internal/runtime/tmux/... -run TestNudgeSessionClearsPendingInputBeforePaste -v
--- PASS: TestNudgeSessionClearsPendingInputBeforePaste
```

Full non-integration package suite, the broader nudge/send-keys integration
set, and the `GC_TMUX_INTEGRATION=1` poke-timing suite all still pass with no
new failures:

```
go test ./internal/runtime/tmux/...
ok  	github.com/gastownhall/gascity/internal/runtime/tmux

go test -tags integration ./internal/runtime/tmux/... -run 'TestNudgeSession|TestSendKeys|TestSubmitEnterAndConfirm' -v
... (all PASS, 24 tests)

GC_TMUX_INTEGRATION=1 go test -tags integration ./internal/runtime/tmux/... -run TestNudgePokeRealTmux -v
--- PASS: TestNudgePokeRealTmux (all 5 subtests)
```

Note: this clone is independent of the sibling
`fix/nudge-confirm-not-discarded` branch (finding 1) — both branch from the
same `main` and land as separate small PRs per the rung's instructions, so
this diff does not include finding 1's `ErrNudgeSubmitUnconfirmed` change.

Source bead: ra-3x46cy (finding 2).
