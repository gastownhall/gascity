# Plan: cancel tmux copy-mode before controller key delivery (ga-c4w major #2)

> **Status:** ready-to-execute — 2026-06-05
> **Bead:** `ga-c4w` (feature, P2) — addendum for the SOLE remaining
> blocker on PR #3103.
> **Authority:** platform-architect ruling (2026-06-05 20:39) + reviewer-2
> re-review #3 (20:37). sjarmak (human) PR review 4437810731.
> **Scope:** major **#2 only**. Majors #1 + #3 are **already fixed** (stale
> review) — do NOT touch them. **No re-architecture / no MouseOn-seam
> centralization** (architect explicitly narrowed to #2).
> **Branch:** continue the existing PR branch `gc/ga-c4w` (head f4f53388e);
> push to `fork/gc/ga-c4w` → updates PR #3103. Base = `main`.

## Context

This bead added a `WheelUpPane → copy-mode -e` binding (Part A) so the
mouse wheel scrolls tmux scrollback in interactive sessions. That binding
introduces a **regression**: when a pane is parked in copy-mode (a human
scrolled up), the controller's key delivery is **silently dropped** —
tmux routes keystrokes to copy-mode instead of the program.

Both delivery seams issue a bare literal send with **no copy-mode guard**
(verified on `gc/ga-c4w@f4f53388e`):

- `internal/runtime/tmux/tmux.go:1099` `SendKeysDebounced` →
  `t.run("send-keys", "-t", session, "-l", keys)` then a separate `Enter`.
  Carries nudges, prompts, mail, controller messages.
- `internal/runtime/tmux/interaction.go:269` `Respond` →
  `t.run("send-keys", "-t", name, "-l", key)` (the `1`/`2`/`3` interaction
  responses).

If `#{pane_in_mode}` is true at delivery, both are swallowed. Only bites
interactive mouse-on panes (headless agents stay mouse-off → the wheel
binding never parks them), but it is a real silent-drop of controller
input — the architect's sole remaining merge blocker.

### Fix (architect-prescribed, focused)

Before the literal send in **both** seams, cancel copy-mode on the target
pane **iff it is parked**. Add one shared helper on `*Tmux` and call it at
the top of each delivery path:

```go
// cancelCopyModeIfParked exits copy-mode on the target pane before key
// delivery so controller keystrokes are not swallowed when a human has
// scrolled back (ga-c4w introduced the WheelUpPane->copy-mode binding).
// No-op when the pane is not in a mode (the common case) and for headless
// agent panes (mouse-off, never wheel-parked).
func (t *Tmux) cancelCopyModeIfParked(session string) error {
    // if-shell evaluates the format on the target; cancel only when parked.
    _, err := t.run("if-shell", "-t", session, "-F", "#{pane_in_mode}",
        "send-keys -t "+session+" -X cancel")
    return err
}
```

(Equivalent: `display-message -p -t <session> '#{pane_in_mode}'` → if `"1"`,
`send-keys -t <session> -X cancel`. Executor picks whichever the fake-exec
harness tests most cleanly; behaviour is what matters.) Errors from the
guard must not abort delivery on a pane that simply isn't in a mode.

The seam dispatches through `t.exec.executeCtx` (`tmux.go:248-265`), which
tests stub with a fake recorder (see `executor_test.go` /
`interaction_test.go`) — so the regression test can assert the cancel is
issued before the `-l` send when parked, and not issued when not.

## Micro-tasks

TDD, red→green. First task is the failing test. Run from the worktree root
(Go 1.26.3; raw `go test` needs the icu4c CGO flags — `-I/-L $(brew
--prefix icu4c)` — per the gascity dev loop).

| id | description | acceptance (single failing test → make it pass) | est_minutes | slings |
| --- | --- | --- | --- | --- |
| T-001 | Add failing test `TestSendKeysCancelsCopyModeBeforeDelivery` (tmux pkg, fake-exec recorder mirroring `executor_test.go`): with the fake reporting `#{pane_in_mode}`=1, `SendKeysDebounced` issues a copy-mode `-X cancel` on the target **before** the `-l` keys send; with `pane_in_mode`=0, **no** cancel is issued and delivery is unchanged. | `go test ./internal/runtime/tmux/ -run TestSendKeysCancelsCopyModeBeforeDelivery` **fails** — no cancel is issued today; the parked-pane assertion fails. | 5 | — |
| T-002 | Add `func (t *Tmux) cancelCopyModeIfParked(session string) error` and call it at the top of `SendKeysDebounced` (`tmux.go:1099`), before the `-l` send. Guard errors must not abort normal delivery. | `go test ./internal/runtime/tmux/ -run TestSendKeysCancelsCopyModeBeforeDelivery` **passes**. | 5 | — |
| T-003 | Add failing test `TestRespondCancelsCopyModeBeforeDelivery`: with `pane_in_mode`=1, `Respond` (interaction.go) cancels copy-mode before sending the `1`/`2`/`3` key; `pane_in_mode`=0 → no cancel. | `go test ./internal/runtime/tmux/ -run TestRespondCancelsCopyModeBeforeDelivery` **fails** — `interaction.go:269` sends `-l` with no guard. | 4 | — |
| T-004 | Call `cancelCopyModeIfParked` in `Respond` (`interaction.go`) before the `-l key` send. | `go test ./internal/runtime/tmux/ -run TestRespondCancelsCopyModeBeforeDelivery -count=1` **passes**, and the full tmux suite (`go test ./internal/runtime/tmux/ -count=1`) stays green. | 4 | — |

Total est: ~18 min (2 red/green pairs; files: `internal/runtime/tmux/tmux.go`, `internal/runtime/tmux/interaction.go`, + the tmux test file).

## GDPR data-flow impact

**No impact.** This change cancels a tmux UI mode (copy-mode) before
delivering keystrokes to a local terminal pane. No personal data is read,
written, transmitted, or logged. The keystrokes are controller control
input (nudges, prompts, `1`/`2`/`3` interaction responses), not
data-subject data. No new persistence, network egress, or log fields.
Article 30 record of processing unaffected.

## MDR Class I traceability

**No-op outside voxmemo.** This change is in `gascity` (the `gc`
orchestration runtime's tmux adapter), not the voxmemo→voxist-api clinical
documentation pipeline. It does not touch the chain-of-evidence from
microphone capture through ASR to exported clinical note. The heading is
retained per Voxist planner discipline so an auditor sees the explicit
consideration.

## Validation gates

- `go test ./internal/runtime/tmux/ -count=1` green; `go vet ./...` clean.
- **Regression proven:** controller delivery to a copy-mode-parked pane
  now reaches the program (cancel issued before the literal send); a
  non-parked pane is unchanged (no spurious cancel); headless/agent panes
  (mouse-off, never wheel-parked) are unaffected.
- `git diff` confined to `internal/runtime/tmux/tmux.go`,
  `internal/runtime/tmux/interaction.go`, and the tmux test file. **Do not
  modify** the MouseOn seams (majors #1/#3 are already fixed) or the
  gastown wheel binding (Part A is correct).
- No new third-party Go modules; no new env vars.
- **Manual operator check:** scroll an interactive session into copy-mode,
  trigger a controller nudge/mail/prompt; confirm it lands (pane exits
  copy-mode and receives the keys). Record in the PR.

## Notes for the executor

- **Scope discipline.** Architect ruling: #2 is the SOLE blocker. #1
  (resume seam `session_runtime.go`) and #3 (named/managed
  `template_resolve.go:729`) were already fixed by T-007/T-008/T-009 —
  sjarmak's review of those is stale. Do **not** re-touch them; do **not**
  attempt the "centralize MouseOn into one interactive gate" refactor the
  reviewer floated (architect did not order it).
- **One guard at the top.** Cancelling copy-mode once before the `-l keys`
  send is enough; the subsequent `Enter` then lands in normal mode. No
  need to guard the `Enter` separately.
- **Don't break the happy path.** The guard must be a no-op (and
  non-fatal) when `#{pane_in_mode}` is empty/0 — the overwhelmingly common
  case. Assert this in the tests so a tmux-version quirk can't silently
  start dropping the guard's error onto normal delivery.
- **After green:** push to `fork/gc/ga-c4w`, confirm CI, then re-request
  sjarmak's review on PR #3103 (all 3 majors then addressed: #1/#3 already
  in, #2 by this change). DoD = merge is out of our scope; `ga-5x9` is the
  human merge-gate and no longer blocks `ga-c4w`.

## Open questions

- `[architect]` **MouseOn-seam centralization** (compute interactive-ness
  once; each seam reads from it). Reviewer-2 floated this as the durable
  root-cause fix; the architect's ruling did not order it for this bead.
  Track as a separate follow-up if desired — out of scope here.

## Out of scope

- Majors #1 and #3 (already fixed) and any MouseOn resolution changes.
- The gastown `WheelUpPane`/`WheelDownPane` binding (Part A — correct).
- Centralizing the 4 MouseOn seams (see Open questions).
- The human merge (`ga-5x9`) — out of our DoD.

## Execution status

All 4 micro-tasks green (2 red/green pairs; the red test commits at its
green, so each pair shares one checkpoint commit). Full tmux suite green
(188 passed, 0 failed); `go vet ./internal/runtime/tmux/` clean;
`gofumpt -l`/`gofmt -l` empty on all touched files.

- [x] T-001 — failing test `TestSendKeysCancelsCopyModeBeforeDelivery` (parked → probe+cancel before `-l`; unparked → no cancel) ✅ green at 2701bf0e0
- [x] T-002 — `cancelCopyModeIfParked` helper + wired into `SendKeysDebounced` ✅ green at 2701bf0e0
- [x] T-003 — failing test `TestRespondCancelsCopyModeBeforeDelivery` ✅ green at e9c1c1710
- [x] T-004 — wired guard into `Respond`; updated `respondInteractionSeamResult` (now 4 tmux calls incl. the not-parked `#{pane_in_mode}` probe); full tmux suite green ✅ green at e9c1c1710

**Implementation note.** The guard probes `#{pane_in_mode}` via
`display-message` then issues `send-keys -X cancel` only when parked
(mirrors the existing `IsSessionRunning`/`IsSessionAttached` house
pattern), rather than an opaque `if-shell`. This makes the
parked-vs-not behaviour observable to the fake-exec recorder, so the
regression tests can assert the cancel is issued before delivery when
parked and NOT issued otherwise. `cancelCopyModeIfParked` returns no
error and swallows probe/cancel failures, structurally guaranteeing the
guard can never abort delivery on a pane that simply is not in a mode.
