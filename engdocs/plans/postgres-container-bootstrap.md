# Plan: container-runtime postgres local bootstrap (ga-63a79l)

> **Status:** decomposed - 2026-05-17
> **Source design:** `ga-63a79l` - docker-compose / podman-compose
> PostgreSQL bootstrap alternative.
> **Design parent:** `ga-7nwr` - Linux systemd-user bootstrap design.
> **Decomposed into:** 3 builder beads.

## Context

The designer contract in `ga-63a79l` pins a container-runtime alternative
to the host-installed local PostgreSQL bootstraps. PostgreSQL still appears
to bd-backed scopes at `127.0.0.1:5433` and still uses the slice-2
credentials file at `~/.config/beads/credentials`, but initialization and
lifecycle are owned by `docker compose` or `podman-compose`.

The runbook uses the official `postgres:16` image, a bind mount at
`~/.local/share/beads/postgres/data`, a compose file at
`~/.config/beads/docker-compose.yml`, and a mode-600 `.env` file for the
initial password. The doctor amendment is a fallback after host-service
amendments: Linux systemd, non-systemd Linux, and macOS launchd all take
precedence when their conditions fire.

## Children

| ID | Title | Routing label | Routes to | Depends on |
|----|-------|---------------|-----------|------------|
| `ga-63a79l.1` | As a Docker or Podman user, I can bootstrap local PG with a compose runbook | `ready-to-build` | `gascity/builder` | - |
| `ga-63a79l.2` | As an operator, gc doctor points me at the container bootstrap when local PG is not running | `ready-to-build` | `gascity/builder` | `ga-63a79l.1`, `ga-hag81j.2`, `ga-soulka.2` |
| `ga-63a79l.3` | As a gc user, I can print the container postgres bootstrap from gc doctor | `ready-to-build` | `gascity/builder` | `ga-63a79l.1` |

## Acceptance For Parent

The parent is complete when all three child beads close and these outcomes
hold:

- `engdocs/postgres-container-bootstrap.md` exists with the verbatim body
  from `ga-63a79l` section 1, including frontmatter and sections 1 through 8.
- The compose runbook uses `postgres:16`, `restart: unless-stopped`,
  `127.0.0.1:5433:5432`, `~/.config/beads/docker-compose.yml`,
  `~/.config/beads/.env`, and `chmod 600` for both `.env` and credentials.
- `internal/lints/postgres_container_bootstrap_test.go` covers frontmatter,
  section order, `bash -n` over fenced shell blocks, intentional port usage,
  env-file mode, restart policy, image tag, credentials heredoc, and the
  FixHint/doc reference.
- `internal/doctor/checks_postgres.go` includes
  `beadsPostgresContainerRunning()` with a 5-second timeout, docker-to-podman
  fallback, and a test seam for command execution.
- `postgresServerFixHint` returns
  `local container runtime PG not running — see engdocs/postgres-container-bootstrap.md for setup`
  for loopback failures when the named container is not running, unless a
  host-service amendment takes precedence.
- Container-running, no-loopback, all-OK, timeout/error, and host-amendment
  precedence cases preserve existing doctor behavior; `CanFix()` remains false.
- `gc doctor --explain-postgres-container-bootstrap` prints the container
  bootstrap doc body byte-for-byte, exits zero, emits no stderr, and does
  not run normal doctor checks.
- The targeted suites from `ga-63a79l` pass:
  `go test ./internal/lints/ -run "TestPostgresContainerBootstrapDoc" -count=1`,
  `go test ./internal/doctor/ -run "TestPostgresServerFixHint_Container|TestBeadsPostgresContainerRunning" -count=1`,
  and `go test ./cmd/gc/ -run "TestExplainPostgresContainerBootstrap" -count=1`.
- `go test ./...` and `go vet ./...` are clean before final merge.

## Builder Notes

`ga-63a79l.1` is the base dependency because both the doctor FixHint and
the explain flag point at the new engdoc.

`ga-63a79l.2` is intentionally blocked by `ga-hag81j.2` and `ga-soulka.2`.
The container amendment belongs after the host-installed postgres amendments:
Linux systemd first, non-systemd Linux second, macOS launchd third, and
container fallback fourth.

`ga-63a79l.3` owns command wiring for the explain flag. It should follow the
existing doctor explain patterns and prove output equality against the source
document.

## Out Of Scope

- Custom PostgreSQL images.
- Named Docker volumes.
- Kubernetes, Swarm, or multi-container stacks.
- Automatic Docker boot-enable or system service changes.
- Docker secrets or Swarm-only password handling.
- Any `gc init` or `gc destroy` automation.

## Risks

- The container helper is the only postgres doctor bootstrap probe that needs
  `os/exec`; the timeout and graceful degradation tests are required to keep
  doctor from hanging.
- Docker and Podman behavior differ around rootless bind mounts. The runbook
  should stay within the pinned Docker-primary contract and document Podman
  only as the specified compatible path.
- The FixHint ordering can regress as the platform amendments land. The
  cross-design blockers encode the intended merge order for the doctor branch.
