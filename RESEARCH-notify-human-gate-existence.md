# Existence Report — notify-on-human-gate-creation + staleness re-nudge

**Bead:** sc-lcwsqf (research-only increment 1) · **Charter:** Chris 9:16 AM ET 07-22 (binding, #dip-oversight): *"don't invent new tooling until you prove that it does not already exist and possibly needs to be used properly… this sounds like a lack of knowledge of existing infrastructure not a real problem."*
**Author:** claude-1 · **Base:** gascity fork worktree @ `origin/main` `6ba5a29f1` (#4541); beads module `github.com/steveyegge/beads v1.1.0` (installed `bd` = `1.1.0 (8e4e59d39)`).

## The two capabilities under test

1. **notify-on-human-gate-creation** — when a `type=human` gate bead is created, the system emits a **mail + nudge to the gate's human addressee** so the human is actively told a gate awaits them.
2. **staleness re-nudge** — an OPEN human gate open past a configurable threshold **re-fires mail+nudge** to the addressee, repeating on an interval.

## VERDICT (proven, not vibes)

| Capability | Verdict | Proof |
|---|---|---|
| (1) notify-on-human-gate-creation | **ABSENT** | Gate creation performs zero notification side-effects (code + CLI + runnable demo). |
| (2) staleness re-nudge for human gates | **ABSENT** | The only gate watcher **skips human gates entirely**; "escalation" is a one-shot shell-out to an external `gt escalate`, never a repeating mail+nudge, and never reaches human gates. |

Closest existing machinery is documented below under **§6 "configure-it-properly" analysis** — none of it covers either capability, and none is configurable to.

---

## §1 — First-hand code evidence (beads module `cmd/bd/gate.go`)

This is where `bd gate create` / `bd gate check` / `--escalate` actually live (gascity's `gc bd gate …` delegates to embedded beads `v1.1.0`; `internal/beads/` in gascity is only a store adapter). Path: `~/go/pkg/mod/github.com/steveyegge/beads@v1.1.0/cmd/bd/gate.go`.

### 1a. Gate creation notifies nobody — `gateCreateCmd` (gate.go:252–338)

The entire create path: resolve the blocked target, build the gate `types.Issue{IssueType:"gate", AwaitType:gateType, …}`, `store.CreateIssue`, add a `blocks` dependency, `store.Commit`, then **print to stdout** and return. There is **no mail send, no nudge enqueue, no notification** of any addressee. Full side-effect surface (gate.go:307–338):

```
if err := store.CreateIssue(ctx, gate, actor); err != nil { … }      // create bead
… store.AddDependency(ctx, dep, actor) …                              // blocks edge
… store.Commit(ctx, commitMsg) …                                      // commit
fmt.Printf("✓ Created gate %s (type: %s)\n", … )                       // stdout only
fmt.Printf("\nResolve with: bd gate resolve %s\n", gate.ID)            // stdout only
```

Note the gate carries `Owner: getOwner()` but no assignee/addressee is set and nothing is sent to anyone. → **Capability (1) ABSENT.**

### 1b. The gate watcher explicitly skips human gates — `gateCheckCmd` (gate.go:497–641)

`bd gate check` loads open `gate`-type issues and dispatches by `AwaitType` (gate.go:559–571):

```
switch {
case strings.HasPrefix(gate.AwaitType, "gh:run"): … checkGHRun …
case strings.HasPrefix(gate.AwaitType, "gh:pr"):  … checkGHPR …
case gate.AwaitType == "timer":                   … checkTimer …
case gate.AwaitType == "bead":                     … checkBeadGate …
default:
    // Skip unsupported gate types (human gates need manual resolution)
    continue                                        // <-- human gates fall here
}
```

`human` hits `default → continue` (gate.go:568–570): human gates are **never processed** by the watcher. `shouldCheckGate` (gate.go:644+) filters before this too.

### 1c. "Escalation" is a one-shot external shell-out, never a repeating nudge — `escalateGate` (gate.go:891–906)

Escalation only fires when `r.escalated` is true (gate.go:606–617), and escalation is set **only** by `checkGHRun` (gh:run failure/canceled) and `checkGHPR` (gh:pr CLOSED). `checkTimer` **never escalates** by design (gate.go:856–857: *"timers resolve but never escalate (escalated is always false by design)"*). Human gates can't reach here at all (§1b). When it does fire, escalation is:

```
func escalateGate(gate *types.Issue, reason string) {
    topic := fmt.Sprintf("Gate escalation: %s", gate.ID)
    message := fmt.Sprintf("Gate %s needs attention.\nType: %s\nReason: %s\nCreated: %s", …)
    escalateCmd := exec.Command("gt", "escalate", topic, "-s", "HIGH", "-m", message)   // external tool
    … if err := escalateCmd.Run(); err != nil { "Warning: escalation failed …" }         // best-effort, no-op if gt absent
}
```

It shells out **once** to an external `gt escalate` — not gascity's mail/nudge system, not the gate addressee, not repeating on an interval. → **Capability (2) ABSENT.**

### 1d. Gate subcommand surface (gate.go:909–946)

Registered subcommands: `list`, `create`, `show`, `resolve`, `check`, `add-waiter`. There is no `notify`, `remind`, or `re-nudge` subcommand. `gate add-waiter` / the "wake" hint (gate.go:174–177) notify the **waiter/successor when the gate CLOSES** — the opposite direction from notifying the human addressee when it OPENS.

---

## §2 — CLI help-surface receipts (installed `bd 1.1.0`)

Full transcript: `scratchpad/adversarial-receipts.txt`. Key points:

- **`bd gate create --help`** flags = `--await-id`, `--blocks`, `--reason`, `--timeout`, `--type` (default `human`). **No `--notify` / `--nudge` / `--mail` / `--addressee` / `--assignee` flag exists.** Creation cannot be configured to notify.
- **`bd gate check --help`** `--type` accepts `gh | gh:run | gh:pr | timer | bead | all`. **`human` is not a checkable type.** "A gate is escalated when: gh:run failure/canceled; gh:pr CLOSED" — human/timer never escalate. No `--interval` / re-nudge flag.

---

## §3 — Runnable behavioral demonstration (isolated `bd` store)

Isolated store via `bd init` in a scratch dir; full transcript in `scratchpad/adversarial-receipts.txt`. Receipts:

- **Create human gate** → output is *only*: `✓ Created gate bd-demo-f4x (type: human) / Blocks: … / Resolve with: bd gate resolve bd-demo-f4x`. **No mail, no nudge, no addressee notified.**
- **Full store dump** after creation shows **no message/nudge/notification bead** — only the target task + the gate.
- **`bd gate check --escalate`** with an OPEN human gate present → `Checked 0 gates: 0 resolved, 0 escalated, 0 errors`. The human gate is invisible to the watcher.
- **`bd gate check --type=human --escalate`** → also `Checked 0 gates`. You cannot even target human gates.

---

## §4 — Community record

- **gascity#4399** (OPEN, `kind/feature`, `priority/p3`, triaged 2026-07-19): *"[steps.gate] type vocabulary … is parsed-but-not-enforced — no watcher resolves formula-synthesized typed gates."* This is about **auto-resolution** of `timer/gh:run/gh:pr` formula-synthesized gates, and it **explicitly states human gates are manual-resolution by design** and that `human` is absent from `bd gate check --type`. Adjacent context to cite — **not** the notify/re-nudge capability. Still open; no resolution merged.
- **Merged-PR sweep** (`gh search prs --repo gastownhall/gascity --merged --match title gate`, 30 hits): every "gate" PR is a CI/config/RC/pool/molecule gate — **none** implements notify-on-human-gate-creation or human-gate staleness re-nudge.
- No open gascity issue matches "human gate notify"; no beads issue matches the query.

---

## §5 — Independent corroboration (three passes, unanimous)

Three independent investigations reached the same verdict as the first-hand read above — **both ABSENT**.

**(a) Codex CLI independent pass** (`/usr/bin/codex exec`, read-only, 248k tokens, exit 0; full trace `scratchpad/codex-research-output.txt`, prompt `scratchpad/codex-research-prompt.txt`). Verbatim verdict: *"Both requested capabilities are absent… No existing config switch connects these pieces; both capabilities require new watcher/wiring code."* Its cites match mine (gate.go:262/294/307 create-no-notify; gate.go:554/568 check-skips-human; gate.go:606/891 escalate=external `gt`) and it independently located the **gascity-side gate-sweep** order: `internal/bootstrap/packs/core/orders/gate-sweep.toml:4` (30s cooldown) → `assets/scripts/gate-sweep.sh:28` runs `gc bd gate check --escalate` **only for `timer`/`gh`, never `human`**.

**(b) Adversarial shipped-code sweep** (gascity). Traced *every* bead-creation entrypoint — `internal/beads/caching_store_writes.go:51`, `internal/api/huma_handlers_beads.go:558`, `internal/session/wait_store.go:329`, `internal/molecule/molecule.go:950/1177`, `internal/formula/compile.go:520-559` — all funnel to `store.Create` and emit only a `bead.created` **cache event**. That event has **exactly two consumers** (`cmd/gc/api_state.go:488` cache-freshness; `cmd/gc/dispatch_runtime.go:691` dispatcher-wake) — **neither notifies**. No post-create hook mechanism exists; no shipped order triggers on `bead.created`. For staleness: every periodic loop is cleanup / auto-close / readiness-wake / pool-backstop — **none re-notifies an aging open gate**. Note: **`gc.forward_gate` does not exist anywhere in gascity code** (it is town doctrine only).

**(c) Adversarial docs/config/help sweep.** Formula `[steps.gate]` `type` and `timeout` are documented **"Accepted But Inert / no runtime consumer"** (`docs/reference/specs/formula-spec-v2.md:900-908`, v1:668-682). There is **no `[gate]`/`[nudge]`/`[notify]` config table** (`internal/config/config.go:256-304`). The `notify`/`poll_interval` config fields belong to the **GitHub PR monitor** (`internal/config/github_pr_monitor.go`), not gates. Full candidate-knob table (every plausible flag/field, and why each fails) captured in the sweep — all fail.

---

## §6 — "Configure-it-properly" analysis (honoring the charter's hypothesis)

The charter's hypothesis was that this may be *"a lack of knowledge of existing infrastructure, not a real problem."* Tested directly: **there is no configuration path** that yields either capability (proven in §2/§3/§5). But the honest, useful finding is that **the eventual build is thin wiring on existing primitives — not net-new tooling.** The reusable substrate:

- **Event-order subsystem** — an order `trigger = "event", on = "bead.created"` fires on gate creation; `bead.created` is a real event constant carrying `issue_type`, `assignee`, `metadata`, `labels`. (`internal/orders/triggers.go:360`, `internal/events/events.go:23`.) This is the natural hook for capability (1).
- **`gate-sweep.toml` cooldown-order pattern** (`internal/bootstrap/packs/core/orders/gate-sweep.toml`) — the template for a periodic "list open `human` gates aged past a threshold → notify" sweep (capability (2)).
- **`cascade-nudge-on-blocker-close.sh`** — a working script template for "react to an event and notify the relevant party."
- **`escalate.sh`** (`internal/bootstrap/packs/core/assets/scripts/escalate.sh:49`) — **already mails a configurable `GC_ESCALATION_RECIPIENT` (default `human`)**. The mail-a-human primitive EXISTS; it is simply never invoked by gate creation or gate staleness.

**Design wrinkles the build MUST handle** (surfaced by the sweeps — these are *why* it's not mere configuration):

1. **The human channel is special.** `gc mail send --notify` deliberately **does not nudge `human` recipients** (`cmd/gc/cmd_mail.go:1847` `to != "human"`; humans have no tmux session to poke). "Notify the human" must route via mail/Slack/`escalate.sh`, not the tmux-nudge path.
2. **Deferred assignee.** Formula/molecule-synthesized gate steps **strip the assignee to `gc.deferred_assignee` at create time** (`internal/molecule/molecule.go:1372-1376`). A naive "nudge the assignee on creation" finds an empty assignee — the addressee must be resolved from the deferred field / the doctrine convention.
3. **Gate `type` vocabulary is inert by design**, so the fix cannot ride on "just make the existing gate `type` do something" without also confronting spec §4's deliberate no-runtime-consumer stance (i.e., it is a genuine implementation, reviewable as such).

**Conclusion of §6:** ABSENT, and *not* reachable by configuration — but the build is a composition on existing order/event/escalate/nudge machinery plus the two wrinkles above, which is the correct, cheapest shape and directly answers the "use existing infra properly" hypothesis: the infra to *build on* exists; the *capability* does not.

---

## §7 — Conclusion

Both capabilities are **ABSENT** and **not reachable by configuration** of existing tooling — proven by first-hand source (beads `gate.go`), CLI help surfaces, a runnable isolated-store demonstration, and the community record. Per the charter's two honest outcomes, this is the **ABSENT** branch: the proven delta (and only it) authorizes the build in later increments — a native fork fix that, on `type=human` gate creation, mails+nudges the addressee (respecting loud-fail semantics), plus a staleness re-nudge sweep for aging open human gates.
