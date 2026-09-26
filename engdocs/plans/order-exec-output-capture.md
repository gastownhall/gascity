# Plan: order-exec-output-capture — order tracking beads carry the script's stdout, stderr, and exit code

> **Status:** decomposing — 2026-05-12
> **Source bead:** `ga-2uizkj` (P2, BUG) — *[gc order] order-tracking
> beads don't capture exec stdout/stderr/exit-code — silent failures
> invisible.*
> **Decomposed into:** 1 builder bead (see Children below)

## Context

Order tracking beads currently signal "success" the moment the order
subsystem's tracking-bead lifecycle completes — **not** the moment the
exec'd script succeeds. This is a forensics hole that masks real
failures.

### The 2026-05-12 incident

Daily standup orders fired at 04:00 PDT. All four created HQ tracking
beads (e.g. `gm-vrd9im` for `daily-standup-beads`) and all four
tracking beads were marked CLOSED with reason `"order dispatch
completed: tracking bead lifecycle finished"`.

But: `daily-standup-beads` runs the script
`/home/jaword/projects/gc-management/bin/daily-standup-request.sh beads`,
and that script did NOT actually create the rig-side standup bead in
the beads rig. The mayor only noticed because re-running the script
manually that morning succeeded and created `be-0dej` cleanly.

Investigation of `gm-vrd9im` showed description and notes both empty.
There is no record of:

- Whether the exec'd command produced any stdout
- Whether it produced stderr
- What exit code it returned
- How long it took

So we cannot answer "did it actually work?" from the tracking bead
alone; we have to re-run the script manually under `bash -x` and hope
the failure mode is reproducible.

### Why it matters beyond this incident

- The 2026-05-11/12 dolt-leak incident (`ga-n29qet`) is partly a story
  of `mol-dog-stale-db` running ineffectually for hours with no
  forensic trail. When that order is fixed to also clean processes
  (see `dolt-test-process-leak-cleanup.md`), the tracking bead is the
  ONLY place a human will find out how many strays it killed.
- `daily-standup-*` failures cascade: ship-gate work has nothing to
  evaluate against if the standup didn't actually create the bead.
- The cron-firing fix (`dynamic-order-discovery-reload.md`) will make
  more orders fire more often; the value of post-hoc forensics scales
  with fire rate.

## Strategy

Single builder bead. The dispatcher already creates a tracking bead
(`cmd/gc/order_dispatch.go:242 trackingBead, err := store.Create(...)`)
and dispatches the script asynchronously
(`cmd/gc/order_dispatch.go:254 go m.dispatchOne(ctx, store, target, a, cityPath, trackingBead.ID)`).
The builder's job is to extend `dispatchOne` (and its peers — there's
likely a formula-dispatch sibling) so that the exec captures:

- **Exit code**
- **Stdout** (head + tail, capped at a configurable size — see below)
- **Stderr** (head + tail, capped at a configurable size)
- **Wall-clock duration**

…and persists them to the tracking bead before closing it. The close
reason should reflect success/failure based on the exit code, not
just lifecycle completion.

## Capture limits

Stdout/stderr can be unbounded (some scripts produce log dumps). The
builder MUST cap captured output so a runaway order can't blow up the
beads store. Recommended (builder may tune):

- **Cap at 16 KiB per stream** (32 KiB total per tracking bead).
- **Head + tail strategy** when output exceeds the cap: keep the first
  8 KiB and the last 8 KiB with a `\n--- [truncated N bytes] ---\n`
  separator in between. The head shows the prelude; the tail shows
  the error message.
- **Add a `gc.order.output_truncated: true` metadata field** on the
  tracking bead when truncation happened so consumers know to look
  elsewhere for the full log.
- **Stream to a file under `.gc/runtime/order-logs/<tracking-bead-id>.log`**
  for unbounded persistence — the bead carries the summary, the file
  carries everything. (Builder MAY defer the file-on-disk part to a
  follow-up if it requires significant runtime plumbing; the bead-side
  capture is the must-have.)

## Where to persist

Tracking-bead **notes** is the natural home for the captured output
(notes are already free-form text). Recommended notes shape:

```
exit_code: <int>
duration_ms: <int>
stdout:
<captured stdout, possibly head+tail truncated>
stderr:
<captured stderr, possibly head+tail truncated>
```

Plain-text, regex-friendly, fits the existing `bd show` rendering.
Avoid JSON — keeps it grep-able and consistent with other bead notes
in this project.

Tracking-bead **description** (currently empty) is where a one-line
verdict goes:

```
ok: daily-standup-beads exec returned 0 in 423ms
fail: mol-dog-stale-db exec returned 1 in 12s (see notes for stderr)
```

The close reason mirrors the verdict.

## Status semantics

Today: every tracking bead is closed with `"order dispatch completed:
tracking bead lifecycle finished"` regardless of exec outcome.

New behavior:

| Exec result          | Close reason                                                | Status |
|----------------------|-------------------------------------------------------------|--------|
| Exit code 0          | `order dispatch completed: exec exit 0`                     | closed |
| Non-zero exit code   | `order dispatch failed: exec exit <code>`                   | closed |
| Timeout (kill)       | `order dispatch failed: exec timed out after <duration>`    | closed |
| Spawn error          | `order dispatch failed: exec could not start: <err>`        | closed |

"closed" status preserved across all outcomes so the existing reaper
logic doesn't change shape. The presence of "failed" in the close
reason is the human-readable failure signal. If you have a status enum
that already distinguishes failure modes (e.g. `closed_failed`), the
builder MAY use it — but adding a new status is out of scope.

## Out of scope

- **Retry-on-failure for exec'd scripts.** The order subsystem does
  not currently retry; this slice doesn't change that. Cron-style
  orders get a fresh fire on next match anyway.
- **A new dashboard view.** `bd show <tracking-id>` rendering the
  notes is sufficient for v1; a richer UI is a follow-up.
- **Structured event-bus emission of exec output.** Real-time event
  consumers can subscribe to a future event type; for now, the bead
  is the persistence point.
- **Formula-dispatch capture for `mol-dog-stale-db`**-style orders.
  If `mol-dog-stale-db` runs as a *formula* rather than `exec`, the
  capture path is different (formula stdout is the agent's output).
  Builder confirms scope at impl time: if formula orders share the
  same dispatch path as exec orders, fine; if they're separate, this
  slice covers `exec` only and the builder files a follow-up bead
  for formula capture (most users care about `exec` first).

## Children

| ID            | Title                                                                                                          | Routing label    | Routes to         | Depends on |
|---------------|----------------------------------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-ync9rq`   | feat(orders): capture exec stdout/stderr/exit-code/duration into tracking bead description+notes (ga-2uizkj)   | `ready-to-build` | `gascity/builder` | (none)     |

## Acceptance for the parent (ga-2uizkj)

Met when the builder bead closes and all of the following hold:

- [ ] `gc order run daily-standup-beads` (or any successful exec
      order) produces a tracking bead with:
      - description: `ok: daily-standup-beads exec returned 0 in <N>ms`
      - notes: includes `exit_code: 0`, `duration_ms: <N>`, and any
        captured stdout/stderr (likely empty for this script).
- [ ] `gc order run <intentionally-failing-order>` (builder adds a
      tiny test order that runs `false` or `exit 1`) produces:
      - description: `fail: <name> exec returned 1 in <N>ms`
      - notes: include `exit_code: 1` and any stderr.
      - close reason: `order dispatch failed: exec exit 1`.
- [ ] An order whose script produces >16 KiB of stdout truncates
      head+tail with a clear separator, and the tracking bead has
      `gc.order.output_truncated: true` in metadata.
- [ ] An order that times out (exceeds `timeout` from the order's
      `.toml`) closes with `order dispatch failed: exec timed out`
      and the killed process leaves no stray (`pgrep` zero for the
      script's command). The capture preserves whatever output the
      script produced before the kill.
- [ ] `gm-vrd9im`-style empty tracking beads no longer occur on
      subsequent daily-standup runs.
- [ ] Existing unit tests in `cmd/gc/order_dispatch_test.go` pass
      with adjustments only to reflect the new tracking-bead shape
      (description+notes content). Add tests for: capture of exit 0,
      capture of exit 1, capture of timeout, capture of truncated
      output, and absence of regression on the existing
      tracking-bead label set.

## Builder design notes

- The capture mechanism is `os/exec`'s `Cmd.Stdout` / `Cmd.Stderr`
  hooked to `bytes.Buffer` writers (for the in-memory cap), wrapped
  in a head+tail truncation helper. There is likely an existing
  exec helper in `gascity/internal/` that already does this for
  agent prompts; reuse it if it fits.
- `bd update <id> --description "..."` and `--notes "..."` are the
  shape the rest of the project uses to mutate beads from Go
  (via the in-process beads library, not the `bd` CLI fork).
  Don't introduce a `bd` CLI fork in the dispatch path.
- **Concurrency.** Multiple orders can fire in the same tick. Each
  has its own tracking bead, so there's no shared-state concern,
  but the builder MUST ensure the bead mutation happens AFTER the
  exec completes (or after the timeout fires) and BEFORE the
  tracking-bead-closed event is published.
- **Don't add a sleep**. If the existing dispatch path closes the
  tracking bead immediately after `go m.dispatchOne(...)` returns
  (i.e., the goroutine launches and `dispatchOne` returns before
  the exec finishes), that's the bug — restructure so the close
  happens in the goroutine, after the exec.

## Verification (for the PM, post-merge)

1. `gc order run daily-standup-beads` and inspect the resulting
   tracking bead. Description should say `ok:`, notes should have
   exit_code and duration.
2. Run a temporary order with `exec = "/usr/bin/false"`. Inspect
   the tracking bead — should say `fail: ... returned 1`.
3. Spot-check `gm-vrd9im`'s peers on the next 04:00 cron run.
4. Cross-link verification with `dynamic-order-discovery-reload.md`:
   once that ships, fires happen more often, so output capture
   gets exercised on the live system, not just unit tests.
