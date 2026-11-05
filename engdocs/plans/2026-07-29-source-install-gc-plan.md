---
title: Install Gas City from Current Source - Plan
type: chore
date: 2026-07-29
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-plan-bootstrap
execution: code
---

# Install Gas City from Current Source - Plan

## Goal Capsule

Build the `gc` binary from this checkout and install it through the repository's
supported source-install target. The resulting command must resolve to the
current checkout's build, without restoring or using the Homebrew formula.

Stop if the source build or install fails. Do not modify shell configuration,
city state, or tracked product code.

## Product Contract

### Summary

The user wants the globally available Gas City command to come from this local
source checkout instead of Homebrew.

### Problem Frame

Homebrew's `gc` was removed. The repository's `make install` target is the
canonical source-install path and installs to `go env GOPATH`/`bin`, with a
compatibility symlink in `~/.local/bin` when that directory exists.

### Requirements

- R1. Build `gc` from the current checkout with the repository's version and
  commit metadata.
- R2. Install the result using `make install`, not Homebrew and not a raw copy
  into the Homebrew Cellar.
- R3. Verify the installed command reports the source build and the checkout's
  commit identity.
- R4. Leave repository product files and shell configuration unchanged.

### Scope Boundaries

In scope: the source build, the supported user-level Go binary installation,
the `~/.local/bin/gc` link created by the Makefile, and verification.

Out of scope: Homebrew installation, shell-profile edits, city/service restart,
dependency upgrades, and code changes.

## Planning Contract

### Key Technical Decisions

- KTD1. Use `make install` as the installation mechanism, chosen over a raw
  `go install` or Homebrew reinstall because it applies the repository's
  build metadata, local Darwin signing, and supported `GOPATH/bin` plus
  `~/.local/bin` placement.

- KTD2. Verify both the installed path and binary metadata, chosen over
  checking only `gc version` because a version string alone cannot prove that
  the command resolves to this checkout.

### Sequencing and Constraints

1. Record the checkout commit and current command resolution.
2. Run `make install` from the repository root.
3. Verify `command -v gc`, `gc version`, and Go build metadata against HEAD.
4. Confirm the working tree contains no changes except any explicitly tracked
   LFG plan artifact.

## Implementation Units

### U1. Build and install the current source

Goal: Install the current checkout's `gc` binary through the supported source
path.

Requirements: R1, R2, R4.

Files: `Makefile`, `cmd/gc/` (read-only build inputs).

Approach: Run `make install` from the repository root. Do not invoke Homebrew
or write into `/opt/homebrew/Cellar`.

Test scenarios:

- A successful build creates and installs the binary under the Go toolchain's
  `GOPATH/bin`.
- When `~/.local/bin` exists, its `gc` link points to the installed source
  binary.
- A failed build stops before claiming installation success.

Verification: `make install` exits successfully and the installed binary is
executable.

### U2. Verify global resolution and provenance

Goal: Prove that the command found on PATH is the source-built binary.

Requirements: R3, R4.

Files: none.

Approach: Resolve `gc`, run its version command, inspect Go build metadata, and
compare the embedded commit with the checkout HEAD.

Test scenarios:

- `command -v gc` resolves through the user-level source-install location.
- `gc version` succeeds.
- The embedded commit identifies the current checkout rather than the removed
  Homebrew release.

Verification: Path, version output, metadata, and git status all agree.

## Verification Contract

- `make install`
- `command -v gc`
- `gc version`
- `go version -m "$(command -v gc)"`
- `git status --short --branch`

The install is accepted only when the build succeeds, `gc` resolves to the
source-install location, the command runs, and the embedded commit matches the
checkout's HEAD (allowing the Makefile's short-commit representation).

## Definition of Done

- U1 and U2 verification passes.
- Homebrew remains uninstalled for `gascity`.
- No shell configuration or city/service state was changed.
- No abandoned build artifacts or temporary files are left in the repository.
- The working tree is clean apart from the intentional LFG plan artifact, if
  the pipeline persists it.
