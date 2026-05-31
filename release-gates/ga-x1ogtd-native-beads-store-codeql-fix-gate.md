# Release Gate: ga-x1ogtd - Native Beads Store CodeQL Fix

- Gate run: 2026-05-31T10:48:20Z
- Deploy bead: ga-x1ogtd
- Review bead: ga-1eszfc
- Prior follow-up bead: ga-5oc14s
- PR: https://github.com/gastownhall/gascity/pull/2640
- Branch: builder/ga-l2souo-6-e2e
- Evaluated head: 62571d4c44baeb7435b61430da8b7c23b49ea8ca
- Release criteria source: deployer release-gate criteria. `docs/PROJECT_MANIFEST.md` is not present in this checkout.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-1eszfc is closed with `Review Verdict: PASS` for head 62571d4c44baeb7435b61430da8b7c23b49ea8ca on branch builder/ga-l2souo-6-e2e. It explicitly re-reviewed the prior CodeQL shell-injection finding from ga-5oc14s. |
| 2 | Acceptance criteria met | PASS | The branch now passes pool-demand route targets as `sh -c` positional arguments, keeps the shared `bd ready --metadata-field "$key=$target"` predicate in one helper, preserves `gc.run_target` before `gc.routed_to` precedence, keeps the legacy workflow-control fallback, and no longer embeds dynamic `key=target` text in the shell body. Tests assert these properties. |
| 3 | Tests pass | PASS | Local gates passed: focused config/work-query tests, `GOTOOLCHAIN=auto make dashboard-check`, `GOTOOLCHAIN=auto go vet ./...`, `GOTOOLCHAIN=auto make test-fast-parallel`, `GOTOOLCHAIN=auto make check-native-dependency-surface`, `git diff --check origin/main...HEAD`, and dashboard preview smoke on 127.0.0.1:4197 returning 22,831 bytes. GitHub PR checks are green, including CodeQL, Trivy, CI preflight, and CI required. |
| 4 | No high-severity review findings open | PASS | ga-1eszfc resolves the prior critical shell-injection finding. Its only remaining note is LOW/non-blocking extra allocation capacity. `gh pr checks 2640` reports CodeQL and Trivy passing at this head. Unresolved HIGH review finding count: 0. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --porcelain=v1 -uno` was empty and `origin/builder/ga-l2souo-6-e2e` matched HEAD. Dashboard generation left no working-tree diff. This gate file is the only deployer-added change and will be committed before push. |
| 6 | Branch diverges cleanly from main | PASS | After fetching `origin/main`, `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree 54dc795b77890dac450fc28015bf15fe3a7959a6. GitHub reports PR #2640 `mergeable=MERGEABLE` and `mergeStateStatus=CLEAN`. |
| 7 | Single feature theme | PASS | The PR remains one deploy theme: native beads store selection and its required dependency, diagnostics, API/schema/dashboard, and security-gate support surfaces. The final delta is a targeted CodeQL shell-argument fix for that same feature branch. |

## Acceptance Evidence

- `internal/config/config.go` scopes route target data as shell positional parameters: `shellquote.Join([]string{"sh", "-c", script, "--", target})` and `probe_pool_demand "$1"`.
- `poolDemandFirstRowFunctionScript` assigns `target="$1"` and evaluates `bd ready --metadata-field "$key=$target"` inside the shell, so the route target is data rather than syntax.
- `poolDemandCountShell` mirrors the same positional-argument pattern for reconciler demand counting.
- `TestPoolDemandPredicateSharedWithWorkQuery` verifies the work query and demand query share predicates and routing-key order, include the target as an argument, and do not embed `gc.run_target=<target>` or `gc.routed_to=<target>` in the predicate.

## Test Evidence

| Command | Result |
|---------|--------|
| `GOTOOLCHAIN=auto go test ./internal/config ./cmd/gc -run 'TestEffectiveWorkQuery\|TestEffectivePoolDemandQuery\|TestPoolDemandPredicateSharedWithWorkQuery\|TestComputeWorkSet_RunsWorkQuery\|TestPrefixedWorkQueryForProbe_UsesNamedSessionRuntimeName' -count=1` | PASS |
| `GOTOOLCHAIN=auto make dashboard-check` | PASS: OpenAPI generation, Vite build, TypeScript typecheck, and `go test ./cmd/gc/dashboard/...` passed. |
| `GOTOOLCHAIN=auto go vet ./...` | PASS |
| `GOTOOLCHAIN=auto make test-fast-parallel` | PASS: all fast jobs passed. |
| `GOTOOLCHAIN=auto make check-native-dependency-surface` | PASS: modules=725, aws=25, azure=9, dolthub=14, googleapi=1, binary_bytes=239088912. |
| `npm run preview -- --host 127.0.0.1 --port 4197 --strictPort` plus `curl -fsS http://127.0.0.1:4197/` | PASS: preview served 22,831 bytes; preview process was stopped after the smoke check. |
| `git diff --check origin/main...HEAD` | PASS |
| `git merge-tree --write-tree origin/main HEAD` | PASS |
| `gh pr checks 2640 --watch=false` | PASS: CodeQL, Trivy, CI preflight, CI integration, CI required, and PR check rollups are green. |

## Final Gate Result

PASS. The current PR head is suitable for human review and merge decision after this gate artifact is committed and pushed.
