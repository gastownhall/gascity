# PR #3301 Native-Store Split

Owner: `gascity/pm`
Created: 2026-06-10
Root bead: `ga-frmdxd`
Source review bead: `ga-gllg5b`
Original PR: https://github.com/gastownhall/gascity/pull/3301

## Goal

Recover the release path for PR #3301 by splitting the bundled work into
single-theme ship lanes.

The deploy gate failed criterion 7 because `origin/main..HEAD` contained the
reviewed emergency relay, SupervisorHTTPCheck, and macOS pack-release symlink
fixes plus an independent native-store hook/autoclose rewrite from commit
`630cef370`.

## Child Beads

| Bead | Target | Purpose |
| --- | --- | --- |
| `ga-frmdxd.1` | `gascity/builder` | Restore PR #3301, or a replacement PR, to the reviewed emergency/doctor/pack-release scope only. |
| `ga-frmdxd.2` | `gascity/builder` | Extract the native-store hook/autoclose rewrite into its own PR and release lane. |

## Acceptance

`ga-frmdxd.1` must be based on current `origin/main`, exclude commit
`630cef370` and native-store/autoclose-only changes, and retain only the
emergency relay, SupervisorHTTPCheck, macOS pack-release symlink fix, and any
generated artifacts required by those scoped changes.

`ga-frmdxd.2` must be based on current `origin/main`, contain only the
native-store hook/autoclose scope, exclude emergency/doctor/pack-release
changes, and explain that it was split out because PR #3301 failed the
single-feature deploy gate.

Both branches must record fresh test evidence and then move through the normal
review and deploy path. No rig agent should merge PR #3301 or either split PR
directly.

## Dependency Graph

Both child beads are children of `ga-frmdxd`. There is no blocker edge between
them: the emergency/doctor/pack-release lane and native-store/autoclose lane
can proceed independently after this PM split.

## Risks

If builder discovers that the native-store/autoclose work is technically
required for the emergency/doctor/pack-release scope, builder should stop and
route a `needs-architecture` bead instead of broadening either ship lane.

If either lane lacks adequate focused test coverage, builder should route a
`needs-tests` bead before handing off for deploy.
