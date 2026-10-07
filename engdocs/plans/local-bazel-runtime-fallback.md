# Local Bazel runtime fallback

PM source: **ga-knmcwp** (P1), filed by mayor from gate tracker **ga-sdon3o**.
Prepared against `origin/main` at `99fda7d4824992c1977739f5af709fed80263684`.

## User impact and evidence

Contributors whose host lacks the pinned ICU runtime cannot execute locally
linked Bazel binaries. The loader fails before the checks run. On this fleet's
Fedora 44 host, the tracker records 78 failing targets in a completed 211-target
run on October 7, with the same ICU loader signature in all 101 failing logs.
Staged documentation also reaches this condition through pre-commit's
`make check-docs`. Repeating the full suite spends shared-host capacity without
producing evidence about the proposed change.

The sysroot pins ICU 74 in `MODULE.bazel`; the host has ICU 77. Static ICU
archives are excluded by the sysroot. Hermetic child re-executions can discard
`LD_LIBRARY_PATH`, so setting that variable is insufficient as a general fix.
The tracker remains the record for this condition, rather than becoming another
work assignment.

## Required behavior

1. Before auto mode would launch local Bazel tests, determine whether the host
   loader can resolve the required pinned runtime. On a missing runtime, select
   the existing Go suite and print the existing warning plus a reason naming
   the missing soname and a supported runtime/remote-executor fix hint. No
   Bazel test run starts in this case.
2. A resolvable runtime preserves today's cache path. Preserve eligible remote
   execution and the existing worker-pin/refusal rules. Explicit `cache`, `rbe`
   and `go` selections are never silently replaced.
3. Required sonames have one authoritative source tied to the sysroot pin,
   with verification that detects drift when the pin changes.
4. Staged-docs pre-commit checks no longer fail on this loader condition. The
   architect chooses the authorized non-Bazel validation path or a clearly
   reported skip limited to ICU-dependent checks. Genuine docs failures remain
   visible; beads chaining and other staged-change checks remain effective.
5. Use deterministic seams and isolated hook fixtures for regression coverage,
   including missing/resolvable runtime, probe errors, explicit modes, remote
   behavior and supported platforms. No host package installation is needed.
6. State accurately which checks ran. A passing Go fallback does not establish
   parity with the Bazel checks enforced by CI.

## Work packages and dependencies

| Bead | Priority / route | Acceptance and prerequisite |
|---|---|---|
| ga-fmkdo5 | P1 / architect | Settle bounded loader probing, platform/error handling, soname authority, decision matrix and pre-commit validation contract. |
| ga-3nnp7h | P1 / validator, then builder | One RED + Build + GREEN scope for the shared runtime decision and both hooks. Blocked by ga-fmkdo5; stamped `gc.fixes_tracker=ga-sdon3o`. |
| ga-ylttci | P1 / builder | Docs-only contributor guidance and verified examples. Blocked by ga-fmkdo5 and ga-3nnp7h; confirm landing or joint shipment before publication. |

Each package carries `discovered-from:ga-knmcwp`. The validator transfers the
same open ga-3nnp7h bead to the builder after test authoring; there is no second
implementation bead. Owning invariant updates belong to that implementation.
Each bead holds its measurable acceptance criteria and handoff requirements.

## Risks, boundaries and completion

Probe design must avoid a false fallback on supported platforms, an unbounded
loader check, or a silent downgrade of explicitly requested validation. The
pre-commit decision must identify any omitted checks and their CI coverage.
Architectural choices stay on ga-fmkdo5; this plan defines the outcomes.

Host package installation, sysroot changes, static linking, remote configuration
changes and changes to CI enforcement are outside this work. The mayor owns any
separate operator decision to install a side-by-side runtime. The current
authorized push workaround is `GC_PREPUSH_SUITE=go` through the active hook.

PM completion means this artifact is committed, verified clean and pushed, with
the three packages routed and their context mail verified. It does not mean the
runtime defect is fixed. Keep ga-sdon3o open until the actual fix lands, or the
mayor confirms an operator-runtime landing. The upstream change follows the
normal review/deploy pipeline and MPR merge path. No external tracker skill is
installed, so tracker import is a no-op.
