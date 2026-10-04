# Plan: non-systemd Linux postgres local bootstrap (ga-soulka)

> **Status:** decomposed - 2026-05-17
> **Source design:** `ga-soulka` - OpenRC / runit / s6 PostgreSQL
> bootstrap support.
> **Design parent:** `ga-7nwr` - Linux systemd-user bootstrap design.
> **Decomposed into:** 3 builder beads.

## Context

The designer contract in `ga-soulka` pins one shared runbook for Linux
systems that do not use systemd: OpenRC, runit, and s6. The common postgres
setup mirrors the systemd-user runbook: user-owned data directory,
loopback port 5433, generated role password, and
`~/.config/beads/credentials`. Only service installation differs, and that
difference is isolated to section 6 subsections for each init system.

The doctor amendment handles Linux hosts where `/run/systemd/system` is
absent and none of the known non-systemd service files exists.

## Children

| ID | Title | Routing label | Routes to | Depends on |
|----|-------|---------------|-----------|------------|
| `ga-soulka.1` | As a non-systemd Linux developer, I can bootstrap local PG with one OpenRC/runit/s6 runbook | `ready-to-build` | `gascity/builder` | - |
| `ga-soulka.2` | As a non-systemd Linux operator, gc doctor points me at the right PG bootstrap | `ready-to-build` | `gascity/builder` | `ga-soulka.1` |
| `ga-soulka.3` | As a gc user, I can print the non-systemd postgres bootstrap from gc doctor | `ready-to-build` | `gascity/builder` | `ga-soulka.1` |

## Acceptance For Parent

The parent is complete when all three child beads close and these outcomes
hold:

- `engdocs/postgres-non-systemd-linux-bootstrap.md` exists with the verbatim
  body from `ga-soulka` section 1, including frontmatter and sections
  1 through 11.
- Section 6 contains the required subsections for `### 6.1 OpenRC`,
  `### 6.2 runit`, and `### 6.3 s6`, with service paths matching the
  designer contract.
- The shared runbook keeps PostgreSQL data, port, credentials, password,
  and manual bootstrap behavior aligned with the Linux systemd-user runbook.
- `internal/lints/postgres_non_systemd_bootstrap_test.go` covers
  frontmatter, section order, section 6 subsections, `bash -n` over fenced
  shell blocks, FixHint/doc reference, port pinning, credentials heredoc,
  no `systemctl` or `loginctl`, and OpenRC/runit/s6 path references.
- `internal/doctor/checks_postgres.go` includes the three test-overridable
  service path vars and `beadsPostgresNonSystemdServiceInstalled()`.
- The helper returns true if any OpenRC, runit, or s6 service file exists,
  false when none exist, false without probing on non-Linux or Linux+systemd,
  and `(false, err)` on user lookup failure.
- `postgresServerFixHint` returns
  `local PG not installed yet — see engdocs/postgres-non-systemd-linux-bootstrap.md for one-time setup`
  for Linux + non-systemd + loopback failure + no service file.
- Linux systemd, service-present, non-Linux, warning-only, and OK cases
  preserve existing doctor behavior; `CanFix()` remains false.
- `gc doctor --explain-postgres-non-systemd-linux-bootstrap` prints the
  non-systemd bootstrap doc body byte-for-byte, exits zero, emits no stderr,
  and does not run normal doctor checks.
- The targeted suites from `ga-soulka` pass:
  `go test ./internal/lints/ -run "TestPostgresNonSystemdBootstrapDoc" -count=1`,
  `go test ./internal/doctor/ -run "TestPostgresServerFixHint_NonSystemd|TestBeadsPostgresNonSystemd" -count=1`,
  and `go test ./cmd/gc/ -run "TestExplainPostgresNonSystemdBootstrap" -count=1`.
- `go test ./...` and `go vet ./...` are clean before final merge.

## Builder Notes

`ga-soulka.1` is the base dependency because both the doctor FixHint and
the explain flag point at the new engdoc.

`ga-soulka.2` owns the non-systemd doctor behavior. Its FixHint branch belongs
after the Linux systemd bootstrap branch and before the macOS launchd branch.
The service-installed helper is filesystem-only; no command execution is
needed.

`ga-soulka.3` owns command wiring for the explain flag. It should follow the
existing doctor explain patterns and prove output equality against the source
document.

## Out Of Scope

- Windows or non-systemd WSL2.
- Daemontools-specific instructions.
- s6-rc-specific wiring.
- System-wide distro PostgreSQL services under `/var/lib/postgresql`.
- Auto-boot setup beyond the documented runit/s6 notes.
- Any `gc init`, `gc destroy`, or `gc doctor --fix` automation.

## Risks

- s6 service layout varies by distro. The runbook should keep the default
  `~/.s6/service` path and the documented operator override without expanding
  the implementation scope.
- The no-systemd predicate must stay mutually exclusive with the existing
  Linux systemd branch, or operators will see unstable FixHint precedence.
- The lints should treat the s6 `execlineb` block as non-bash, matching the
  designer's test contract.
