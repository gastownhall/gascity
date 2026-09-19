# Plan: Supervisor warning dedup + FATAL clarity in `gc start` (`ga-q0bf` family)

> Owner: `gascity/pm-1` · Created: 2026-04-30
> Source: architecture decision `ga-q0bf` (closed)
> Designer addendum: in `ga-q0bf.1` notes

## Why this work exists

`gc start` floods operator stderr with the same supervisor warning
repeated dozens of times, drowning a single FATAL line that the
operator actually needs to see. The architect's decision (`ga-q0bf`)
adds an in-process `Dedup` type for warnings (single supervisor
process) plus a `gc-fatal:` marker to make the FATAL line TTY-
distinguishable when proxied through `gc start`. `--verbose`
disables both dedup levels for log-scraper / debugging use cases.

## Goal

When `gc start` fails, the operator sees the cause-of-death within
the first screen of stderr without scrolling. Repeated warnings are
visually compressed; the FATAL line is unmissable; the dedup is
disable-able for scraper scripts.

## Work breakdown

| Bead         | Title                                       | Priority | Routes to | Gate           |
|--------------|---------------------------------------------|----------|-----------|----------------|
| `ga-q0bf.1`  | Supervisor warning dedup + FATAL distinction in `gc start` output | P2       | builder   | ready-to-build |

Single coherent implementation unit. Architect+designer have
specified:

- The `internal/logutil` package surface (`Dedup`, `markers.go`).
- Warning-emission-site updates in `internal/orders/discovery.go`.
- The CLI proxy in `cmd/gc/cmd_start.go` with TTY-aware FATAL
  rendering.
- The `--verbose` and `--color={auto,always,never}` flags.
- Output stability contract (four prefix patterns).
- The complete unit + regression test matrix.

No further PM decomposition is needed.

## Routing rationale

`source:actual-architect`, `source:actual-designer`,
`source:actual-planner` already on the bead. Routed to **builder**
with `ready-to-build`.

## PM decisions on designer's open questions

1. **Add `--color={auto,always,never}` flag** — confirm. Git-
   parity is the right convention; `--color=always` for piped
   output that wants ANSI (e.g., `less -R`).
2. **Honour `NO_COLOR` env var** — confirm. Freedesktop
   convention; cheap to support.
3. **Emit-at-flush for the dedup count** — confirm designer's
   choice. The trade-off (warning lines appear after supervisor
   settles, not in real-time) is acceptable for a startup phase
   bounded by seconds. Revisit if operators complain about real-
   time visibility.
4. **Suppression hint on the first occurrence with final count** —
   confirm. Designer's choice avoids the visual noise of two
   annotations.
5. **`gc reload` / `gc restart` inherit the proxy?** — out of
   scope for this bead. File `kind:enhancement` follow-up after
   `ga-q0bf.1` lands. Factor the proxy so future expansion is a
   one-line change rather than a refactor.

## Acceptance criteria (rolled up)

1. **Wall-of-warnings collapsed.** A startup that today emits 41
   identical warnings now emits exactly one warning line with
   `(suppressed 40 more)` suffix.
2. **FATAL line is the last loud line** of `gc start` output. No
   warnings emit after the FATAL; only quiet `gc start: …`
   procedural narration may follow.
3. **TTY rendering of FATAL.** On `isatty(stderr)` true, the
   FATAL line uses bold red (`\x1b[1;31m … \x1b[0m`) with the
   prefix `FATAL:`. On non-TTY, the prefix is `gc-fatal:` with
   no ANSI codes.
4. **`NO_COLOR=1` and `--color=never`** force non-TTY rendering
   even on a TTY. `--color=always` forces TTY rendering even off
   a TTY. Flag wins over env var.
5. **`--verbose` disables both dedup levels.** Every warning line
   emitted by the supervisor reaches operator stderr unchanged.
6. **`Dedup` cap at ~1000 keys with LRU eviction.** A single meta-
   warning (`warning: dedup cap reached; …`) emits when eviction
   begins.
7. **Output stability.** Four prefix patterns frozen:
   `^gc start: `, `^warning: `, `^FATAL: ` (TTY) /
   `^gc-fatal: ` (non-TTY), `\(suppressed \d+ more\)` suffix.
8. **Regression test (FR-6).** With a config that triggers N
   deprecated-order-path warnings + a fatal, the output's line
   count equals `unique-warning-count + (header + fatal)`; last
   loud line is the FATAL.

## Risks and unknowns

- **`Dedup` memory grows unbounded** in long-lived supervisors.
  Mitigation: bound to ~1000 keys with LRU eviction. The cap-
  reached meta-warning makes degradation visible.
- **Marker collides with user log content.** `gc-fatal:` is
  unique enough; revisit if a real collision is reported.
- **Output ordering between stdout and stderr** breaks the
  "fatal is last loud line" guarantee. Proxy captures and re-
  emits with a single sink; test under heavy interleaving.
- **`--verbose` regression** — easy for a future emission site
  to skip the dedup helper. Mitigation: route all supervisor
  warnings through one helper; lint or test for direct stderr
  writes.
- **Suppression hint wrong on long startups.** Emit-at-flush
  means the warning line lands after the supervisor settles;
  during a 10s startup the operator sees nothing for 10s. Trade
  acceptable; revisit if real complaints surface.

## Out of scope (explicit)

- Persistent dedup across supervisor restarts — recurrence is
  informative and we want operators to see it.
- Structured-log support (JSON output mode) — future work.
- Operator-side alerting (Slack, PagerDuty) — pure local UX.
- Extending the proxy to `gc reload` / `gc restart` — file as
  follow-up after `ga-q0bf.1` lands.
- ANSI true-color or 256-color palette — SGR is the common
  denominator.

## Validation gates

- `go test ./...` green.
- `go vet ./...` clean.
- Regression test on a config triggering ≥30 duplicate warnings +
  a fatal; assert the line count and last-line invariant.
- Manual smoke: simulate broken pack on a real city; run
  `gc start` and `gc start --verbose`; confirm both behaviors
  visually.
- Manual smoke under `NO_COLOR=1` and `--color=always` to confirm
  override precedence.
- One `bd remember` entry from the builder when this lands so
  future maintainers learn the marker pattern from `bd prime`,
  not from archaeology.
