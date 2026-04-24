---
title: "Workspace Identity Site Binding — Implementation Plan"
---

Companion to GitHub issue `#600` and beads issue `ga-h6h43`.

This plan finishes the PackV2 identity cutover that earlier work only started:

- `#850` moved `rig.path` into `.gc/site.toml`
- `#923` made registration aliases machine-local
- `#853` preserved template `workspace.name` during `gc init`

The remaining work is to move workspace identity and prefix into site binding,
resolve effective identity early, and make runtime/API/doctor surfaces use the
effective values rather than raw `city.toml` fields.

## Decisions

- Runtime name precedence:
  1. registered alias when the city is running under the supervisor
  2. site-bound workspace name from `.gc/site.toml`
  3. directory basename fallback
- `pack.name` is not a runtime identity source; it is only an init-time default
- Prefix moves in the same slice as name
- Operational config/API surfaces expose effective identity by default
- Raw declared values remain available only where migration/debugging needs them

## Acceptance

- `city.toml` can omit `workspace.name` and `workspace.prefix` after migration
- `gc doctor` is clean for migrated PackV2 cities
- runtime naming, session naming, `GC_CITY_NAME`, and HQ prefix use effective identity
- `gc init*` writes machine-local identity into `.gc/site.toml`
- `gc doctor --fix` migrates legacy workspace name/prefix into `.gc/site.toml`
- legacy configs remain readable during the migration window

## Phase 1: Site Binding Foundation

### 1A. Extend `.gc/site.toml` schema
**Files:** `internal/config/site_binding.go`, `internal/config/site_binding_test.go`, `docs/packv2/doc-loader-v2.md`

**Scope:**
- add workspace name/prefix fields to `SiteBinding`
- add helpers to apply workspace identity from site binding
- keep rig binding behavior unchanged

**Verification:**
- unit tests for load/apply/persist of workspace name/prefix

### 1B. Resolve effective identity in config load
**Files:** `internal/config/config.go`, `internal/config/named_sessions.go`, `internal/config/compose.go`, relevant tests

**Scope:**
- formalize helpers for effective city name and effective HQ prefix
- set effective identity from site binding before named-session validation
- preserve basename fallback when no site-bound name exists

**Verification:**
- unit tests proving named-session validation uses site-bound name
- unit tests proving prefix resolution uses site-bound prefix

## Phase 2: Runtime and CLI Cutover

### 2A. Replace raw workspace-name consumers
**Files:** runtime/CLI callers currently reading `cfg.Workspace.Name` or `cfg.Workspace.Prefix`

**Primary surfaces:**
- `cmd/gc/providers.go`
- `cmd/gc/cmd_hook.go`
- `cmd/gc/cmd_status.go`
- `cmd/gc/cmd_events.go`
- `cmd/gc/cmd_mail.go`
- `cmd/gc/cmd_prime.go`
- `cmd/gc/order_dispatch.go`
- `internal/workdir/workdir.go`
- other session/runtime name helpers discovered during implementation

**Scope:**
- switch these paths to effective identity helpers
- keep raw config access only where editing/migration needs it

**Verification:**
- targeted unit tests for session names, `GC_CITY_NAME`, workdir expansion, and prefix consumers

### 2B. Align standalone controller/runtime behavior
**Files:** `cmd/gc/controller.go`, `cmd/gc/city_runtime.go`, `cmd/gc/cmd_supervisor.go`, tests

**Scope:**
- make reload/name-lock logic compare effective identity, not only `workspace.name`
- keep supervisor registered alias authoritative when present
- ensure standalone runtime uses site-bound/basename identity consistently

**Verification:**
- controller reload tests
- supervisor initialization tests

## Phase 3: Init, Migration, and Doctor

### 3A. Write site-bound identity on init
**Files:** `cmd/gc/cmd_init.go`, `cmd/gc/main_test.go`, init txtar fixtures

**Scope:**
- `gc init`, `gc init --file`, and `gc init --from` write workspace name/prefix to `.gc/site.toml`
- `pack.name` remains the init-time default only
- new writes stop persisting workspace name/prefix into `city.toml`

**Verification:**
- targeted init tests
- txtar coverage for fresh init, file-based init, and template init

### 3B. Migrate legacy workspace identity out of `city.toml`
**Files:** `cmd/gc/doctor_v2_checks.go`, `cmd/gc/doctor_v2_checks_test.go`, `internal/configedit/...`

**Scope:**
- add a `gc doctor --fix` migration for `workspace.name` and `workspace.prefix`
- keep legacy read fallback until migration completes
- define conflict handling when site binding and legacy fields disagree

**Verification:**
- migration tests for happy path, conflict path, and idempotent rerun

### 3C. Make doctor clean for migrated cities
**Files:** `internal/doctor/checks.go`, `cmd/gc/doctor_v2_checks.go`, tests

**Scope:**
- stop warning just because `workspace.name` is absent
- retire or rewrite the `v2-workspace-name` warning
- report healthy state when effective identity is site-bound or basename-derived

**Verification:**
- doctor tests proving migrated cities are warning-free

## Phase 4: API, Docs, and Schema

### 4A. Expose effective identity in API/config surfaces
**Files:** `internal/api/huma_handlers_config.go`, config/status handlers, related tests

**Scope:**
- `/v0/config` and related operational endpoints return effective name/prefix
- if needed, expose raw declared values only in explain/debug surfaces

**Verification:**
- API response tests for effective name/prefix

### 4B. Update docs and schema
**Files:** `docs/reference/config.md`, `docs/packv2/doc-pack-v2.md`, `docs/packv2/skew-analysis.md`, `docs/guides/migrating-to-pack-vnext.md`, schema/reference outputs

**Scope:**
- document `.gc/site.toml` as the home of machine-local workspace identity
- remove stale claims that `workspace.name` is the required runtime identity
- document `pack.name` as init-only default

**Verification:**
- doc/schema regeneration checks used by the repo

## Execution Order

1. Phase 1A
2. Phase 1B
3. Phase 2A
4. Phase 2B
5. Phase 3A
6. Phase 3B
7. Phase 3C
8. Phase 4A
9. Phase 4B

Each phase should keep the repo building and the targeted tests green before
moving on.
