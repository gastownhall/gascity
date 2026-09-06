# Plan: macOS launchd postgres local bootstrap (ga-hag81j)

> **Status:** decomposed - 2026-05-17
> **Source design:** `ga-hag81j` - macOS launchd analogue for local
> PostgreSQL bootstrap.
> **Design parent:** `ga-7nwr` - Linux systemd-user bootstrap design.
> **Decomposed into:** 3 builder beads.

## Context

The designer contract in `ga-hag81j` pins the macOS LaunchAgent analogue
of the Linux `engdocs/postgres-local-bootstrap.md` runbook. The PostgreSQL
setup stays the same as the Linux slice: private user-owned data directory,
loopback `127.0.0.1:5433`, generated password, and
`~/.config/beads/credentials`. The platform-specific work is the service
layer: a launchd LaunchAgent at
`~/Library/LaunchAgents/com.beads.postgres.plist`.

The downstream work should preserve the designer's literal text contract.
FixHint strings, idempotency messages, doc headings, and explain-output
bytes are acceptance-sensitive and must not be paraphrased.

## Children

| ID | Title | Routing label | Routes to | Depends on |
|----|-------|---------------|-----------|------------|
| `ga-hag81j.1` | As a macOS developer, I can bootstrap local PG with a launchd runbook | `ready-to-build` | `gascity/builder` | - |
| `ga-hag81j.2` | As a macOS operator, gc doctor points me at the launchd bootstrap when local PG is missing | `ready-to-build` | `gascity/builder` | `ga-hag81j.1` |
| `ga-hag81j.3` | As a gc user, I can print the macOS postgres bootstrap from gc doctor | `ready-to-build` | `gascity/builder` | `ga-hag81j.1` |

## Acceptance For Parent

The parent is complete when all three child beads close and these outcomes
hold:

- `engdocs/postgres-macos-launchd-bootstrap.md` exists with the verbatim
  body from `ga-hag81j` section 1, including frontmatter and sections
  1 through 11.
- The macOS runbook uses launchd LaunchAgent semantics, Homebrew-scoped
  PostgreSQL guidance, `lsof` port checks, loopback port 5433, and the
  credentials file at `~/.config/beads/credentials`.
- `internal/lints/postgres_macos_bootstrap_test.go` covers the doc contract:
  frontmatter, section order, `bash -n` over fenced shell blocks, plist path
  consistency, unquoted credentials heredoc, port pinning, and rejection of
  `ss -tln`, `systemctl`, and `loginctl`.
- `internal/doctor/checks_postgres.go` includes the macOS plist probe helper
  and a Darwin FixHint branch that returns
  `local PG not installed yet — see engdocs/postgres-macos-launchd-bootstrap.md for one-time setup`
  for Darwin + loopback failure + missing LaunchAgent plist.
- Plist-present, non-Darwin, warning-only, and OK cases preserve existing
  doctor behavior; `CanFix()` remains false.
- `gc doctor --explain-postgres-macos-launchd-bootstrap` prints the macOS
  bootstrap doc body byte-for-byte, exits zero, emits no stderr, and does
  not run normal doctor checks.
- The targeted suites from `ga-hag81j` pass:
  `go test ./internal/lints/ -run "TestPostgresMacOSBootstrapDoc" -count=1`,
  `go test ./internal/doctor/ -run "TestPostgresServerFixHint_Darwin|TestBeadsPostgresMacOS" -count=1`,
  and `go test ./cmd/gc/ -run "TestExplainPostgresMacOSBootstrap" -count=1`.
- `go test ./...` and `go vet ./...` are clean before final merge.

## Builder Notes

`ga-hag81j.1` is the base dependency because both the doctor FixHint and
the explain flag point at the new engdoc. It should land the operator-facing
document and doc-only lint tests first.

`ga-hag81j.2` owns the doctor behavior. It should use pure filesystem probes
and test seams consistent with the existing postgres doctor helpers. The
Darwin branch belongs after the Linux bootstrap branch and before the base
fallthrough.

`ga-hag81j.3` owns command wiring for the explain flag. It should follow
the existing doctor explain patterns and prove output equality against the
source document.

## Out Of Scope

- LaunchDaemon or root-owned macOS service installation.
- Auto-repair or `gc doctor --fix` behavior.
- MacPorts-specific path handling.
- Docker-compose, podman-compose, OpenRC, runit, or s6 bootstrap variants.
- Any changes to PostgreSQL authentication resolution beyond the doc and
  doctor surfaces explicitly named by `ga-hag81j`.

## Risks

- Explain-output implementation must avoid doc drift. The tests should make
  byte equality against the source file visible immediately.
- The lint suite spans doc and code references. If the builder splits commits,
  tests should be staged so each bead remains green when landed.
- The FixHint branch must not probe macOS plist paths on non-Darwin systems;
  the non-Darwin no-probe test is load-bearing for CI portability.
