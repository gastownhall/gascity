# Plan: Dolt data-plane resilience design handoff

> Owner: `gascity/pm` - Created: 2026-06-17
> Sources: designer mail `gm-wisp-8yw0u5h`; design beads `ga-ox5oz8`,
> `ga-v5cb1z`, `ga-cnc6sc`, `ga-lhukkq`; architecture root `ga-pqfk8t`

## Goal

Turn the completed Dolt resilience design handoff into builder-ready work
packages while preserving the explicit operator approval gate for the
compactor interval change.

The work addresses four gaps from the Dolt data-plane resilience scope:

- bound Dolt journal growth with a shorter compactor interval;
- add a leading `gc doctor` signal for large Dolt journals;
- document the backup stale threshold RPO constraint;
- provide a crisis-first runbook for fail-closed journal corruption startup.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-ox5oz8.1` | Operator approves 2h Dolt compactor interval | `mayor` | `needs-info` |
| `ga-2ommpw.1` | Design compact-duration alert for mol-dog-compactor | `gascity/designer` | `needs-design` |
| `ga-2ommpw.2` | Build compact-duration alert for mol-dog-compactor | `gascity/builder` | `ready-to-build` |
| `ga-ox5oz8.2` | Build approved 2h Dolt compactor interval | `gascity/builder` | `ready-to-build` |
| `ga-v5cb1z.1` | Build dolt-journal-size doctor check | `gascity/builder` | `ready-to-build` |
| `ga-cnc6sc.1` | Build BACKUP_STALE_S RPO script documentation | `gascity/builder` | `ready-to-build` |
| `ga-lhukkq.1` | Build journal corruption recovery runbook | `gascity/builder` | `ready-to-build` |

## Dependency graph

```text
ga-ox5oz8.1
  -> closed after operator approval

ga-2ommpw.1
  -> blocks ga-2ommpw.2

ga-2ommpw.2
  -> blocks ga-ox5oz8.2

ga-v5cb1z.1
  -> blocks ga-lhukkq.1

ga-cnc6sc.1
  -> no downstream blocker
```

`ga-ox5oz8.2` must not land until the operator approval bead closes,
`ga-ox5oz8` contains the required approval note, and the compact-duration
alert required by that approval is implemented. `ga-lhukkq.1` waits for
`ga-v5cb1z.1` so the recovery runbook does not reference
`dolt-journal-size` before the check exists.

## Acceptance summary

### `ga-ox5oz8.1`

1. Operator reviews the compactor tradeoff on `ga-ox5oz8`: observed
   8.3 GB journal incident, projected 2h bound near 0.7 GB, 12 lock
   acquisitions/day, short write-lock risk, and non-viable `auto_gc`.
2. Operator appends an approval note to `ga-ox5oz8` with CPU impact
   acknowledged, 2h interval accepted, and compact-over-5-minute alert
   coverage confirmed.
3. `ga-ox5oz8` has `operator-approved` and no longer has
   `pending-operator-approval`.

Status update: operator approval was recorded on `ga-ox5oz8` on
2026-06-17 and `ga-ox5oz8.1` is closed. The approval accepted the
5-minute compact-duration alert as a requirement; that alert is tracked
under `ga-2ommpw`.

### `ga-2ommpw.1`

1. Designer defines the operator-visible alert UX for `gc dolt compact`
   runs exceeding 5 minutes.
2. The design specifies the producer location in the existing
   `examples/bd/dolt` order/doctor/health-check structure.
3. The design specifies alert severity, mail/notify/event surface, and
   required context fields.
4. The design includes a builder contract and test expectations.

### `ga-2ommpw.2`

1. Builder implements the designer-specified compact-duration alert.
2. Compact runs over 5 minutes produce an operator-visible alert.
3. Compact runs under 5 minutes do not alert.
4. Tests cover both sides of the threshold.
5. `go test ./...` and `go vet ./...` pass.

### `ga-ox5oz8.2`

1. Builder verifies `ga-ox5oz8.1` is closed and `ga-ox5oz8` contains the
   approval note before editing.
2. Builder verifies `ga-2ommpw.2` is closed before landing the interval
   change, because the operator approval was conditioned on the
   compact-duration alert.
3. Only `examples/bd/dolt/orders/mol-dog-compactor.toml` changes.
4. The interval changes from `24h` to `2h`.
5. The designer-specified audit comment is added above the interval.

### `ga-v5cb1z.1`

1. TDD coverage is written for OK, warning, error, skip, env override,
   and per-database largest-journal cases.
2. `dolt-journal-size` scans `*.journal` files per database, warns at
   4 GB, errors at 6 GB, and never auto-compacts.
3. The check is registered once per city in the managed-Dolt doctor path.
4. `CanFix()` is false and warmup eligibility is false.

### `ga-cnc6sc.1`

1. `examples/bd/dolt/assets/scripts/mol-dog-doctor.sh` gets the RPO
   comment block from the design handoff.
2. `BACKUP_STALE_S` remains `43200` seconds by default.
3. The change is documentation-only.

### `ga-lhukkq.1`

1. `examples/bd/dolt/docs/journal-corruption-recovery.md` is created.
2. The runbook follows the designer's section order and references
   `ga-pqfk8t` for incident context.
3. Quick Reference is a single `bash` fenced code block.
4. Destructive restore and fresh-DB reconstruction paths have explicit
   `[WARNING]` callouts before commands.
5. Verification covers offline `dolt status`, `gc doctor`, and `gc start`.

## Handoff notes

- Tracker import was a no-op: no tracker-to-beads command or sibling
  tracker skill is installed in this worktree.
- WP-2 and WP-3 are immediately ready for builder.
- WP-1 operator approval is complete, but the interval edit now waits on
  `ga-2ommpw.2` because the approval accepted a compact-over-5-minute
  alert as a required companion change.
- Mayor confirmed in `gm-wisp-fr8okoq` that he would not self-approve or
  substitute for the operator acknowledgment; the approval was later
  recorded by the operator on `ga-ox5oz8`.
- WP-4 is routed but blocked on WP-2 so the runbook and doctor check land
  in a coherent order.
- The compact-duration alert requires design first (`ga-2ommpw.1`), then
  builder implementation (`ga-2ommpw.2`). No additional architecture or
  validator bead is required from this handoff.

## Risks

- Merging WP-1 without the compact-duration alert would violate the
  operator's approval condition for the higher compact frequency.
- Building WP-4 before WP-2 could publish recovery guidance that references
  a doctor check not yet available.
- This rig's Beads/Dolt store is local-only; do not run `bd dolt push`,
  `bd dolt pull`, or `gc dolt sync` for this work.
