# Verification gates

Run these before considering docs work complete. They are ordered cheapest-first.

## Durable repo gates (committed — always available)

```bash
# 1. Docs sync: nav<->file consistency + local markdown links resolve.
#    This is the gate the pre-commit hook runs for docs changes.
make check-docs            # == go test ./test/docsync

# 2. Diagrams: re-render any changed Excalidraw source to SVG (idempotent).
make diagrams-excalidraw

# 3. Generated reference docs: regenerate after editing the Go source they
#    come from (Cobra Long: strings, config struct doc comments).
go run ./cmd/genschema     # writes docs/reference/{cli.md,config.md,schema/*}

# 4. API / dashboard: required when you touch internal/api/, the OpenAPI spec,
#    docs/reference/schema/openapi.*, or the dashboard.
make dashboard-check

# 5. Live preview while editing.
make docs-dev              # or: ./mint.sh dev   -> http://localhost:3000
```

## Principles the gates don't fully cover

`make check-docs` validates nav and local markdown links, not everything. Also confirm:

- **Every TOML fence parses.** A `formula.toml` fence must parse under the formula
  parser; a `city.toml` fence under the config schema. Don't ship a fence that
  wouldn't load.
- **Every internal link and anchor resolves**, and **no page is orphaned** from
  `docs/docs.json` (an unreferenced page fails `check-docs` and reads as an orphan).
- **No body `# H1`** — frontmatter `title` is the H1. (Beware false positives:
  `#` comments inside code fences are not H1s; scan fence-aware.)

The deeper TOML-fence and link/anchor/orphan tooling used during the big docs
program lived in the user-scoped `improve-docs` skill workspace (`.improve-docs/`,
which is git-excluded) — it is **not** part of this repo, so don't reference its
paths in committed work. If that skill is available, it's a good complement; if
not, `make check-docs` plus a manual fence/link pass is the durable floor.

## macOS / CGO environment

Building anything that links Dolt/ICU (e.g. `cmd/genschema`, beads tests) needs ICU
on the CGO path, and the test socket path needs a short `TMPDIR`:

```bash
icu="$(brew --prefix icu4c)"
export CGO_CPPFLAGS="-I$icu/include" CGO_LDFLAGS="-L$icu/lib" TMPDIR=/tmp
```

Pure-doc fence checks can run with `CGO_ENABLED=0` to avoid the ICU link entirely.

## Moving or removing a page

Removing or merging a page breaks inbound links and external bookmarks. Do all of:

1. **Redirect** — add an entry to the `redirects` array in `docs/docs.json` from the
   old path to the new one.
2. **Rewrite inbound links** — every `docs/` link to the old URL, and every
   `engdocs/` link that used the relative `../../docs/...` form (the `docsync`
   `TestLocalMarkdownLinks` check catches the engdocs ones).
3. **Fix anchor text** — if a link's label named the old page ("Architecture
   Overview"), update the label too, and dedupe links that now collapse to the same
   target.
4. **Update IA references** — `docs/docs.json` nav and
   `engdocs/contributors/docs-organization.md`.
