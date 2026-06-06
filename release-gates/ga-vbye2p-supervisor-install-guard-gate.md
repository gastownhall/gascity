# Release Gate: supervisor install binary-mismatch guard

- Deploy bead: `ga-vbye2p`
- Source review bead: `ga-f1tqo6`
- Original incident bead: `ga-72abmr`
- Follow-up test bead: `ga-yuq2g6`
- Release branch: `release/ga-vbye2p-supervisor-install-guard`
- Base: `origin/main` at `84b75173a`
- Cherry-picked commits:
  - `7314817bd0c7991f5ea26a17a88d94efd12cee26` - `fix(supervisor): refuse install when service unit references a different binary`
  - `2317e21301c843afc9bfb64dad79111c751e8651` - `test(supervisor): pass validator RED tests for install binary guard (ga-yuq2g6)`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this worktree; this gate evaluates the deployer prompt criteria plus the source bead acceptance criteria.

## Gate Result

PASS.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-f1tqo6` is closed with `Review verdict: PASS`; deploy bead `ga-vbye2p` records reviewer PASS and the required cherry-picks. |
| 2 | Acceptance criteria met | PASS | Source criteria from `ga-yuq2g6` are covered in `cmd/gc/cmd_supervisor_install_guard_test.go`: systemd refuse/matching/force/fresh cases, launchd refuse/matching/force/fresh cases, systemd ExecStart parser, launchd plist path parser, binary equivalence helper, and `gc supervisor install --force` flag binding. Implementation is in `cmd/gc/cmd_supervisor_lifecycle.go`. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'Test(InstallSupervisor(Systemd\|Launchd)\|SupervisorSystemdExecStartBinary\|SupervisorLaunchdPlistGCPath\|SupervisorSameBinary\|SupervisorInstallForce)'` passed. `make test-fast-parallel` passed all 8 fast jobs. `go vet ./...` passed. `gofmt -l` on changed Go files produced no output. `git diff --check origin/main...HEAD` passed. |
| 4 | No high-severity review findings open | PASS | `ga-f1tqo6` lists only INFO findings. `ga-vbye2p` handoff states no HIGH findings. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` showed `release/ga-vbye2p-supervisor-install-guard...origin/main [ahead 2]` with no dirty entries. The gate file is committed as the branch tip before push. |
| 6 | Branch diverges cleanly from main | PASS | Branch was cut from fresh `origin/main`; `git rev-list --left-right --count origin/main...HEAD` returned `0 2` before the gate commit; `git merge-tree --write-tree origin/main HEAD` completed successfully. |
| 7 | Single feature theme | PASS | The final diff is limited to supervisor install binary mismatch protection and its docs/tests: `cmd/gc/cmd_supervisor_lifecycle.go`, `cmd/gc/cmd_supervisor_install_guard_test.go`, `cmd/gc/cmd_supervisor_test.go`, and `docs/reference/cli.md`. |

## Acceptance Evidence

| Acceptance criterion | Evidence |
|---------------------|----------|
| systemd refuses an existing unit with a different gc binary unless `--force` is set | Guard in `installSupervisorSystemd`; tests cover refusal and assert no systemctl calls before refusal. |
| systemd allows matching binary, `--force` override, and fresh install | Table tests in `TestInstallSupervisorSystemdBinaryMismatchGuard`. |
| launchd refuses an existing plist with a different gc binary unless `--force` is set | Guard in `installSupervisorLaunchd`; tests cover refusal and stderr guidance. |
| launchd allows matching binary, `--force` override, and fresh install | Table tests in `TestInstallSupervisorLaunchdBinaryMismatchGuard`. |
| helper parsing handles expected service/plist forms | Tests cover quoted/unquoted systemd ExecStart paths, missing ExecStart, launchd ProgramArguments extraction, XML entity decode, and missing ProgramArguments. |
| binary comparison avoids false mismatches for equivalent paths | Tests cover clean-path equality and same-inode hardlink comparison, plus different-file negatives. |
| CLI exposes force override | `newSupervisorInstallCmd` test confirms `--force` sets `supervisorInstallForce`; docs list the flag under `gc supervisor install`. |

## Commands Run

```text
git worktree add -B release/ga-vbye2p-supervisor-install-guard ... origin/main
git cherry-pick 7314817bd 2317e2130
git diff --check origin/main...HEAD
go test ./cmd/gc -run 'Test(InstallSupervisor(Systemd|Launchd)|SupervisorSystemdExecStartBinary|SupervisorLaunchdPlistGCPath|SupervisorSameBinary|SupervisorInstallForce)'
make test-fast-parallel
go vet ./...
gofmt -l cmd/gc/cmd_supervisor_lifecycle.go cmd/gc/cmd_supervisor_test.go cmd/gc/cmd_supervisor_install_guard_test.go
git merge-tree --write-tree origin/main HEAD
git config core.hooksPath
```

`git config core.hooksPath` returned `.githooks`.
