# Release Gate: ga-x5v5.1 - gc start troubleshooting walkthrough docs

**Deploy bead:** ga-lr3sol
**Originating bead:** ga-x5v5.1 (closed)
**Parent design:** ga-x5v5
**Umbrella PRD:** ga-r8hs
**Branch:** `builder/ga-x5v5-1` (fork: `quad341/gascity`)
**Commit:** `2d3601312583`
**Verdict:** PASS

Note: `docs/PROJECT_MANIFEST.md` is not present on this branch, so this gate uses the deployer role's release criteria plus the repository guidance in `TESTING.md`.

## Criteria

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-lr3sol` notes contain `Review verdict: PASS` from `gascity/reviewer` on 2026-05-16. |
| 2 | Acceptance criteria met | PASS | `docs/troubleshooting/gc-start-walkthrough.mdx`, `docs/images/troubleshooting/gc-start-fatal.png`, `docs/docs.json`, and `docs/getting-started/troubleshooting.md` are the only changed docs files. The seven required anchors are present: `#bd-op-init-timeout`, `#pack-schema-mismatch`, `#duplicate-name`, `#unknown-field-agent-pool`, `#rig-path-required`, `#template-not-found`, `#duplicate-identity`. `docs/docs.json` lists `troubleshooting/gc-start-walkthrough` first in the Troubleshooting group, followed by `troubleshooting/dolt-bloat-recovery`. The install troubleshooting page links near the top to `/troubleshooting/gc-start-walkthrough`. The PNG is 353,836 bytes at 1860x810 RGBA, under the 500 KB requirement. Word count excluding fenced code blocks is 1,268, within the 800-1,500 target. Existing `bd remember` memory `docs-troubleshooting-runbook-pages` records the per-runbook docs convention. |
| 3 | Tests pass | PASS | `make check-docs` PASS (`github.com/gastownhall/gascity/test/docsync` 2.899s). `go vet ./...` PASS. `make test-fast-parallel` PASS (`All fast jobs passed`). Mintlify preview smoke PASS: `./mint.sh dev --port 3017`; `/troubleshooting/gc-start-walkthrough` returned HTTP 200 with 352,908 bytes; `/images/troubleshooting/gc-start-fatal.png` returned HTTP 200 with 353,836 bytes. |
| 4 | No high-severity review findings open | PASS | Review notes contain one MEDIUM documentation-quality finding about background GitHub links and one INFO wording note; zero HIGH or CRITICAL findings are open. The MEDIUM item is design-sourced and explicitly non-blocking in the review. |
| 5 | Final branch is clean | PASS | After committing this gate file, `git status --short --branch` reports a clean tree on `builder/ga-x5v5-1...fork/builder/ga-x5v5-1 [ahead 1]`. |
| 6 | Branch diverges cleanly from main | PASS | After committing this gate file, `git merge-tree --write-tree origin/main HEAD` exited 0 with no conflicts. Merge base with `origin/main` is `d24018969552`; the branch is two commits ahead. |

## Reviewer Notes

The reviewer identified background links in `docs/troubleshooting/gc-start-walkthrough.mdx` that point at bead-style issue IDs under GitHub `issues/` URLs. The review classified this as MEDIUM, non-blocking, and design-sourced because those links are informational background context rather than operator navigation. Follow-up bead `ga-x5v5.3` tracks resolving or removing those background links.
