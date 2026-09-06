# Plan: Lenient validation mode for `gc stop` (`ga-r8iz` family)

> Owner: `gascity/pm-1` · Created: 2026-04-30
> Source: architecture decision `ga-r8iz` (closed)
> Designer addendum: in `ga-r8iz.1` notes

## Why this work exists

`gc stop` shares `loadCityConfig` with `gc start`, so any
pack-validation error that prevents the city from starting also
prevents the city from stopping. Operators end up
`kill -9`-ing the supervisor — a pattern that bypasses our own
shutdown hooks and leaves the runtime dirty. The architect's
decision (`ga-r8iz`) introduces a `LoadMode` enum (Strict / Lenient)
and switches `gc stop` to lenient by default, with `--validate` to
opt back into strict and `--kill-timeout` to bound zombie waits.

## Goal

`gc stop` always succeeds at stopping the supervisor unless the
kernel itself prevents it. Config errors degrade to warnings, not
refusals. Operators get a predictable, scriptable shutdown that
doesn't require `kill -9` even when the on-disk config is broken.

## Work breakdown

| Bead         | Title                                  | Priority | Routes to | Gate           |
|--------------|----------------------------------------|----------|-----------|----------------|
| `ga-r8iz.1`  | Implement lenient validation mode for `gc stop` | P2       | builder   | ready-to-build |

Single coherent implementation unit. Architect+designer have
specified:

- The `LoadMode` enum and `loadCityConfig` signature change.
- The `/proc`-based supervisor discovery fallback (with two-factor
  PID match: PID + exe path).
- The SIGTERM → SIGKILL escalation with `--kill-timeout`.
- The output text and stability contract for log scrapers.
- The `--help` design with named exit codes.
- The complete unit + integration test matrix.

No further PM decomposition is needed. The bead body IS the
implementation plan.

## Routing rationale

`source:actual-architect`, `source:actual-designer`,
`source:actual-planner` already on the bead — the pipeline has
done its work. Routed to **builder** with `ready-to-build`. TDD
discipline applies: bead's test matrix is authored alongside the
code.

## PM decisions on designer's open questions

1. **Stdout silent; all narration on stderr** — confirm.
   Scriptability wins; existing project convention matches.
2. **`--quiet` flag** — defer. Operators can `2>/dev/null` if
   needed. Add only if a real ask emerges.
3. **Zombie hint specificity** — keep the parenthesised
   speculation ("kernel D-state? swap? container?") as designer
   wrote it. Short, signalling, doesn't commit to a wrong
   diagnosis.
4. **`--kill-timeout=0` suppresses the SIGTERM line** — confirm.
   The output should reflect that no SIGTERM was sent.

## Acceptance criteria (rolled up)

1. **Broken-config stop succeeds.** With an unparseable
   `pack.toml`, `gc stop` exits 0 after warnings; the supervisor
   process is gone.
2. **Two-factor PID match prevents misfire.** `/proc/<pid>/exe`
   resolved consistently between probe and signal — different
   process at the same PID after rollover MUST NOT receive a
   signal. Pinned by integration test.
3. **`--validate` preserves strict behavior.** With strict mode
   and an invalid config, exit code is 1 and the supervisor is
   NOT signaled. Errors render with the `gc stop:` prefix, not
   `warning:`.
4. **`--kill-timeout` honored.** Default 30s; SIGKILL fires at
   the deadline; `(forced)` suffix appears on the city-stopped
   line.
5. **Zombie escape exits non-zero.** After 2× kill-timeout with
   the supervisor still running, exit 1 with the "giving up on
   pid …" message. systemd's restart loop will catch it.
6. **Idempotent re-stop.** Running `gc stop` on an already-
   stopped city exits 0 with `(already stopped)` note.
7. **Output stability contract.** Every line begins `gc stop: `
   or `warning: `. Final line on success is exactly
   `gc stop: city stopped` (or `gc stop: city stopped (forced)`).
8. **No new status files.** Discovery is via `/proc` queries and
   env-stamped `argv`, never a written-out PID file.
9. **Other commands keep strict mode.** Unit test pins each
   command's `LoadMode` so `gc reload` can't accidentally inherit
   lenient via a future refactor.

## Risks and unknowns

- **Two-factor PID match misfires** if `/proc/<pid>/exe` resolves
  differently between probe and signal (symlink replaced
  mid-flight). Mitigation: read once and pin; reject if exe
  differs at signal time.
- **systemd unit restart-loops** if `gc stop` exits before
  `/proc/<pid>` disappears. Mitigation: wait for `/proc/<pid>` to
  vanish before exit.
- **Lenient mode masks a real config break operator needed to
  see.** Mitigation: every error is rendered as `warning:` —
  the operator still sees the issue; only the refusal-to-act
  is gated.
- **`gc reload` accidentally inherits lenient** during a future
  refactor. Mitigation: pin via unit test (acceptance #9).

## Out of scope (explicit)

- Standalone `gc kill` command — folded into
  `gc stop --kill-timeout=0`.
- Lenient mode for any command other than `gc stop`.
- Cross-host supervisor stop (only local).
- ANSI color output; all severity is carried by leading words.

## Validation gates

- `go test ./...` green; new unit + integration tests included.
- `go vet ./...` clean.
- Integration test with broken `pack.toml` confirms exit 0 and
  process is gone.
- Output stability check: `grep -E '^(gc stop|warning):' <output>`
  matches every line.
- Manual smoke: edit `pack.toml` to introduce a syntax error;
  run `gc stop`; confirm clean shutdown.
- One `bd remember` entry from the builder when this lands so
  future maintainers learn the LoadMode pattern from `bd prime`,
  not from archaeology.
