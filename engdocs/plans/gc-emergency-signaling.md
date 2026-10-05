# Plan: Dolt-independent emergency signaling (`ga-7d6i` family)

> Owner: `gascity/pm-1` · Created: 2026-04-30
> Source: architecture decision `ga-7d6i` (closed)
> Designer addenda: in each child bead's notes

## Why this work exists

When dolt wedges, every agent reporting path fails simultaneously
(`bd update`, `bd close`, `gc mail send`). Agents go silent because
their escalation surface IS the broken substrate. The architect's
decision (`ga-7d6i`) introduces a four-layer dolt-independent path:
filesystem spool (canonical), `events.jsonl` (audit), controller
socket (live fan-out), OS notification (last-mile). Fronted by a new
CLI `gc emergency send`, with a reader CLI (`list`/`ack`/`show`) and
a mayor hook surface for the human-facing read side.

Composes with the user's existing `notify-send -u critical` UX
documented in their global `CLAUDE.md` — agents → OS notification
is the same mechanism in reverse.

## Goal

Restore reachability when the substrate is broken: an agent failing
a `bd update` MUST be able to escalate within ≤ 2 s using a CLI
that touches no dolt-backed state. The mayor MUST see unacked
emergencies on every session-start hook injection. Operators MUST
be able to ack, inspect, and prune via dedicated subcommands.

## Work breakdown

| Bead         | Title                                                  | Priority | Routes to | Gate           |
|--------------|--------------------------------------------------------|----------|-----------|----------------|
| `ga-7d6i.1`  | `gc emergency send` writer + spool + events + OS-notify | P2       | builder   | ready-to-build |
| `ga-7d6i.2`  | `gc emergency list`/`ack`/`show` + mayor hook + doctor prune | P2       | builder   | ready-to-build |
| `ga-7d6i.3`  | Agent prompt-template guidance for `gc emergency send` | P2       | builder   | ready-to-build |

Each child bead's body is the implementation brief — both the
architect's API/data-model spec AND the designer's UX/output-text
spec. No further PM decomposition is needed; the test inventories
and stability contracts are already at builder grain.

## Dependency graph

```
ga-7d6i.1 (writer)  ──────► ga-7d6i.2 (reader+hook+prune)  ──────► ga-7d6i.3 (prompt fragments)
```

Encoded via `bd dep add` so `bd ready` only surfaces a child once
its blocker is closed. Rationale:

- `.2` needs `.1`'s spool format and event types to read records.
- `.3`'s mayor variant references `.2`'s hook injection block.
  Worker variant only needs `.1`'s CLI shape, but landing the whole
  fragment together (worker + mayor) avoids two pack overlay edits.

## Routing rationale

All three children have already been through architect
(`source:actual-architect`) and designer (`source:actual-designer`).
They do not need another design hop.

- **`.1`** — net-new CLI subtree, new package
  (`internal/osnotify`), event-payload registration, controller-
  socket leg. Routed to **builder** with `ready-to-build`. TDD
  unit + integration matrix is in the bead body.
- **`.2`** — extends the same CLI subtree with read paths,
  introduces the hook-injection rendering helper (golden-file
  test), wires `gc doctor` prune, and adds the gascity mayor pack
  overlay step. Routed to **builder** with `ready-to-build`.
- **`.3`** — pack-overlay-only (two new template fragments + ~11
  inclusion-line edits + a pack-loader test). Routed to **builder**
  with `ready-to-build`. No Go code touches role names.

## PM decisions on designer's open questions

### Bead `.2` open questions

1. **`gc emergency list` default `--limit 50`** — confirm.
   Operator can pass `--limit 0` for unlimited.
2. **Persist `acked_by` and `acked_at` into the JSON record at ack
   time (rewrite-then-rename)** — confirm. Avoids cross-referencing
   `events.jsonl` from `show`. The rewrite cost is bounded (single
   <4 KiB record).
3. **Shell completion for `<id>`** — out of scope. File a follow-up
   bead after `.2` lands; tag `kind:enhancement`.
4. **Mayor pack overlay file path stable?** — builder verifies at
   wire-up time. The path lives under
   `pack/overlays/gascity/templates/` family; the loader will fail
   loudly on a missing include.
5. **`gc doctor --emergency-ttl` scope** — emergency-only. The flag
   name's `-emergency-` prefix already declares scope; do not
   couple to other doctor TTLs.

### Bead `.3` open questions

1. **Fragment lives in `gastown` pack** —
   `.gc/system/packs/gastown/template-fragments/` — co-located with
   the templates that include it. Do NOT spin up a new
   `gascity-core` pack.
2. **`boot` agent gets the full fragment** — symmetry over
   minimalism. Boot writes setup beads; the failure mode is real.
3. **Mayor's fragment does NOT add a `--help` pointer** — the hook
   injection block already names the relevant subcommands. Avoid
   redundancy in the static prompt.
4. **Pack-loader test name** — defer to builder. Designer's
   proposal `TestEmergencyFragmentInAllWorkerRoles` is fine; bolt
   onto an existing template-loader test file if one exists.

## Acceptance criteria (rolled up)

The architecture decision and per-bead design briefs carry the full
criteria. Roll-up for stakeholder visibility:

1. **Substrate-failure escalation works.** With dolt stopped,
   `gc emergency send -s critical "<msg>"` exits 0 within ≤ 2 s,
   writes the spool record, and the mayor sees it on the next
   session-start hook injection.
2. **No `internal/beads` import on the writer path.** The
   import-guard test pins it.
3. **Spool records survive controller crash.** Filesystem is the
   canonical store; controller socket and OS-notify are
   best-effort.
4. **Idempotent re-ack.** `gc emergency ack <id>` on an
   already-acked id exits 0 with a "already acked" line.
5. **Path-traversal closed.** `<id>` validation regex
   (`^[0-9TZ\-]+\-[0-9a-f]{8}$`) rejects `../etc/passwd` and any
   non-conforming string before any filesystem syscall.
6. **Hook-injection format stable.** Golden-file test pins the
   render output. Empty list emits no `<system-reminder>` block.
7. **OS-notify dedupe works.** Same severity+actor+message within
   5 minutes fires the OS notification once.
8. **Doctor prune at 7-day TTL.** Acked entries older than
   `--emergency-ttl` (default 168h) are pruned; dedupe markers
   older than 1 day are pruned.
9. **Worker fragment in 10 templates.** `architect, designer, pm,
   builder, validator, witness, deacon, boot, polecat, refinery`
   each contain "When the Reporting Channel Itself Is Broken" in
   their rendered prompt.
10. **Mayor fragment is distinct.** Mayor's rendered prompt
    contains "Surfacing Agent Emergencies" and does NOT contain
    the worker fragment header.

## Risks and unknowns

- **Controller leg blocks the CLI** if the deadline isn't enforced.
  Mitigation: `net.Dialer{Timeout: 1*time.Second}` plus
  `conn.SetDeadline`; integration test brings the controller down
  and verifies the writer still exits 0.
- **macOS `osascript` quoting** when message contains quotes /
  newlines. Mitigation: sanitize at the helper boundary;
  integration test for newline-bearing messages.
- **Mayor pack template change breaks cities pre-`.1` deployment.**
  Mitigation: hook step uses `|| true` so a missing CLI doesn't
  break the chain; pack-loader test pins this fallback.
- **Fragment drift between roles** if a future maintainer copy-
  pastes instead of using `{{ template }}`. Mitigation: pack-
  loader test asserts each role's rendered prompt contains the
  canonical heading; fails loudly on drift.
- **Cap-reached degraded behavior** in long-lived supervisors with
  many warning categories. Bound to ~1000 keys with LRU; document
  the cap. (This risk applies to `ga-q0bf.1`'s `Dedup` type, but
  the same `internal/logutil` package will house this code, so
  PM-level visibility matters.)

## Out of scope (explicit)

- API-server subscription to the controller's emergency channel.
  The controller fans out to a buffered channel; no listener
  subscribes yet. File as `kind:enhancement` follow-up.
- Bulk-ack-by-filter (`gc emergency ack --all-older-than 1h`).
  Defer until operator load justifies it.
- Auto-ack heuristics — would violate ZFC.
- Persistent dedupe markers across host reboots beyond mtime.
- Localization of the prompt fragments — English-only for now.
- Updating non-gascity packs — they can opt into the fragment
  voluntarily; document the opt-in path in the gascity README.

## Validation gates

- `go test ./...` green (includes new unit + integration tests).
- `go vet ./...` clean.
- `TestEveryKnownEventTypeHasRegisteredPayload` covers
  `EmergencySignaled` + `EmergencyAcked`.
- Import-guard test asserts `cmd_emergency.go` does NOT
  transitively import `internal/beads`.
- Pack-loader test asserts the fragments are included correctly.
- Manual smoke: stop dolt, run `gc emergency send -s critical
  "test"`, verify the mayor's next session shows the unacked-
  signal block and `gc emergency ack <id>` clears it.
- One `bd remember` entry from the builder per bead when each
  lands so future maintainers learn the format from `bd prime`,
  not from archaeology.
