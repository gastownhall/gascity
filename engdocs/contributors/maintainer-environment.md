# Maintainer Environment

Conventions specific to the maintainers' shared build hosts and internal
work ledger. External contributors do not need any of this: GitHub Issues
is the public tracker, and a normal `go build` / `make` setup is enough.

## Shared build hosts

**Hard ban: never run `go clean -cache`** in any script, hook, or agent session.

Running `go clean -cache` against a shared `GOCACHE` (the default when
`$GOCACHE` is not overridden) corrupts the fleet-wide build cache for every
concurrent executor. Each executor that hits a missing cache entry then runs a
full rebuild, and any that calls `go clean -cache` mid-flight invalidates all
the others' in-progress caches. The incident (vp-g96b, 2026-06-13) produced
~10 cascading cache-miss errors across the executor pool.

**Just run `go build` / `make` — do NOT set `GOCACHE` yourself.** The host `go`
shim already routes the default `GOCACHE` to a shared **on-disk** cache
(`~/.cache/go-build`) and pins compile/link temp to disk
(`GOTMPDIR=/var/tmp/gotmp`). A warm shared cache is faster and is never
corrupted by a normal build.

**Never point `GOCACHE` (or `TMPDIR`) at `/tmp`.** `/tmp` is a size-capped
RAM-backed tmpfs (61G) shared by the whole fleet — including the harness's
tool-output capture dir. A bare `mktemp -d` (no `-p` dir) resolves against the
unset `$TMPDIR`, which defaults to `/tmp` — one cold cache built there is
2-3GB, and a concurrent build wave fills tmpfs and ENOSPCs every agent
on the host (incident gm-tkz1r / ga-x9k9b9, 2026-07). The shim deliberately
does **not** relocate a `GOCACHE` you set explicitly, so an explicit `/tmp` path
defeats it.

**If you truly need an isolated cold build** (a from-scratch compile without
`go clean -cache`), put the throwaway cache **on disk** and remove it
unconditionally with a `trap`, and redirect `TMPDIR` to the same dir so the
linker's own scratch also stays off tmpfs:

```bash
tmp=$(mktemp -d -p /var/tmp) && trap 'rm -rf "$tmp"' EXIT
GOCACHE="$tmp" TMPDIR="$tmp" go build ./cmd/gc/
```

**Exception:** `go clean -testcache` is explicitly allowed. It clears only the
test-result cache, not the compiled-object cache, and does not corrupt
concurrent builds.

## Internal work ledger (bd)

Maintainers track internal work in a local bd (beads) ledger; their agent
tooling injects `bd prime` context at session start. Contributors never need
it — public work is GitHub issues and pull requests.

- Run `bd prime` for the full command reference.
- When a bead needs to pause on a specific actor or condition, only
  `hold:mayor` and `hold:external` are canonical (set via
  `bd set-state <id> hold=mayor|external --reason "..."`) — never invent a new
  ad hoc hold/blocked label. See
  [hold-label-conventions.md](hold-label-conventions.md).
- gascity Dolt is LOCAL-ONLY (no remote). Do NOT run `bd dolt push`,
  `bd dolt pull`, or `bd dolt remote add` here -- they fail and re-introduce
  a doomed `origin` remote (ga-9wsri).
- That same no-remote shape is why bd >= 1.3.0 refuses to auto-apply pending
  schema migrations to gascity's shared Dolt sql-server: migrating would lock
  out every co-resident bd still on the old schema. If a bd WRITE fails with a
  refusal naming pending migrations, the sanctioned fix is `bd migrate schema`
  run once by a designated migrator after every bd client is upgraded -- NOT
  `bd dolt pull`, and not an ad-hoc `BD_ALLOW_REMOTE_MIGRATE=1`.
- A bead that tracks public work links its GitHub issue; the GitHub issue is
  the record outsiders see.
