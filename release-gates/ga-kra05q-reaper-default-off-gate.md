# Release Gate: ga-kra05q reaper default off

Branch: `release/ga-kra05q-reaper-default-off`
Base: `origin/main` at `a67afaa25`
Code commit before gate checklist: `000e15da6788196134e80991ba247c732e02411a`
Source commit: `238a56ae2`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer release criteria table plus the bead-specific operator
instructions in `ga-kra05q`.

## Scope

`AutoReapClosedBeadWorktrees` now defaults to false when unset. The change also
updates the config schema, generated config reference, and focused tests.

Changed in this branch:

- `internal/config/config.go`: nil `AutoReapClosedBeadWorktrees` returns false.
- `internal/config/config_test.go`: default assertion now expects false.
- `cmd/gc/city_runtime_bead_worktree_reap_test.go`: runtime reaper tick test
  requires explicit true to fire the closed-bead worktree reaper.
- `docs/reference/config.md`, `docs/schema/city-schema.json`,
  `docs/schema/city-schema.txt`: default and description say false/opt-in.

Already true before this branch:

- The bead provider default remains `bd`, and an empty bd backend defaults to
  `dolt` (`cmd/gc/providers.go`, `docs/reference/config.md`).
- `modernc.org/sqlite` was already present in `origin/main`'s dependency graph;
  this branch does not add or remove SQLite provider code.
- A default local Linux build is CGO-enabled (`go env CGO_ENABLED` -> `1`) and
  produced a dynamically linked binary with ICU/libc dependencies.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-kra05q` is a P0 mayor/operator-directed release handoff. No separate reviewer bead exists for `ga-kra05q`; the bead notes identify `238a56ae2` as the correct change and route it directly to deployer for a clean PR. |
| 2 | Acceptance criteria met | PASS | Reaper default is false in code/schema/docs/tests. Default bead backend remains `bd` -> `dolt`; no modernc cutover is introduced. Default build verification shows CGO enabled and a dynamically linked binary. |
| 3 | Tests pass | PASS | See test log below. |
| 4 | No high-severity review findings open | PASS | No review bead or HIGH findings found for `ga-kra05q`; branch contains only the six requested files. |
| 5 | Final branch is clean | PASS | Branch was clean after cherry-pick before adding this gate; final cleanliness verified after committing the gate. |
| 6 | Branch diverges cleanly from main | PASS | `git cherry-pick 238a56ae2` applied cleanly on current `origin/main`; `git merge-base --is-ancestor origin/main HEAD` passed; `git diff --check origin/main...HEAD` passed. |
| 7 | Single feature theme | PASS | One subsystem/theme: make closed-bead worktree reaping opt-in by default and document the default. |

## Test Log

- PASS: `go test ./internal/config -run TestDaemonAutoReapClosedBeadWorktrees -count=1`
- PASS: `go test ./cmd/gc -run 'TestCityRuntimeTick_(SkipsClosedBeadWorktreeReapWhenDisabled|AttemptsClosedBeadWorktreeReapWhenEnabled)' -count=1`
- PASS: `make test-fast-parallel`
- PASS: `go vet ./...`
- PASS: `go build -o /tmp/gascity-gate-ga-kra05q/gc ./cmd/gc`
- PASS: `file /tmp/gascity-gate-ga-kra05q/gc` reported a dynamically linked
  Linux x86-64 executable.
- PASS: `ldd /tmp/gascity-gate-ga-kra05q/gc` reported ICU/libstdc++/libc
  dynamic dependencies.

## Notes For Mayor

Changed: the worktree reaper default is now off.

Already off/default-safe: bead storage defaults still select `bd` with a `dolt`
backend unless explicitly configured otherwise. This branch does not deploy or
cut over modernc/SQLite; the existing `modernc.org/sqlite` dependency remains
unchanged from `origin/main`.
