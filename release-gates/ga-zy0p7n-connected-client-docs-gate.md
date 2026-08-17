# Release Gate: ga-zy0p7n connected-client docs

Result: PASS

Date: 2026-06-19

## Candidate

- Bead: `ga-zy0p7n`
- Title: `needs-deploy: connected-client API guide and reference docs`
- Clean deploy branch: `deploy/ga-zy0p7n-connected-client-docs`
- Base: `origin/main` at `ee9239f4311d9dbf4428db399e46377d680a9eaa`
- Source branch: `builder/ga-omnkls`
- Source commits evaluated:
  - `a6c0c013c` - `docs(extmsg): add connected-client API guide and reference docs (ga-lyikvt.3)`
  - `d8837b590` - `test: add connected-client docsync coverage`
- Final branch commits before this gate file:
  - `16087043d` - `docs(extmsg): add connected-client API guide and reference docs (ga-lyikvt.3)`
  - `f9aae5157` - `test: add connected-client docsync coverage`

## Changed Paths

- `docs/docs.json`
- `docs/guides/connected-clients.md`
- `docs/reference/api.md`
- `test/docsync/connected_clients_test.go`

## Release Criteria Source

`docs/PROJECT_MANIFEST.md` is not present in this repository. This gate applies
the deployer release criteria, the Gas City docs verification guidance, and the
repository testing guidance in `TESTING.md`.

## Operational Hold Note

These docs are spec-first. They document `POST /v0/extmsg/clients` and
`GET /v0/extmsg/{provider}/{account_id}/{conversation_id}/subscribe` before the
implementation for those endpoints is merged. The search surface for those
paths is limited to the new docs and docsync coverage. Treat the PR as ready for
review, but hold merge or docs publication until the connected-client transport
implementation ships unless the operator intentionally wants docs ahead of code.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-90vojc` is closed and includes `## VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | Adds the connected-client guide, links it from `docs/docs.json`, extends the API reference, and adds docsync coverage for the guide, navigation entry, anchors, and API-reference links. The known implementation gap is explicitly recorded above. |
| 3 | Tests pass | PASS | After rebasing onto `origin/main`: `make check-docs` PASS; `go test ./test/docsync -run 'TestConnectedClients'` PASS; `go vet ./...` PASS; `make test` PASS (`observable go test: PASS log=/tmp/gascity-test.jsonl.WiUgeg`). |
| 4 | No high-severity review findings open | PASS | Review notes include only a low-severity example error-handling follow-up; no HIGH findings are open. |
| 5 | Final branch is clean | PASS | `git config core.hooksPath` reports `.githooks`; `git status --short --branch` on the candidate branch was clean before adding this gate file. This gate file is committed as release evidence before push. |
| 6 | Branch diverges cleanly from main | PASS | `git fetch origin main` completed, then `git merge-tree --write-tree origin/main HEAD` returned tree `3a505e88b432cad533587b550b1f7bc8ef1c38e8` with exit 0. |
| 7 | Single feature theme | PASS | The final branch touches only connected-client external messaging docs and their docsync coverage; the unrelated commits present on `builder/ga-omnkls` were excluded. |

## Review Evidence

- `ga-90vojc`: PASS review for the connected-client docs.
- No external contributor interaction was observed on this deploy branch.
- `gh auth status` passed for the authenticated deploy account before PR work.

## Acceptance Notes

- The guide covers registration, subscription, inbound message posting, callback
  delivery, connection lifecycle, security notes, configuration, and a minimal
  Go example.
- The API reference covers the connected-client registration and subscribe
  endpoints along with the existing inbound endpoint.
- Docsync coverage locks the navigation and anchor surface so the public docs do
  not drift from the reference links.
