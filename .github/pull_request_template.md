Closes #

<!-- Every pull request links a documented issue (file it first or alongside;
no approval needed). The issue holds the motivation, impact, and risk; this
description covers what changed and the evidence that it works. See
CONTRIBUTING.md. -->

## What changed

-

## Evidence it works

<!-- End-to-end evidence: commands run and their output, a scenario you
exercised, screenshots. "Unit tests pass" alone is not enough. -->

## Checklist

- [ ] `make check`
- [ ] `make check-docs` if docs, navigation, or links changed
  > **Note:** `docs/` is authored for [docs.gascityhall.com](https://docs.gascityhall.com) (Mintlify), not for direct GitHub viewing. Use extensionless page links (e.g. `/tutorials/01-beads`, not `/tutorials/01-beads.md`). If something looks broken on GitHub but works on the live site, that's intentional.
- [ ] `make test-integration` if runtime, controller, or workflow behavior changed
- [ ] Added or updated tests for behavior changes
- [ ] Updated docs for user-facing changes
- [ ] Updated the owning `AGENTS.md` if an invariant or boundary changed
- [ ] Called out breaking changes or migration notes
