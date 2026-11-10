# Prospector behavior findings: PM disposition

This plan records the four decisions requested by `ga-81d8qwfj` on
2026-11-10. The priority is trustworthy operational diagnostics, followed by
command discoverability. Existing work covers three findings, so this intake
reuses those scopes rather than creating another implementation tree.

Planning base: `34485296e6bbef72f2940afea13ffcb66c5cfc80` (`origin/main`
after the required worktree freshen). Runtime observations below came from the
deployed `gc`; inspecting this checkout does not prove a fix is deployed.

## Decisions

| Finding | Decision | Work and priority | Reason |
| --- | --- | --- | --- |
| `gm-cpejn`: healthy sessions reported as orphans | Build through the existing scope | `ga-cm3cfp`, raised from P3 to P2, `ready-to-build`, `gascity/builder` | Five currently running, tracked sessions are reported as orphaned; the check also offers a repair action. |
| `gm-1o8d19`: imported command help omits script flags | Build through the existing scope | `ga-f02lce.1`, P3, `ready-to-build`, `gascity/builder` | The command works, but its help still hides the flags needed to use it. The existing architecture already defines a bounded fix. |
| `gm-r6z5lq` plus `gm-6ljw1d`: lint cannot resolve composed fragments | Continue the existing deployment | `ga-xb7fkdr`, P2, `needs-deploy`, held by `gascity/deployer` | These two reports describe one defect. Increment 1 has already passed review and is in its deployment gate. |
| `gm-ja4a8k`: completed convoys remain open | Leave the rig-wide claim pending a fresh failing example; retire the four historical HQ examples | No new build scope | The four named HQ convoys are closed. The historical rig count is not a current measurement, and the bounded rig sample does not reproduce it. |

No new implementation or test bead is created. The three existing work
records carry the PM evidence and acceptance clarification. Nonblocking
`related` edges connect this intake to them; the independent help and doctor
fixes do not block each other. The intake closes when this decision artifact
is committed and the handoffs are verified, rather than waiting for those
fixes to ship.

## Evidence and acceptance

### Orphan diagnostics

`gc doctor --check orphan-sessions -v` returned five orphan warnings with
exit code 0. An immediately captured `gc session list --json` contained all
five names as running, active sessions, among twelve active sessions total:

- `builder-gcg-session-e982424b3a7f98e62753ef1d12130206`
- `core__control-dispatcher-gcg-session-ab1e72552ecbe66056040eb7f8c0944b`
- `pack-author__pack-author-gcg-session-a388c133ca798d26dfbfcd0ac68b0f9b`
- `reviewer-gm-wisp-uonpjao`
- `witness-gcg-session-a220cb618af4171086bb20dfd1dfbf44`

Current `internal/doctor/checks.go` still derives expected names from agent
configuration in both `Run` and `Fix`. This agrees with the existing bug's
scope. Its adoption caveat remains essential: a runtime can have an old
environment session ID while a newer live session record legitimately owns it.

Acceptance on the existing bead:

1. Active tracked named, pooled, session-ID-named, and adopted runtimes are
   recognized as tracked.
2. A genuinely untracked runtime is still reported even if its name resembles
   a configured named session.
3. Partial or unavailable identity evidence cannot authorize destructive
   repair. The controller-running repair refusal remains effective.
4. Fake-provider/session fixtures prove those outcomes and retain the existing
   list-error and partial-list regressions. Run
   `bazel test //internal/doctor:doctor_test //cmd/gc:gc_test` and `make check`,
   with the matching integration tier when applicable.

The live checks were read-only. PM did not run `gc doctor --fix`.

### Imported command help

`gc assay file --help` still shows the generic Cobra flags and none of
`--title`, `--class`, `--reading`, `--probe`, or `--disposition`.
`ga-f02lce` already settles the design; `ga-f02lce.1` is its build scope.

Retain that approved scope: opt-in `native_help` support for both command
schemas, safe static defaults, help-only arguments, bounded fallback, and
unchanged help detection and namespace listings. Acceptance includes success,
failure, timeout, and unset cases, plus the unchanged
`TestAddDiscoveredCommandsToRoot_HelpFlagShowsBuiltInHelp` and
`TestTryDiscoveredCommandFallback_HelpFlagShowsHelpWithoutRunning` controls.
Use `bazel test //internal/config:config_test //cmd/gc:gc_test` and `make check`.

The mechanism needs a separate consumer rollout after landing. Enabling help
delegation on commands in `gc-management` requires verifying each script's
help handler first; this intake does not dispatch that other repository's work.

### Lint fragment scope

The correct recursive reproduction is:

```sh
cd /home/jaword/projects/gc-management
gc lint . --json
```

The fresh result covers 49 packs and contains ten undefined
`startup-work-query` diagnostics. A broader fragment/template-missing filter
finds 93 diagnostics, including a maintenance `architecture` template finding;
that total is not a count of confirmed false positives. Historical 11/12 and
74/80 counts must not be reported as current measurements.

Passing the absolute city directory as the lint target checks one pack,
which is a different invocation and cannot demonstrate that recursive lint
is fixed. The current checkout contains the earlier #6256 change; the
reviewed extension is tracked by `ga-bwhpjq.1` → `ga-dvz83ow` → `ga-xb7fkdr`.

Keep the existing Increment 1 gate and acceptance unchanged. Its production
comparison should account for the fragment findings while allowing unrelated
city lint diagnostics to remain. Review completion, gate completion, merge,
and installing a binary are separate evidence points.

`ga-63fyich` and `ga-cvja8cf` remain the existing later increments. Their
instance namespace and collision work is not dispatched by this triage.
Before later dispatch, require Increment 1's actual landing as its own
precondition; a dependency on the already-closed review does not prove that.

### Convoy state

The original finding's own later metadata records an HQ probe reading of
zero. Fresh direct reads confirm `gm-j1ggtr`, `gm-ms89e2`, `gm-dvrvb7`, and
`gm-ua5ah1` are closed, with their tracked members also closed.

The rig store currently lists 694 open convoys. All eight oldest sampled
convoys have unfinished tracked members: `ga-baudlk`, `ga-l4uw02`,
`ga-nuv1ww`, `ga-6vvttn`, `ga-mczpoi`, `ga-xr3z81`, `ga-v34cs9`, and
`ga-4m18zx`. Their members are open, blocked, or deferred, so their open
state does not demonstrate the reported all-members-closed failure. This
sample does not establish whether another eligible convoy remains stranded.
The backstop order has recent executions at 09:26, 10:26, and 11:26 Pacific;
an execution record alone does not establish its exit status or coverage.

Reopen implementation intake only with a current, authoritative convoy read
showing an unowned open convoy whose complete member set is closed, plus
evidence that a subsequent backstop execution missed it. Resolve residency
on the split-store city before treating a retained legacy-store row as live
state. Record the convoy/member IDs, timestamps, command results and sweep
outcome, then compare existing convoy scopes before filing a fix. That is the
named evidence trigger for further work; this plan schedules no repeated
measurement and claims no rig-wide repair. PM did not run the mutating
`gc convoy check` command.

## Delivery rules

For the help and doctor implementations, complexity acceptance is zero growth
against the pinned target merge base. Apply the full shared
`complexity-zero-growth` rule when measuring: a green base requires a green
merge check; otherwise compare both check outputs and require no worse rows,
with no new or regressed threshold rows or deleted functions that still live
outside the scan. A feature does not edit `engdocs/complexity-baseline.json`.
The [repository gates](../../AGENTS.md) also apply. Filtered Bazel evidence must include
`--test_env=GO_TEST_WRAP_TESTV=1 --test_output=all` and actual named PASS entries.

Tracker import is a no-op: no tracker-to-beads importer or sibling tracker
skill is installed in this session's materialized skill directory. This is a
bead-first triage. The plan and bead notes are the durable decision record;
raw read-only observations for this session are under
`/var/tmp/pm-ga-81d8qwfj.dye0J1`.
