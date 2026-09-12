# Plan: postgres-local-bootstrap.md + doctor FixHint + --explain flag (ga-7nwr)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-amb2` (closed) — bd-owned bootstrap;
> gc documents and surfaces.
> **Designer spec:** `ga-7nwr` (closed; 1480 lines / ~74KB) — full
> verbatim doc body, 5 shell refinements, FixHint amendment
> composition rules, explain-flag signature + output, idempotency
> error wording, linger prompt, password handling, the §8 test
> contract, and the cross-repo coordination filing template.
> **Sibling builder (postgres-server check):** `ga-yebqis` (open,
> P1) — provides the doctor `postgres-server` check whose FixHint
> this slice amends.
> **Decomposed into:** 1 builder bead + 1 coordination bead (see
> Children below).

## Context

This slice closes the loop on the architect's PG-local-bootstrap
strategy from `ga-amb2`: **bd owns the bootstrap; gc documents
and surfaces it.** Three deliverables ship together as a single
PR per the designer's §0:

1. `engdocs/postgres-local-bootstrap.md` — verbatim §1 of ga-7nwr.
   ~600 LOC of operator-facing prose + fenced shell blocks.
   Frontmatter, §1 Audience through §11 Uninstallation.
2. `postgres-server` doctor FixHint amendment — when Linux +
   systemd + loopback + unit-missing, the FixHint references the
   new engdoc. Verbatim composition rules in ga-7nwr §3.
3. `gc doctor --explain-postgres-bootstrap` flag — prints the
   embedded doc body. Verbatim signature + output rule in ga-7nwr
   §4. Designer ruled IN-scope per §0 #4.

Plus, separately:

4. Upstream coordination filing at `github.com/gastownhall/beads`
   for `bd init --backend=postgres --bootstrap-local` (FR-10 of
   ga-amb2; venue/title/body template pinned in ga-7nwr §9).
   Architect-action, not a builder task.

## Why one builder bead (not two)

The architect's confirmation mail (gm-sy6dz3) listed three items
as separate beads. The designer's §0, however, explicitly says
"single PR" — and the §8 test contract bears this out: doc tests,
FixHint amendment tests, and explain-flag tests all assert on
shared state (the on-disk doc, the FixHint composer, the embedded
doc string). Splitting into two builder beads would force one PR
to land first with a half-state — either:

- Engdoc lands first, then a follow-up PR adds the flag that prints
  the engdoc (introduces a brief window where `--explain` references
  a doc the flag does not yet print), or
- Flag lands first with no doc to print (pointless), or
- Both PRs need synchronized merge timing (operationally fragile).

The work is tightly coupled and small (~770 LOC total per designer
estimate). One builder bead matches the designer's intent. If the
architect prefers two-bead tracking, the bead can be split during
intake — but the verdict is single-PR coupling.

The coordination filing is genuinely separate work (different
target repo, different actor — architect, not builder), so it
gets its own bead.

## Children

| ID            | Title                                                                                                                | Routing label    | Routes to            | Priority | Depends on |
|---------------|----------------------------------------------------------------------------------------------------------------------|------------------|----------------------|----------|------------|
| `ga-cv6ome`   | feat(engdocs/doctor): postgres-local-bootstrap.md + FixHint amendment + --explain flag (ga-7nwr)                     | `ready-to-build` | `gascity/builder`    | P2       | `ga-yebqis` |
| `ga-ubtdnc`   | coord: file upstream bd-side gh issue for bd init --backend=postgres --bootstrap-local (ga-7nwr §9, FR-10)           | `upstream-coordination` | `gascity/architect`  | P3       | (soft: engdoc landed) |

## Acceptance for the parent (ga-7nwr)

Met when `ga-cv6ome` and `ga-ubtdnc` both close and the following
hold (mirroring the designer's acceptance hints):

- [ ] `engdocs/postgres-local-bootstrap.md` exists with §1 Audience
      through §11 Uninstallation in order.
- [ ] Bootstrap is idempotent on re-run; FATAL/WARNING wording byte-
      matches ga-7nwr §5.
- [ ] Linger prompt format is Enter-to-continue (NOT typed yes/no);
      verbatim body per ga-7nwr §6.
- [ ] Credentials heredoc is `<<EOF` (unquoted) so `$PG_PASSWORD`
      expands per ga-7nwr §7.
- [ ] Doctor `postgres-server` FixHint amendment composes per
      ga-7nwr §3 — `local PG not installed yet — see
      engdocs/postgres-local-bootstrap.md for one-time setup` on
      Linux + systemd + loopback + unit-missing.
- [ ] `gc doctor --explain-postgres-bootstrap` prints the embedded
      doc byte-equal to the on-disk source (`diff` is empty).
- [ ] No Go code in gc invokes `pg_ctl`, `initdb`, `systemctl`, or
      `loginctl` — the doc is operator-runbook only.
- [ ] All 26 tests from ga-7nwr §8 (CI lints + FixHint + flag) pass.
- [ ] Upstream gh issue filed at `gastownhall/beads` with verbatim
      title and body per ga-7nwr §9.2 / §9.3.

## Notes for the builder (ga-cv6ome)

- **Read ga-7nwr in full.** It is 1480 lines / ~74KB. The bead body
  in ga-cv6ome is a summary; the design body is the contract.
- **Tests use literal-string equality.** Every FATAL/WARNING string,
  every FixHint composition, every doc heading is pinned to a
  byte. Builder may not paraphrase.
- **Blocked-by ga-yebqis.** The FixHint composer being amended is
  introduced by ga-yebqis. Wait for ga-yebqis to land before
  starting this slice.
- **Single embed declaration.** `cmd/gc/cmd_doctor_explain_postgres.go`
  uses `//go:embed engdocs/postgres-local-bootstrap.md` so the
  flag's output and the on-disk doc cannot drift —
  `TestExplainPostgresBootstrap_EmbedMatchesSourceFile` enforces
  this at test time.
- **macOS / non-Linux scope.** The doc is Linux+systemd only. Tests
  short-circuit on non-Linux (`TestPostgresServerFixHint_NonLinuxUnitMissing_NoBootstrapAmendment`).
  macOS launchd analogue is a separate follow-on bead (out of scope
  per ga-7nwr §11).

## Notes for the architect (ga-ubtdnc)

- **Wait until the engdoc lands and is pushed.** The issue body
  hyperlinks to the engdoc on the gascity main branch; the link
  must resolve when the upstream maintainers click it. ga-7nwr
  §9.4 captures the filing checklist.
- **Issue body is fully pinned.** Architect copies ga-7nwr §9.3
  verbatim. No paraphrasing — the body cross-references ga-amb2,
  ga-7nwr, ga-46yyd, ga-dga2 by ID, and the upstream readers will
  follow those links to gascity for full rationale.
- **Filing checklist in ga-7nwr §9.4** — capture the issue number,
  append a note to ga-amb2, cross-link from ga-7nwr, subscribe
  the gascity bot account.

## Out of scope

These belong to follow-on beads of ga-amb2 and must not creep into
this slice:

- macOS launchd plist analogue — follow-on, separate bead.
- Docker-compose / podman-compose alternative bootstrap — follow-on.
- OpenRC / runit / s6 distro support — follow-on.
- Boot-time install on non-systemd Linux — follow-on.
- `bd init --backend=postgres --bootstrap-local` actual implementation
  in the bd repo — upstream (`gastownhall/beads`), not this repo.
- `postgres-server` doctor check itself — owned by ga-yebqis.

## Validation gates

- `go test ./internal/lints -run TestPostgresBootstrap -count=1` green
  (8 lints).
- `go test ./internal/doctor -run TestPostgresServerFixHint -count=1`
  green (8 FixHint cases + 4 helper-probe cases; ga-46yyd's existing
  cases preserved).
- `go test ./cmd/gc -run TestExplainPostgresBootstrap -count=1` green
  (6 explain-flag tests).
- `go test ./... -count=1` green; `go vet ./...` clean.
- `diff <(gc doctor --explain-postgres-bootstrap) engdocs/postgres-local-bootstrap.md`
  shows zero diff (manual verification).
- ZFC: no role names in the diff.
- Typed wire: N/A (no API surface changes).
- `git diff` confined to: `engdocs/postgres-local-bootstrap.md` (new),
  `internal/doctor/checks_postgres{,_test}.go` (edit),
  `internal/lints/postgres_bootstrap_test.go` (new),
  `cmd/gc/cmd_doctor_explain_postgres{,_test}.go` (new),
  doctor command wiring.

## Risks and unknowns

- **`//go:embed` path resolution.** The flag file is in `cmd/gc/`
  but embeds `engdocs/postgres-local-bootstrap.md` at the repo
  root. `embed` traverses relative paths from the source file;
  builder may need to use `//go:embed all:../../engdocs/postgres-local-bootstrap.md`
  or add a `go:generate` directive that copies the doc into the
  package. The designer's `TestExplainPostgresBootstrap_EmbedMatchesSourceFile`
  catches drift either way.
- **Coordination filing is async.** ga-ubtdnc has no hard
  blocked-by edge — the architect files the upstream issue at
  their discretion after the engdoc lands. The plan acknowledges
  this is operationally separate from the code PR.
- **`bash -n` on fenced blocks.** The CI lint
  `TestPostgresBootstrapDocShellBlocksParse` extracts fenced
  ` ```bash ` blocks and pipes through `bash -n`. The doc must
  use ` ```bash ` exactly (not ` ```sh ` or ` ``` `) so the
  extractor regex hits — verify by grepping the verbatim §1
  body.
- **Architect's "three beads" framing.** The architect's mail
  (gm-sy6dz3) listed engdoc and flag as separate items. This plan
  decomposes them as one because they share a test surface and
  must land together per the designer's §0. If the architect
  prefers split tracking, the bead can be split during intake —
  but the verdict here is single-PR coupling.
