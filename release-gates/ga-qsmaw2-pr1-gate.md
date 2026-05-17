# Release gate - CityRoot pre_start doctor check (ga-gqhfxj / ga-qsmaw2)

**Verdict:** PASS

- Deploy bead: `ga-gqhfxj`
- Source bead: `ga-qsmaw2`
- Review bead: `ga-wsn3qp`
- Branch: `builder/ga-qsmaw2-pr1`
- Implementation commit: `029a764a9af240c9a03549bf3c0c439fd68e88fa`
- Base checked: `origin/main` at `3ef98456b154fbf32f83c09c2e547fda1df286f7`
- Push target: `origin` (`git push --dry-run origin HEAD` succeeded)
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this repo; this gate uses the deployer prompt criteria and the source bead acceptance criteria.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-wsn3qp` is closed and includes `VERDICT: pass` for commit `029a764a9`. |
| 2 | Acceptance criteria met | PASS | `resolvePreStartScript(cmd, sourceDir, cityPath string)` is present; `{{.CityRoot}}` and `{{.ConfigDir}}` are both substituted; commands with neither still skip; `Run(ctx)` passes `ctx.CityPath`; new CityRoot tests and the 7-case resolver table exist; `cmd/gc/cmd_doctor.go` is unchanged. The PR body will reference design bead `ga-x0pq6s` as required by the source acceptance criteria. |
| 3 | Tests pass | PASS | Deployer re-ran focused doctor tests, `make test-fast-parallel`, `go vet ./...`, and `make lint`; all passed. |
| 4 | No high-severity review findings open | PASS | Review notes report no security concerns and no blocking findings; no HIGH findings are present in the review bead. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before this markdown-only gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded and produced tree `056d1d59051dc32387854d2d47023790a502ec08`; PR-style diff is limited to `internal/doctor/pre_start_scripts_check.go` and `internal/doctor/pre_start_scripts_check_test.go`. |

## Acceptance Evidence

- Helper signature: `func resolvePreStartScript(cmd, sourceDir, cityPath string) (string, bool)`.
- CityRoot substitution is implemented with `strings.ReplaceAll(expanded, "{{.CityRoot}}", cityPath)`.
- ConfigDir substitution is preserved with `strings.ReplaceAll(expanded, "{{.ConfigDir}}", sourceDir)`.
- First-token validation still rejects unresolved templates and relative paths.
- `PreStartScriptsCheck.Run(ctx)` reads `ctx.CityPath` and passes it to the resolver.
- New tests present:
  - `TestPreStartScriptsCheck_CityRoot_ScriptExists`
  - `TestPreStartScriptsCheck_CityRoot_ScriptMissing`
  - `TestPreStartScriptsCheck_BothTemplates_OneMissing`
  - `TestPreStartScriptsCheck_CityRoot_OtherTemplateInPath`
  - `TestResolvePreStartScript_TableDriven`

## Deployer Validation

- `go test ./internal/doctor/... -run 'TestPreStartScriptsCheck|TestResolvePreStartScript_TableDriven'` - PASS
- `make test-fast-parallel` - PASS
- `go vet ./...` - PASS
- `make lint` - PASS (`0 issues.`)
- `git diff --check origin/main...HEAD` - PASS
- `gh pr list --repo gastownhall/gascity --head quad341:builder/ga-qsmaw2-pr1 --state all` - no existing PR found before deployer push
