## Summary

Makes the tmux nudge carrier's post-paste submit action a declarative,
per-provider-family key sequence (the design proposed in upstream
gastownhall/gascity#4706), instead of a single hardcoded "Enter" call site.
Zero behavior change for every provider today — this is infrastructure, not
a claude-specific fix (see "What this patch does NOT do" below).

## Problem

ra-oudpha's dispatch note: "the idle-nudge composer STILL types-without-
submitting into claude TUI sessions... This is upstream #4706's exact shape
(declarative per-provider nudge submit-key sequence)." This recurred *after*
gascity#5012 (propagate the unconfirmed-submit error instead of a false
`nil`) and #5013 (clear pending input before pasting) had already landed —
so the failure is now honestly reported (no false "delivered" acks) but
still not resolved.

#4706 itself documents the concrete, evidenced version of this problem for
codex: a k8s codex agent's first turn never started because a `send-keys -l
<text> Enter` burst gets buffered by codex's TUI as a paste, and the
trailing `Enter` is swallowed as a composer newline instead of triggering
submit — codex's actual submit sequence is `Escape` then `Enter`. The
proposed fix is to stop hardcoding per-provider key heuristics in Go and
make the submit sequence declarative per provider family instead.

## Investigation for claude specifically

I could not identify a wrong key as the cause of the claude-specific
residual, and did not implement an unverified fix for it — reporting per the
bead's "report if not obvious" instruction rather than guessing:

- #4706 itself specifies claude's default submit sequence as plain `Enter`,
  which is exactly what this fork already sends
  (`providersSkippingEscapeBeforeEnter` already includes `"claude"`, so no
  spurious Escape is synthesized before it either).
- The mechanic's own investigation (ra-3x46cy) explicitly ruled out a
  busy-indicator false negative for the specimen that motivated this bead:
  the composer text was observed **visibly still sitting unsubmitted**, not
  silently-submitted-but-unconfirmed — so this isn't `paneContainsBusyIndicator`
  missing a fast turn.
- `submitEnterAndConfirm` already retries Enter up to 3 times with busy
  polling between sends (~1.8-2.4s budget) before giving up honestly via
  `ErrNudgeSubmitUnconfirmed`.

Pinning the actual cause needs a live trace against a failing session,
which this fork-patch pass does not have (the city this bead is scoped
against is live and read-only for this pass; ra-3x46cy's own investigator
reached the identical conclusion trying to bisect the *dispatch*-side gate:
"I could not safely bisect... without adding a trace line and restarting
the supervisor — out of scope for a read-only pass").

## What this patch does

- `internal/runtime/tmux/tmux.go`: new `nudgeSubmitKeySequences map[string][]string`
  (provider family → ordered tmux key names) and
  `defaultNudgeSubmitKeySequence = []string{"Enter"}`, with a lookup
  (`nudgeSubmitKeySequenceForFamily`) and a target-resolving wrapper
  (`nudgeSubmitKeySequence`, mirroring how `submitVerifyEligible` and
  `shouldSendEscapeBeforeEnter` already resolve provider family from the
  `GC_PROVIDER` pane env var with a process-name-sniff fallback).
- New `sendNudgeSubmitSequence(target string, keys []string) error` sends
  each key via `tmux send-keys`, pausing `nudgeSubmitKeySettle` (100ms)
  between keys in a multi-key sequence.
- `NudgeSession` and `NudgePane` both now resolve and send the target's
  declared sequence instead of a hardcoded literal `"Enter"` string, for
  both the confirm/retry path (`submitEnterAndConfirm`, renamed its
  `sendEnter` param to `sendSubmit` — a rename only, same injected-callback
  shape) and the historical best-effort fallback path.
- **`nudgeSubmitKeySequences` starts empty.** No family (including
  `claude` and `codex`) has an explicit entry, so every provider keeps
  exactly its current single-Enter behavior. This is deliberately scoped as
  pure infrastructure: I did not add codex's `["Escape", "Enter"]` entry
  from #4706 in this patch, since validating it against codex's actual TUI
  is outside a claude-focused bead's scope and I have no live codex session
  to verify against — flagging it as a natural, low-risk follow-up once
  someone can test it.
- Once a live trace pins claude's actual requirement (whatever it turns out
  to be — a different key, a double-Enter, more settle time), landing it is
  a one-line table entry plus a test, not another pass through
  `NudgeSession`'s delivery mechanics.

## Testing

- `TestNudgeSubmitKeySequenceForFamilyDefaultsToEnter` /
  `TestNudgeSubmitKeySequenceForFamilyHonorsTableEntry`
  (`internal/runtime/tmux/nudge_submit_key_sequence_test.go`, no build tag):
  pure unit tests on the lookup table and its default fallback.
- `TestSendNudgeSubmitSequenceSendsEachKeyInOrder` /
  `TestNudgeSessionUsesDeclaredSequenceForProviderFamily`
  (`internal/runtime/tmux/nudge_submit_key_sequence_integration_test.go`,
  `//go:build integration`, gated on `hasTmux()`): live-tmux tests using a
  `cat -v` pane (which echoes control bytes as visible caret notation, e.g.
  Escape → `^[`) and a throwaway `testfam` provider family registered only
  for the test, proving both the low-level primitive and `NudgeSession`
  itself actually emit every key in a declared multi-key sequence, not just
  the last one — this is the real wiring a future claude/codex-specific fix
  would depend on, not just that the lookup function returns the right
  slice.
- Fail-before proven: temporarily made `sendNudgeSubmitSequence` send only
  the sequence's last key (simulating broken multi-key wiring) — both
  integration tests failed (`CapturePaneAll missing Escape...`). Restored →
  both pass.
- `go test ./internal/runtime/tmux/...` (no tag) — all PASS.
- `go test -tags integration ./internal/runtime/tmux/... -run 'TestNudgeSubmitKeySequence|TestSendNudgeSubmitSequence|TestNudgeSessionUsesDeclaredSequence'` — all PASS.
- `go build ./...` and `go vet ./...` — clean.
- Full `GC_TMUX_INTEGRATION=1 go test -tags integration ./internal/runtime/tmux/...`: only the two pre-existing, machine-config-dependent failures already documented on ra-3x46cy's earlier landing note — `TestGetKeyBinding_CapturesDefaultBinding{,WithArgs}` ("depends on this machine's default tmux key-binding config") — everything else, including `TestNudgeSessionSkipsEscapeForClaude`/`TestNudgeSessionSkipsEscapeForOpenCode` and the full nudge-submit-confirm suite, PASS with no regressions.

## Scope

`internal/runtime/tmux/tmux.go` (declarative table + two new methods + the
`sendEnter`→`sendSubmit` rename inside `submitEnterAndConfirm`, `NudgeSession`,
`NudgePane`), plus the two new test files above. No config/TOML surface
added — the table is a Go-level declarative source of truth today, matching
how the existing `providersSkippingEscapeBeforeEnter` per-provider list is
already Go-level rather than threaded through `config.City`; that's a
bigger, separate change (full #4706/#4110-style config plumbing) out of
scope for this fork patch.
