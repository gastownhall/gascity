# Review loop — notify-on-human-gate-creation + renudge-stale-human-gates

**Bead:** sc-lcwsqf (increment 5). **Diff under review:** `d9c89345a..7dfa184d3`
(cap1 `957ceb46b` + cap2 `32c5fee7c` + acceptance `7dfa184d3`), base `origin/main @ 6ba5a29f1`.

## Reviewers (two independent adversarial lenses)

1. **Independent Codex CLI** — `codex-cli 0.145.0`, model `gpt-5.6-sol`, xhigh effort,
   ChatGPT-plan auth (not API). Driven per the 13:43 protocol: `codex exec
   --skip-git-repo-check`, prompt via STDIN, no `--model fugu-ultra`, single bounded
   run, file:line citations demanded. A genuinely different model family (OpenAI GPT)
   as the second pair of eyes. Prompt: `REVIEW-receipts/codex-prompt.txt`; verdict:
   `REVIEW-receipts/codex-review-verdict.md`; trace excerpt:
   `REVIEW-receipts/codex-review-trace-excerpt.txt`.
2. **Worker adversarial pass** (this session, `claude-1`) — first-hand read of all 5
   files, plus tool checks: `gofmt -l`, `go vet`, `go test`, `bash -n`. Standing order
   [Doc OUT of operations] means nothing gates on/waits for the Doc *agent*; the
   "Doc-style" adversarial lens (discipline / convention / correctness) was executed by
   the worker here, so the review loop did not block on an out-of-ops identity.

Every Codex citation was **re-verified against the actual source** before being accepted
or rebutted — a cited line number is a claim, not proof.

## Findings, verification, and resolution

### F0 (worker) — `pack_orders_test.go` was gofmt-dirty — FIXED
`gofmt -l` flagged the file (over-aligned comment block, old lines 275-285). A canonical
PR's CI `gofmt` check would reject it. Fixed via `gofmt -w`.

### F1 (codex, major) — loud-fail was not actually loud — FIXED
Claim: the failed-send stderr line prints but the script exits 0; the controller persists
the bead.created cursor *before* the run and logs exec output *only on non-zero exit*, so
a failed creation notify is swallowed. **Verified TRUE against source:**
`order_dispatch.go` — `front.SetCursor(...)` is called before `m.execRun` ("persist the
cursor before the command runs; otherwise a crash after the side effect can replay the
event"), and the captured `output` is logged only inside `if err != nil` (non-zero exit).
`shellExecRunner` sets `cmd.Stdout` **and** `cmd.Stderr` to the same buffer
(`order_dispatch.go:151-152`), so the existing stderr message *is* captured — it just
needs a non-zero exit to be logged.
**Fix:** both scripts now count failed sends and `exit 1` after writing state (successful
sends stay deduped; the already-advanced event cursor is not rolled back, so no replay
storm). The documented #4543 loud-fail is now real. The over-claiming top-of-file comment
was corrected to describe the actual retry path (opportunistic within the lookback window;
guaranteed by the staleness sweep). Behaviorally proven — see below.

### F2 (codex, major) — nudge-failure deduplicated as success — REBUTTED (by design), noted
Claim: `gc mail send --notify` prints a nudge error but returns 0, so a mail-delivered/
nudge-failed is stamped as notified. **Verified TRUE against source:** `cmd_mail.go` —
after `MailSent` is recorded, the nudge runs `if to != "human"`; on nudge error it prints
`nudge failed` to stderr but the function still `return 0` (the `notified` bool is exposed
only in `--json` output). **Resolution — rebutted as a considered tradeoff, not a silent
gap:** the *mail* is the durable notification (it is in the addressee's inbox); the nudge
is an ephemeral poke. For a `human` addressee the nudge is intentionally skipped (no
session), so `notified=false` there is correct, not a failure. For a session addressee, a
transient nudge failure is recovered by the companion staleness sweep, which re-notifies
any gate still open past the threshold. Treating nudge-failure as un-notified would instead
re-deliver a *duplicate mail* on every retry. Codex's "expose a typed result" is also
unaware that `--json` already surfaces `.Notified`; a future hardening could gate on it,
but the mail-delivered = success semantics are deliberate and safe given the sweep backstop.

### F3 (codex, major) — GNU-only `date -d` disables the sweep on BSD/macOS — FIXED
Claim: `renudge:103` `date -u -d "$1"` is GNU-only; BSD rejects `-d`, so `iso_to_epoch`
returns empty and every gate is skipped at the age check → the sweep is inert on macOS.
**Verified TRUE and in-scope:** the sibling `wisp-compact.sh:68-70` already solves exactly
this with a three-layout fallback (`date -d` → `date -ju -f "%Y-%m-%dT%H:%M:%SZ"` →
no-`Z`), with a comment explaining the BSD path "forces UTC to match GNU `date -d`". This
PR regressed an established repo convention. macOS is a supported dev platform for gascity.
**Fix:** `iso_to_epoch` now uses the same three-layout fallback. A contract test pins
`date -ju -f` so the portability cannot silently regress.

### F4 (codex, minor) — event parsing assumed only the API envelope — FIXED
Claim: `gc events` has a local fallback (API down) that copies the raw bus payload, where
bead fields sit under `.payload` not `.payload.bead`, so `.payload.bead.issue_type` yields
no gates in fallback mode. **Verified TRUE and ground-truthed:** a real `bead.created` line
in the live `events.jsonl` has `has_payload_bead:false` with `issue_type`/`id` directly
under `.payload`; `internal/api/event_payloads.go` confirms the API path wraps the bead as
`{"bead": ...}` while the raw snapshot is unwrapped (`cmd_events.go` `localWireEvent` copies
`e.Payload` verbatim). Fallback mode = exactly when notifications matter most.
**Fix:** the gate filter now normalizes `(.payload.bead // .payload)`, reading both shapes.
Zero-risk for the working API path. Contract test updated to pin the normalization.
Behaviorally proven against a raw-shape event — see below.

### F5 (codex, minor) — substring tests can't catch runtime defects — PARTIALLY ADDRESSED
Fair critique of test depth. **Addressed:** added a fake-`gc`-on-PATH behavioral harness
(`REVIEW-receipts/fix-behavioral-proof.sh`) exercising the notify script end-to-end:
success path, loud-fail non-zero + no-dedup (F1), raw-fallback-shape detection (F4),
non-gate ignored, and the renudge date chain (F3). 6/6 pass
(`REVIEW-receipts/fix-behavioral-proof-output.txt`). The Go contract tests were also
strengthened to pin the three fixed behaviors. A full hermetic Go-level harness with
faked CLI remains a reasonable future hardening but is beyond this bounded increment; the
runtime behavior is now covered by the bash harness + the increment-4 live-town acceptance.

## Codex CLEAN areas (independently confirmed, matched my read)
CLI command/flag surface + API-mode envelope; nested-loop state scope + `${VAR:+...}`
expansion under `set -u`; jq addressee precedence + valid-state pruning; sibling-order
convention + registration (flat TOMLs auto-discovered via `embed.go` `go:embed`, no
manifest edit).

## Re-verification after fixes
`gofmt -l` clean · `go vet` clean · `go test ./internal/bootstrap/packs/core/...` PASS
(6 order tests incl. the 3 strengthened contracts) · `bash -n` clean both scripts ·
behavioral harness 6/6.

**Net:** Codex verdict was HOLD on F1/F2/F3. F1 and F3 fixed; F4 fixed; F2 rebutted with a
documented rationale (safe by design, sweep-backstopped); F5 addressed via a behavioral
harness + strengthened contracts; F0 (gofmt) fixed. The diff is now ship-ready for the
canonical PR (next increment).
