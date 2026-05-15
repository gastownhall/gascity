# Release gate - identity cache regen, wrapper update, and stamp event (ga-ue02fr / ga-qku0jy)

**Verdict:** PASS

- Deploy bead: `ga-ue02fr` (review of source bead `ga-qku0jy`)
- Source branch: `builder/ga-qku0jy`
- Source commit: `dc28ac8a8b2fe3047556b9a51bc0d036570cc9aa`
- Dependency commit in branch: `e9c226da3` (`ga-h0pyln`, open prerequisite PR #2211)
- Gate run date: 2026-05-15
- Project manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate uses the deployer prompt's release criteria table plus the bead acceptance criteria.

## Stack dependency

This branch is stacked on open prerequisite PR #2211, "Treat identity.toml as the canonical project identity". The source bead explicitly depends on `ga-h0pyln` because its cache regeneration, wrapper call path, and event emission sit on top of the L1-authoritative reconcile flow.

Reviewers should merge #2211 first or review this PR with that dependency in mind. While #2211 is open, a PR from this branch to `main` includes both the dependency commit and the source commit.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Reviewer PASS verdict in bead notes | PASS | `ga-ue02fr` notes contain `REVIEW VERDICT: PASS` from `gascity/reviewer` for commit `dc28ac8a` on `builder/ga-qku0jy`. |
| 2 | Acceptance criteria met | PASS | Source commit diff is confined to the accepted surfaces: identity contract cache regen, `gc-beads-bd.sh`, `project.identity.stamped` event payload/registration, `ensure-project-id --city`, generated OpenAPI/client/dashboard types, and tests. All pinned test names are present. |
| 3 | Tests pass on final branch | PASS | `make test-fast-parallel` passed all 8 shards; `go vet ./...` passed; `make dashboard-check` passed; `git diff --check origin/main...HEAD` passed. |
| 4 | No high-severity review findings open | PASS | Review note reports PASS and no security concerns; no HIGH findings were recorded in the deploy bead notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean on `builder/ga-qku0jy` before adding this gate file. |
| 6 | Branch diverges cleanly from main | PASS | After `origin/main` advanced to `245db7423`, `git merge-tree --write-tree HEAD origin/main` completed successfully with tree `166bb80b82bc04bfafe046ae9eebf971faad3b34`; no merge conflicts reported. |

## Acceptance evidence

- `EnsureCanonicalMetadata` signature unchanged: `func EnsureCanonicalMetadata(fs fsys.FS, path string, state MetadataState) (bool, error)`.
- `gc-beads-bd.sh` contains `identity_toml_present` and `ensure_project_identity`; forbidden symbols `metadata_has_project_id` and `backfill_project_id_if_missing` are absent.
- `ensure-project-id` declares required `--city`; event recorder open uses `events.NewFileRecorder(..., io.Discard)` and falls back to `events.Discard` on open error.
- `ProjectIdentityStamped` is registered in `internal/events/events.go` and has typed payload registration in `internal/api/event_payloads.go`.
- Source commit diff check against expected files found no unexpected paths.

Pinned test contracts present:

- `TestEnsureCanonicalMetadataRegeneratesProjectIDFromL1`
- `TestEnsureCanonicalMetadataPreservesProjectIDWhenL1Absent`
- `TestEnsureCanonicalMetadataSurfacesL1ParseError`
- `TestEnsureProjectIDEmitsStampedEvent`
- `generated_emits_three_events_one_per_layer`
- `migrated_from_metadata_emits_one_l1_event`
- `migrated_from_database_emits_one_l1_event`
- `cache_repair_l2_emits_one_l2_event`
- `cache_repair_l3_emits_one_l3_event`
- `TestEnsureProjectIDEmitsNothingOnNoOp`
- `TestEnsureProjectIDEmitsNothingOnRefusal`
- `TestNoBashCleanupProjectIDGuard`
- `TestEveryKnownEventTypeHasRegisteredPayload`

## Validation

- `make test-fast-parallel` - PASS
- `go vet ./...` - PASS
- `make dashboard-check` - PASS
- `git diff --check origin/main...HEAD` - PASS
- `git status --short --branch` - clean before gate-file commit

## Push target

`origin` is `gastownhall/gascity`; `fork` is `quad341/gascity`. Resolve the push target with the deployer dry-run rule immediately before pushing the final branch.
