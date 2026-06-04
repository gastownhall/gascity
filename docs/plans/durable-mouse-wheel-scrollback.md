# Plan — Durable mouse-wheel scrollback in gc tmux sessions

**Bead:** `ga-c4w` (feature, P2) · **Rig:** gascity · **Branch:** `gc/ga-c4w` (base `origin/main` @ dd3ee8524)
**Supersedes:** the po-vtg2 city-local stopgap in portharbour (HQ bead po-vtg2; prototype branch `gc/po-vtg2`).

## Problem (one paragraph)

In gc tmux sessions the mouse wheel must scroll the tmux scrollback (copy-mode),
not browse Claude/shell history — durably and out-of-the-box for human-interactive
sessions — while headless agent sessions stay mouse-off (controller-poll safety).
Two facts make the wheel inert today: (1) the gastown pack `tmux-keybindings.sh`
has no `WheelUpPane` binding, so tmux forwards the wheel to mouse-reporting TUIs;
and (2) the runtime starts every session mouse-off via `disableMouseAndActivity`
unless the session's resolved `MouseOn` is true — and the interactive
provider/named session path never sets it. This plan ships the proper in-source
fix: the wheel binding in the pack, plus an interactive-session `MouseOn` default
in the runtime, replacing the prototype's client-attached `set-hook` stopgap.

## Architecture findings (read before executing)

The mouse-mode machinery is already wired end-to-end; this is a defaulting change,
not new plumbing.

- **Base state:** `examples/gastown/packs/gastown/assets/scripts/tmux-theme.sh:56`
  runs `set-option mouse on` at session create.
- **Runtime toggle:** `internal/runtime/tmux/adapter.go:930` —
  `if !cfg.MouseOn { disableMouseAndActivity(name) }`. `disableMouseAndActivity`
  (adapter.go:815) sets `mouse off` + `monitor-activity off`. So **`MouseOn=true`
  skips the disable and the theme's `mouse on` survives** → the wheel binding fires.
- **Headless agent path (must stay mouse-off):** `cmd/gc/template_resolve.go:564`
  sets `MouseOn: cfgAgent.MouseModeOn()`. With no `mouse_mode` in the agent config
  this is `false` → mouse off → poll-safe. Untouched by this plan.
- **Interactive path (the gap — Part B seam):** human-interactive provider/adhoc
  (`createProviderSession` / `humaCreateProviderSession`) and named sessions
  (`materializeNamedSessionWithContext`) build their runtime hints via
  **`sessionCreateHints`** (`internal/api/session_runtime.go:71`). That function
  does **not** set `MouseOn` → it defaults to `false` → interactive sessions are
  mouse-off → the wheel is inert. Both callers are human-interactive; neither is a
  headless pool worker.

**Why not the bead's suggested seam.** The bead/design proposed defaulting
`mouse_mode='on'` on the interactive template so `MouseOn = cfgAgent.MouseModeOn()`.
That does not work for provider/named sessions: their synthetic
`&config.Agent{Provider: providerName}` is used only for `config.ResolveProvider`
and then discarded — `MouseOn` for these sessions comes solely from
`sessionCreateHints`, not from any `config.Agent`. So the correct, minimal seam is
`sessionCreateHints` itself. This also keeps the change off the agent-template path
entirely, guaranteeing headless behavior is unchanged.

## Approach

Two independent units, each fronted by a failing test:

- **Part B (runtime default):** set `MouseOn: true` in `sessionCreateHints`. This
  flips exactly the human-interactive provider + named session paths to mouse-on;
  the headless agent path (`template_resolve.go`) is not involved.
- **Part A (pack binding):** append the `WheelUpPane`/`WheelDownPane` root-table
  bindings to `tmux-keybindings.sh` (adopt prototype commit `8d2f9963c`,
  applied as a manual two-line append — not a cherry-pick, to avoid dragging
  unrelated po-vtg2 changes). Do **not** add the prototype's client-attached
  `set-hook` stopgap (commit `028868482`); the `MouseOn` default replaces it.

## Micro-tasks

| id | description | acceptance (single failing test) | est_minutes | slings |
| --- | --- | --- | --- | --- |
| T-001 | Add a failing test asserting the interactive hints builder yields mouse-on. | In `internal/api/session_resolved_config_test.go`, extend `TestSessionCreateHintsSeedsRuntimeEnv` (or add `TestSessionCreateHintsEnablesMouse`) to assert `sessionCreateHints(resolved, env, nil).MouseOn == true`. Test **fails** today (defaults false). | 3 | — |
| T-002 | Make T-001 pass: default interactive sessions to mouse-on. | Add `MouseOn: true` to the `runtime.Config{...}` returned by `sessionCreateHints` (`internal/api/session_runtime.go:71`), with a comment referencing ga-c4w. `go test ./internal/api/ -run TestSessionCreateHints` is green. | 3 | — |
| T-003 | Guard the headless path stays mouse-off. | In `cmd/gc/template_resolve_prompt_test.go`, add a case (sibling to the existing on-case at ~:529) building `TemplateParams` from an agent with **no** `mouse_mode` and asserting `tp.Hints.MouseOn == false` **and** `templateParamsToConfig(tp).MouseOn == false`. Passes (agent path untouched) — locks acceptance #2/#4. | 4 | — |
| T-004 | Add a failing test for the pack wheel binding + stopgap-absence. | In `examples/gastown/gastown_test.go` (alongside `TestPromptFilesExist`, content-assertion style), add `TestTmuxKeybindingsScrollWheel`: read `examples/gastown/packs/gastown/assets/scripts/tmux-keybindings.sh` and assert it **contains** `WheelUpPane` and `WheelDownPane` bindings **and does not contain** `client-attached` (no set-hook stopgap, acceptance #5). Fails today (no wheel binding). | 4 | — |
| T-005 | Make T-004 pass: add the wheel bindings to the pack. | Append the two `gcmux bind-key -T root WheelUpPane …` / `WheelDownPane …` lines (with the explanatory comment block) to the end of `tmux-keybindings.sh`, exactly as in prototype `8d2f9963c`. Do not add any `set-hook`. T-004 green. | 3 | — |
| T-006 | Build + targeted tests + CHANGELOG. | `go build ./...` and `go test ./internal/api/... ./cmd/gc/... ./examples/gastown/...` all green; add a CHANGELOG.md entry under the current unreleased section noting the wheel-scrollback fix (Refs: ga-c4w, supersedes po-vtg2). | 5 | — |

### Exact edits (for the executor)

**T-002 — `internal/api/session_runtime.go`, inside `sessionCreateHints` return:**
```go
		AcceptStartupDialogs:   resolved.AcceptStartupDialogs,
		MouseOn:                true, // interactive provider/named sessions: wheel→tmux scrollback (ga-c4w)
		Env:                    sessionEnv,
```

**T-005 — append to `examples/gastown/packs/gastown/assets/scripts/tmux-keybindings.sh`:**
```sh

# ── Mouse-wheel scrollback (root table) ───────────────────────────────
# Make the wheel drive tmux copy-mode scrollback instead of leaking to the
# focused app. Without this, "mouse on" (set in tmux-theme.sh) hands the wheel
# to mouse-reporting TUIs — Claude Code scrolls its own history, a pager/shell
# gets Up-arrows — and only a bare prompt reaches copy-mode. Force copy-mode
# even over mouse-reporting apps (no mouse_any_flag check) so scrollback wins;
# once in copy-mode the wheel passes through (-M) for normal scrolling, and -e
# exits at the bottom. Shift+wheel still does native terminal selection.
gcmux bind-key -T root WheelUpPane   if-shell -F -t= "#{pane_in_mode}" "send-keys -M" "copy-mode -e"
gcmux bind-key -T root WheelDownPane send-keys -M
```

## Manual verification (acceptance #1, #3 — not unit-testable)

After merge + pack roll, in a fresh **interactive** `gc session new <provider>`:
1. Wheel-up in a Claude pane enters tmux copy-mode scrollback; wheel-down scrolls
   down and exits at the bottom.
2. Mouse pane-select, drag-resize, the `MouseDown1StatusRight` mail popup, and
   Shift+wheel native terminal selection all still work.
3. A headless agent session (peeked via `gc session attach`) shows `mouse off`
   (`tmux show-options -t <sess> mouse`).

## GDPR data-flow impact

None. This change governs tmux mouse-mode and key bindings for the gc developer
tooling/runtime. No personal data, special-category data, or data-subject content
is read, written, transmitted, or logged. No new fields, stores, exports, or
retention surfaces are introduced. No DPIA implication.

## MDR Class I traceability

No-op (outside the voxmemo → voxist-api clinical pipeline). This bead touches the
gc orchestration runtime and the gastown tmux pack only; it is not part of any
clinical chain-of-evidence from microphone to exported note. Heading retained per
planner discipline so the consideration is explicit for an auditor.

## Open questions (downstream-resolvable — no PM decision required)

1. **`monitor-activity` side-effect.** `MouseOn=true` skips the whole
   `disableMouseAndActivity`, so interactive sessions also keep `monitor-activity on`.
   This matches what `mouse_mode=on` agents already get today, and is benign for a
   human-attended session. If the reviewer wants `monitor-activity` off regardless
   of mouse, split `disableMouseAndActivity` (mouse conditional, activity always) —
   out of scope for this bead unless flagged. *(reviewer)*
2. **No headless caller of `sessionCreateHints`.** Verified both current callers are
   interactive (provider-adhoc + named). The executor should re-confirm via
   `grep -rn sessionCreateHints` that no agent/pool path adopts it; T-003 guards the
   resolved agent path regardless. *(executor)*
3. **`WheelDownPane send-keys -M` behavior** at the bottom of scrollback — confirm it
   exits copy-mode cleanly (covered by manual verification #1). *(executor/manual)*

## Out of scope

- The portharbour city-local po-vtg2 stopgap removal is a **separate** city-store
  follow-up (HQ bead), not part of this gc-source PR. Once this ships and the pack
  is rolled, file/track the stopgap teardown there.
