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

## Execution status (executor — ga-c4w)

All micro-tasks green; per-task commits on `gc/ga-c4w`. Verified with the
hermetic `env -i` test wrapper (Makefile `TEST_ENV`) + ICU CGO flags for
`icu4c@78` (`CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c@78/include`,
`CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c@78/lib`).

- [x] T-001 — failing test for interactive mouse-on hints       ✅ green at 19d6a9cdf (`TestSessionCreateHintsEnablesMouse`)
- [x] T-002 — default interactive sessions to mouse-on          ✅ green at 19d6a9cdf
- [x] T-003 — guard headless agents stay mouse-off              ✅ green at fe1c2149f (`TestResolveTemplateHeadlessAgentStaysMouseOff`)
- [x] T-004 — failing test for pack wheel binding + no stopgap  ✅ green at 6bc2d400a (`TestTmuxKeybindingsScrollWheel`)
- [x] T-005 — append wheel bindings to gastown pack             ✅ green at 6bc2d400a
- [x] T-006 — build + targeted tests + CHANGELOG                ✅ green at 0745b53d8

**Build/test evidence.** `go build ./...` → Success. `go test ./internal/api/...
./examples/gastown/...` → 1635 passed. `go test ./cmd/gc/...` → all ga-c4w tests
pass; two failures (`TestBdRuntimeEnvManagedCityProjectsHostOverride`,
`TestBdRuntimeEnvForRigInheritedManagedCityProjectsHostOverride`) reproduce
**identically on base `dd3ee8524`** with none of this bead's changes present —
pre-existing, unrelated to ga-c4w (managed-Dolt port resolution; the sandbox's
proxied-server setup does not produce the host-override). The
`TestProbeDetachedWork_TmuxExitStatus` timeouts were host-env flakes that pass
under the hermetic `env -i` wrapper.

**Open questions.** Q#2 resolved (executor): `grep -rn sessionCreateHints` → only
two production callers, both interactive (`session_resolution.go:318` named,
`session_resolved_config.go:58` provider-adhoc); no headless/pool caller. Q#1
(`monitor-activity` side-effect) and Q#3 (`WheelDownPane` exit-at-bottom) deferred
to reviewer / manual verification per plan.

## Re-review correction (executor — ga-c4w, second pass)

The reviewer's **CHANGES_REQUESTED** (PR #3103) found the original fix targeted the
wrong seam: **`gc session new` (CLI) never calls `internal/api/sessionCreateHints`** —
that is the HTTP-API path (dashboard / real-world apps). Open-Q#2's analysis was
mistaken: it verified the *callers of* `sessionCreateHints`, not that `gc session new`
calls it (it does not). The CLI resolves runtime hints in **cmd/gc**: the
managed-deferred reconciler start uses `templateParamsToConfig` (`MouseOn` was
`cfgAgent.MouseModeOn()` = false), and the unmanaged direct start uses
`workerSessionCreateHints` (set no `MouseOn`). So `gc session new` started mouse-off and
the Part A wheel binding never fired — bead acceptance #1 unmet for the canonical entry
point. Fixed across the real CLI seams (TDD, failing-first):

- [x] T-007 — mouse-on for unmanaged `gc session new` direct start (`workerSessionCreateHints`)   ✅ green at ffb3a40c6 (`TestWorkerSessionCreateHintsEnablesMouse`)
- [x] T-008 — mouse-on for managed-deferred `gc session new` via `session_origin=manual`; pool + named stay off (`templateParamsToConfig`)   ✅ green at c254074fc, **scope-corrected** to manual-only (see drift note) (`TestTemplateParamsToConfigInteractiveSessionEnablesMouse`)
- [x] T-009 — keep interactive sessions mouse-on across resume (`sessionResumeHints`, reviewer finding #2)   ✅ green at 6c5e5f825 (`TestSessionResumeHintsEnablesMouse`)
- [x] T-010 — CHANGELOG accuracy (all create + resume seams), LOW comment-rot fixes (`sessionCreateHints` comment, test `adapter.go:930`→function ref), build + targeted tests, plan status

**Fingerprint-drift correction (T-008 scope).** The first T-008 cut keyed mouse-on on
`templateParamsSessionOrigin(tp) != "ephemeral"`, i.e. **manual *and* named** sessions.
That regressed two reconciler tests (`TestPhase0ConfigDrift_AsleepNamedSessionApplies\
TemplateOverrides`, `TestReconcileSessionBeads_RateLimitScreenReholdsAfterQuarantine\
Expiry`) — both pass on base `d2ff3e104`, both failed under the broad cut. Root cause:
**`MouseOn` is an intentional core-fingerprint field** (locked by
`runtime.TestConfigFingerprintIncludesMouseOn`), so flipping it for long-lived
config-declared/named sessions changes their drift hash → spurious config-drift (and in
the rate-limit case, drift even suppressed the post-quarantine wake). Fix: narrow the
predicate to `templateParamsSessionOrigin(tp) == "manual"`, exactly the reviewer's
`session_origin=manual` scope. Named/config sessions keep following `mouse_mode` via
`Hints.MouseOn` (no drift). Both tests pass unmodified; `TestConfigFingerprintIncludesMouseOn`
stays green (fingerprint contract untouched).

**Seam coverage now.** Managed-deferred CLI (`templateParamsToConfig`: origin `manual`
→ on; named/config → follows `mouse_mode`; ephemeral pool → off, controller-poll safe),
unmanaged-direct CLI (`workerSessionCreateHints` → on), API create (`sessionCreateHints`
→ on), API resume (`sessionResumeHints` → on). Headless agent path
(`cfgAgent.MouseModeOn()` = false) is unchanged — `TestResolveTemplateHeadlessAgentStaysMouseOff`
still green, so no poll-safety regression.
