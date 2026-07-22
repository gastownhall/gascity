# Live-Town Acceptance — human-gate notification (sc-lcwsqf increment 4)

**Verdict: BOTH capabilities pass live acceptance on a REAL town human gate.**
Created a real `type=human` gate in the running `sct-city` town, drove the two
deliverable order-scripts against the live `gc` CLI exactly as the controller's
exec-order dispatch would, and captured the real mail+nudge receipts. Full CLI
transcript: [`ACCEPTANCE-receipts/live-acceptance-transcript.txt`](ACCEPTANCE-receipts/live-acceptance-transcript.txt).

## What was exercised (and why this is "live")

The two orders are standalone bash that drive the existing `gc` surface
(`gc events`, `gc bd show`, `gc bd gate list`, `gc mail send --notify`,
`gc rig list`). Running them by hand against the running town is a faithful
exercise of what the controller does when it fires the order — same store, same
mail system, same nudge path — without needing to rebuild/redeploy the town
binary. Dedup state was pointed at a scratch dir so the run is hermetic on the
state side while every store/mail effect is real.

- **Live gate:** `sc-f5bf8h` (`type=human`, blocking throwaway `sc-tzkae6`),
  addressee set to `claude-3` — a **real live session** (me), so the full
  mail **and** nudge path is exercised with a blast radius of one session.
- **Town scopes swept:** HQ `sct-city` + rig `dip` (real `gc rig list`).

## Capability 1 — notify-on-human-gate-creation

| Property | Evidence (transcript step) | Result |
|---|---|---|
| Gate create emits NO notification (the gap) | 1b — `Created gate … Resolve with: bd gate resolve`, nothing else | confirmed gap |
| `bead.created` carries `issue_type=gate`, NOT `await_type` (⇒ re-fetch needed) | 1e / Appendix A | confirmed |
| Script notifies the addressee once | 2b — `notified 1 human gate addressee(s)`, exit 0 | **PASS** |
| Real mail lands in the addressee inbox | 2c/2d — `Human gate awaiting you: sc-f5bf8h`, full body + resolve line | **PASS** |
| `gc mail send --notify` delivers (+nudges a real session) | 2e — `Sent message … to claude-3`, exit 0 | **PASS** |
| Idempotent — notified at most once | 2f/2g — state records the gate; re-run notifies 0 | **PASS** |

## Capability 2 — renudge-stale-human-gates

| Property | Evidence (transcript step) | Result |
|---|---|---|
| Re-fires once the gate ages past threshold | 3a — `re-notified 1 stale human gate addressee(s)` | **PASS** |
| Re-fire mail lands with age + resolve line | 3b — `Reminder — human gate still open: sc-f5bf8h … open and unresolved for 0h4m` | **PASS** |
| Cadence: suppressed within `RENUDGE_INTERVAL` | 3c — re-run sends 0 | **PASS** |
| Cadence: repeats after the interval elapses | 3d — with `INTERVAL=1s` re-fires | **PASS** |
| **Zero-spam under production defaults** | 3e — default `1h` threshold across HQ+dip sends **0** | **PASS** |

**Safety context (3f):** at run time the live town had HQ = 1 open human gate
(the 4-minute-old test gate) and rig `dip` = 52 open gates, **0** of them human
(all legacy `await_type=null` workflow gates). Under the production `1h`
threshold the sweep therefore sent **zero** mail — the fresh gate is not yet
stale and every legacy gate is excluded by the `await_type=="human"` filter.
This is the real-machinery confirmation of increment 3's isolated zero-spam claim.

## Loud-fail note (#4543)

Not forced live (the addressee was deliverable), but the mechanism is visible in
both scripts: the `notified/re-notified N` counters increment **only** on a
successful `gc mail send --notify`; an undeliverable send takes the `else` branch,
prints `FAILED … (will retry next sweep)` to stderr, and is **not** recorded in
dedup state, so the next sweep retries it. Isolated-store proof already exercised
the surface+not-recorded+retry path (increment 3, 12/12).

## Cleanup

Test gate `sc-f5bf8h` resolved and throwaway target `sc-tzkae6` closed
(transcript Step 4); HQ verified back to **0** open human gates. No litter left
in the live town.

## Remaining on sc-lcwsqf (drives later increments)

- (b) Doc adversarial pass + independent Codex CLI review on the final diff +
  this proof pack; findings captured on-bead, each addressed or rebutted.
- (c) Canonical PR per the #4543 house arc (repo template, narrow-scope
  rationale, tests, explicit Proof section citing this acceptance run).
