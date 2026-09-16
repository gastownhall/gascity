# Release Gate: extmsg connected-client subscribe handler

Bead: `ga-j9xfm0`
Source review bead: `ga-izxsss`
Branch: `deploy/ga-j9xfm0-extmsg-subscribe-clean`
Base: `origin/main@dbc7023044a0d22231952b9c4f678845dd0c2b83`
Evaluated implementation head: `94e298a570ad464f220db6b1ed0bcae8b518d2c0`
Gate date: 2026-06-22
Gate worktree: `/tmp/gascity-deploy-ga-j9xfm0-clean.Hhrg0j`

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses the
release criteria from the active deployer instructions and the project gates in
`AGENTS.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-izxsss` is closed with close reason `pass`; its notes begin `REVIEW VERDICT: PASS`. The reviewer specifically rechecked F-A and F-B and found both resolved. |
| 2 | Acceptance criteria met | PASS | The branch adds the connected-client SSE subscribe path, client-token registration/config support, outbound subscriber registry delivery, and `gc extmsg reply`. F-A is covered by a stream-time `session_forbidden` guard before membership creation; F-B is covered by a single fail-fast service lookup reused through subscribe authorization. API schema, generated clients, CLI docs, config docs, and dashboard generated client surfaces are updated. |
| 3 | Tests pass | PASS | `make test`, `go vet ./...`, `make check-docs`, `make dashboard-check`, focused extmsg/API/CLI smoke tests, and `git diff --check origin/main..HEAD` passed on the assembled branch. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-izxsss` mark both high-severity findings resolved and say no new OWASP issues were introduced. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | Before the gate commit, the only uncommitted files were the expected dashboard generated client refresh from `make dashboard-check`; those files are committed with this gate. A clean post-commit status check is part of the deploy record before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base origin/main HEAD` is `dbc7023044a0d22231952b9c4f678845dd0c2b83`, and `git merge-base --is-ancestor origin/main HEAD` exits 0. The branch is cut directly on current `origin/main`. |
| 7 | Single feature theme | PASS | `git diff --name-only origin/main..HEAD` is confined to one extmsg connected-client subscribe/reply feature across API, CLI, config, tests, docs, OpenAPI/genclient, and generated dashboard client surfaces. The generated maintenance exports restored by `make dashboard-check` preserve current `main` schema output and do not introduce a separate feature. |

## Implementation Commits

```text
11b9183e8 feat(extmsg): add connected-client SSE infrastructure (WP-1, WP-2, WP-4)
2e23417bd feat(extmsg): implement connected-client SSE subscribe handler (WP-3, ga-mjkqhb)
cb93c8b3b fix(extmsg): address reviewer findings for WP-3 subscribe handler (ga-vdnivc)
b3e12d179 test(extmsg): add TestSubscribeHandler_ForbiddenSessionEmitsSSEError (ga-q5qjff F4)
dec3965c8 fix(extmsg): move session_forbidden to precheck as HTTP 403 (ga-7pfd10)
94e298a57 fix(extmsg): address reviewer F-A (TOCTOU) and F-B (double svc call) (ga-e1ex2l)
```

## Validation

```text
make test
PASS: observable go test: PASS log=/tmp/gascity-test.jsonl.yZufvy

go vet ./...
PASS

make check-docs
ok github.com/gastownhall/gascity/test/docsync 1.822s

make dashboard-check
PASS: npm ci, npm run gen, npm run build, npm run typecheck, and go test ./cmd/gc/dashboard/... all passed.

go test ./internal/api ./internal/extmsg ./cmd/gc -run 'TestSubscribeHandler|TestSubscriberRegistry|TestClientRegister|TestResolveClientToken|TestRunExtmsgReply|TestExtmsg'
ok github.com/gastownhall/gascity/internal/api 0.683s
ok github.com/gastownhall/gascity/internal/extmsg 0.018s
ok github.com/gastownhall/gascity/cmd/gc 0.229s

git diff --check origin/main..HEAD
PASS
```

## Notes

`make dashboard-check` regenerated `cmd/gc/dashboard/web/src/generated/index.ts`
and `cmd/gc/dashboard/web/src/generated/sdk.gen.ts`. The refresh restores
maintenance endpoint exports already present on current `main`
(`triggerMaintenanceDoltGc` and `getV0CityByCityNameMaintenanceStatus`) while
keeping the extmsg subscribe client generation in sync.

## Decision

PASS. All release criteria are met. Open a pull request and route a
merge-request to mayor/mpr; the deployer must not merge.
