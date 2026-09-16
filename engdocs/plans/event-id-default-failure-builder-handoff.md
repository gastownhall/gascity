# Event ID Default Failure Builder Handoff

Date: 2026-06-14

PM intake source:
- Hook items: ga-xsrxe8 and ga-3yad9d
- Architect parent: ga-9iw9b3
- Designer handoff source: gascity/designer

Tracker import: no-op. This worktree has the `actual` skill materialized, but no
`tracker-to-beads` skill or sibling tracker skill was present.

The expected `docs/architecture/`, `docs/rules/`, and `docs/designs/`
directories had no matching files at intake time. This decomposition uses the
architect and designer context embedded in the beads.

## Goal

Move the event ID default failure recovery work from designer-reviewed PM intake
to concrete builder work. The user-facing outcome is that routing and messaging
commands record events without `Field id does not have a default value` failures
after rebuilding `gc` against the local beads fix.

## Work Packages

### Regular update event regression coverage

Root: ga-3yad9d

- ga-3yad9d.1 -> builder: As an operator, non-ephemeral bead metadata updates record events without HY000 failures.

Acceptance focus:
- Add `TestNativeDoltStoreRegularUpdateEventRecording` in `internal/beads/native_dolt_store_integration_test.go`.
- Match the existing `OpenBestAvailable` skip-if-unavailable pattern from `TestNativeDoltStoreEphemeralCreate`.
- Create a non-ephemeral bead, call `store.SetMetadata(id, "gc.routed_to", "gascity/builder")`, assert no error, reload, and verify metadata.
- Record the focused integration test command and result, or the exact skip condition.
- Avoid `internal/events`, OpenAPI, docs schema, generated dashboard API types, and typed-wire changes.

### Live rebuild and command smoke verification

Root: ga-xsrxe8

- ga-xsrxe8.1 -> builder: As an operator, rebuilt gc records routing and messaging events on live Dolt.

Acceptance focus:
- Build `gc` with a throwaway `GOCACHE`, not `go clean -cache`.
- Confirm the build uses local beads commit `dc0561af2` or an equivalent/superseding explicit event-id fix.
- Verify live schema state for events migration 0051 and the wisp ignored/0010 equivalent without rolling migrations back.
- Smoke `gc sling`, `gc mail send`, and `gc session nudge`; each must avoid the HY000 missing-id failure.
- If a smoke check fails, record whether state mutated; transaction safety requires no partial metadata or wisp creation.
- Include the focused test result from ga-3yad9d.1, or record why it could not run.

Dependency:
- ga-xsrxe8.1 depends on ga-3yad9d.1.

## Risks

- The designer handoff proposes improved operator-facing error text, while the architect parent states the recovery fix should require no Gas City source changes. The builder beads therefore focus on the approved rebuild/test/smoke path. If HY000 remains after rebuild, return evidence to PM or architect before expanding scope.
- The smoke checks mutate live routing/mail/nudge state unless the builder chooses safe throwaway beads and targets. The acceptance criteria require safe smoke setup and mutation verification.

## Handoff Targets

Builder:
- ga-3yad9d.1
- ga-xsrxe8.1

Both child beads have `source:actual-pm`, `ready-to-build`, and
`gc.routed_to=gascity/builder`.

Dispatch:
- The installed `gc` binary failed the first sling attempt for ga-3yad9d.1
  with the known HY000 missing event-id error.
- PM used `GOCACHE=$(mktemp -d) go run ./cmd/gc --city
  /home/jaword/projects/gc-management --rig gascity ...` from
  `/home/jaword/projects/gascity`, which has the local beads replacement, to
  sling both beads without replacing the installed binary.
- Auto-convoys created: ga-q4genu for ga-3yad9d.1 and ga-9w6e7q for
  ga-xsrxe8.1.
- Builder context mail: gm-wisp-6xlv7ol.
