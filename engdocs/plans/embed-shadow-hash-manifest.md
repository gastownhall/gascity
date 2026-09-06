# Plan: embed-shadow hash manifest

> PM owner: `gascity/pm`
> Source beads: `ga-697vuo`, `ga-l83w5y`
> Origin: architect decision plus designer handoff for gascity#3143

## Goal

Non-required builtin packs should refresh files that are still the prior
embedded version after a `gc` binary upgrade, while preserving genuine operator
edits. The accepted strategy is a per-pack `.gc-pack-hashes.json` sidecar that
records the embedded content hashes last written by the binary.

## Accepted Decisions

- `materializeFS` gets an `io.Writer` warning parameter; production callers pass
  `os.Stderr`, tests may pass `nil` or a `bytes.Buffer`.
- Manifest-key pruning stays inline after the `materializeFS` walk because the
  `desired` set is already local to that function.
- The merged manifest is written even when the merged map is empty, so stale
  entries can be removed cleanly.
- Manifest write failure is non-fatal. Pack files still materialize, and the
  warning path records that the manifest could not be written.
- The manifest file and its atomic temp siblings are runtime metadata, not
  embedded content, and must survive generated-pack pruning.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-l83w5y.1` | As a maintainer, embed-shadow manifest behavior is locked by regression tests | `needs-tests` to `gascity/validator` | none |
| `ga-l83w5y.2` | As an operator, non-required builtin packs refresh stale embedded files without overwriting local edits | `ready-to-build` to `gascity/builder` | `ga-l83w5y.1` |
| `ga-l83w5y.3` | As an operator, pack hash manifests survive stale-file pruning and keep only embedded-file entries | `ready-to-build` to `gascity/builder` | `ga-l83w5y.2` |

## Acceptance Summary

`ga-l83w5y.1` is complete when the regression and manifest tests are present in
`cmd/gc/embed_builtin_packs_test.go`, cover the full designer matrix, and fail
against the pre-fix behavior.

`ga-l83w5y.2` is complete when `cmd/gc/embed_builtin_packs.go` implements the
hash manifest helpers and `materializeFS` reconciliation flow from `ga-697vuo`
and `ga-l83w5y`, including warning-writer support, non-fatal manifest writes,
operator-edit preservation, stale-embed refresh, and required-pack
non-regression.

`ga-l83w5y.3` is complete when stale generated-pack pruning preserves
`.gc-pack-hashes.json` and `.gc-pack-hashes.json.tmp.*`, manifest keys are
pruned to embedded desired files after the walk, and the embed builtin pack test
suite is green.

## Dependency Graph

`ga-l83w5y.1` blocks `ga-l83w5y.2`.

`ga-l83w5y.2` blocks `ga-l83w5y.3`.

## Out Of Scope

- In-binary historical hash manifests or CI code generation.
- New external dependencies.
- Changing the `materializeFS` return type.
- Treating `.gc-pack-hashes.json` as embedded pack content.
- New SDK primitives or role-specific Go behavior.
- Commenting on upstream GitHub issue #3143.

## Risk

The main risk is clobbering operator-edited non-required pack files during the
first post-fix run for cities that lack a manifest. The accepted migration
behavior is conservative preserve with no manifest entry. Future upgrades
auto-refresh once the file has a binary-written manifest entry.
