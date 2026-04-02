# Anti-Patterns — What Breaks

Things that have caused incidents, wasted time, or confused agents. Avoid these.

### Running `next dev` in Docker containers (2026-03-28)
Next.js dev mode inside a Docker container causes infinite recompile loops. The planogram dashboard hit 38,000% CPU. Always use production builds (multi-stage Dockerfile: node build -> nginx serve) for containerized frontends.
Source: vap dashboard incident. See memory: project_ulimit_fix.md.

### Treating gastown/beads/mesh as user projects (2026-03-31)
These are TOOLS, not products. Don't create beads in the gastown rig for town infrastructure work. Town-level work uses de- prefix beads. User's actual products: officeworld, deepwork_site, villa_alc_ai, villa_ai_planogram, etc.
Source: User correction during de-9s0 session.

### Excessive GitHub API calls from agents (2026-03-07)
6+ agents hitting GitHub API simultaneously got the account suspended. Never use GitHub for agent coordination. Use Gitea locally. GitHub is public mirror only, with zero agent API calls.
Source: freebird-ai suspension.

### `kill -QUIT` on Dolt (2026-03-30)
SIGQUIT kills the Dolt server — it does NOT produce a goroutine dump like in standard Go programs. Dolt overrides the signal handler. Use `gt dolt status` for diagnostics instead.
Source: Dolt incident 2026-03-30.

### Orphaned mesh sync crons spawning thousands of dolt processes (2026-03-31)
Mesh sync crons (every 2 min) each spawn a dolt subprocess. If the subprocess hangs (e.g., due to ulimit), cron spawns another. This created 9000+ zombie dolt processes on the host. Fix: raise ulimit AND add process-already-running guards to cron scripts.
Source: de-9s0 investigation.

### Slinging town-level beads (de-) to rigs (2026-03-31)
`gt sling de-xxx <rig>` fails by design. The bead prefix must match the rig's database. Create beads with `bd create --rig <rigname>` to get the correct prefix.
Source: de-2yd, de-yn5 investigation.

### Using `go build` instead of `make build` for gt binary (2026-04-01)
Direct `go build` skips ldflags that set BuiltProperly=1, version, and commit hash. The binary works but prints warnings and may behave differently in production code paths that check BuiltProperly.
Source: de-9s0 execution.
