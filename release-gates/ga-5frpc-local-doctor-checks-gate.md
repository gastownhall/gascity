# Release gate - local doctor checks (ga-5frpc)

**Verdict:** PASS

- Source beads: `ga-5frpc.2`, `ga-5frpc.3`, `ga-5frpc.4`
- Deploy beads on hook: `ga-1498l` (registration), `ga-km115` (docs)
- Branch: `builder/ga-5frpc-3`
- Reviewed HEAD: `e5021bb91`
- Base: `origin/main` at `7e175b8f8`

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS verdict in bead notes | PASS | `ga-hsmtq` PASS for the config model; `ga-1498l` PASS for registration at commits `719f0209b`, `9f30b55d6`; `ga-km115` PASS for docs at `e5021bb91`. |
| 2 | Acceptance criteria met | PASS | Config model adds `LocalDoctorCheck`, `DoctorConfig.Checks`, schema/reference docs, and parser coverage. Registration adds city-root-relative resolution, containment rejection, `local:` check names, `ErrorCheck` reporting, and reuses `PackScriptCheck`. Docs add a `[[doctor.check]]` example, naming rules, path rules, exit-code protocol, and local-check behavior. |
| 3 | Tests pass on final branch | PASS | Focused Go tests pass; `make check-docs` passes; `TMPDIR=/home/jaword/tmp/gc-deploy-ga-5frpc make test-fast-parallel` passes; `TMPDIR=/home/jaword/tmp/gc-deploy-ga-5frpc go vet ./...` passes. Initial default-`/tmp` fast run failed only with host `no space left on device`; rerun used home-backed temp space. |
| 4 | No high-severity review findings open | PASS | Reviewer findings: config model had 2 INFO findings; registration had 1 LOW finding matching existing pack-doctor containment behavior; docs had none. 0 HIGH. |
| 5 | Final branch is clean | PASS | `git status --short --branch` clean before gate-file commit on `builder/ga-5frpc-3...fork/builder/ga-5frpc-3`. |
| 6 | Branch diverges cleanly from main | PASS | `origin/main` is an ancestor of HEAD; `git rev-list --left-right --count origin/main...HEAD` reported `0 3`; `git diff --check origin/main...HEAD` clean. |

## Review Beads

| Source bead | Review bead | Reviewed commit(s) | Verdict | Notes |
|---|---|---|---|---|
| `ga-5frpc.2` | `ga-hsmtq` | `523849098` and patch-equivalent branch commit `719f0209b` | PASS | Config model only; current branch includes the rebased config commit and was also covered by `ga-1498l` branch review. |
| `ga-5frpc.3` | `ga-1498l` | `719f0209b`, `9f30b55d6` | PASS | Registration and execution wiring for local doctor checks. |
| `ga-5frpc.4` | `ga-km115` | `e5021bb91` | PASS | Operator-facing docs for local doctor checks. |

## Validation

- `go test ./internal/config ./internal/doctor ./cmd/gc -run 'Test(ParseDoctorLocalChecks|RegisterLocalDoctorChecksRunsCityRelativeScript|ResolveLocalDoctorScriptRejectsUnsafePaths|RegisterLocalDoctorChecksInvalidScriptPathReportsErrorCheck|RegisterLocalDoctorChecksInvalidFixPathReportsErrorCheck)$' -count=1` - PASS
- `make check-docs` - PASS
- `TMPDIR=/home/jaword/tmp/gc-deploy-ga-5frpc make test-fast-parallel` - PASS
- `TMPDIR=/home/jaword/tmp/gc-deploy-ga-5frpc go vet ./...` - PASS
- `git diff --check origin/main...HEAD` - PASS

## Push Target

`git push --dry-run origin HEAD` succeeded, so the release branch can be pushed to `origin` per deployer policy.
