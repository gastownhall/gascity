---
plan_slug: beads-proxy-default-2026-09
phase: tasks
rig: gascity
rig_root: /data/projects/gascity-beads-proxy-20.4-final
artifact_root: /data/projects/gascity-beads-proxy-20.4-final/plans
status: approved
created_at: 2026-09-04T07:10:00Z
updated_at: 2026-09-04T07:10:00Z
---

# Beads proxied-local default and direct escape hatches

This plan executes the approved `ga-p9iuv.30` contract. It keeps Gas City
provider-agnostic, delegates Beads/Dolt lifecycle to bd, defaults fresh bd/Dolt
scopes to proxied-local, preserves existing direct/embedded/DoltLite scopes, and
keeps explicit direct-local, direct-external, and proxied-external choices.

Sol owns the architecture contract and promotion reviews. Terra owns bounded
implementation slices. Each implementation slice must produce a pushed branch
and PR labelled `status/needs-review-auto`; the next dependent slice starts
only after Sol review passes.

## Bead Creation Payload

```yaml
parent_id: ga-p9iuv.30
nodes:
  - key: architecture-contract
    title: Ratify provider-neutral proxied-default contract and RC acceptance matrix
    type: decision
    priority: 0
    assignee: Sol
  - key: init-selector-default
    title: Implement provider-neutral init intent and proxied-local default
    type: feature
    priority: 0
    assignee: Terra
    parent_id: ga-p9iuv.30
  - key: review-init-selector-default
    title: Sol review init/default implementation PR
    type: task
    priority: 0
    assignee: Sol
    parent_id: ga-p9iuv.30
  - key: lifecycle-delegation
    title: Delegate provider-owned lifecycle and preserve legacy ownership
    type: feature
    priority: 0
    assignee: Terra
    parent_id: ga-p9iuv.30
  - key: review-lifecycle-delegation
    title: Sol review lifecycle delegation implementation PR
    type: task
    priority: 0
    assignee: Sol
    parent_id: ga-p9iuv.30
  - key: scope-resolution
    title: Implement persisted-state precedence inheritance and backend preservation
    type: feature
    priority: 1
    assignee: Terra
    parent_id: ga-p9iuv.30
  - key: provider-failure-atomicity
    title: Enforce provider readiness failure and retry-safe partial-init contract
    type: task
    priority: 0
    assignee: Terra
    parent_id: ga-p9iuv.30
  - key: review-safety-contract
    title: Sol review precedence ownership and failure-safety implementation PR
    type: task
    priority: 0
    assignee: Sol
    parent_id: ga-p9iuv.30
  - key: frontdoor-goldens
    title: Add real bd/gc proxied-default direct-escape and refusal front-door goldens
    type: task
    priority: 0
    assignee: Terra
    parent_id: ga-p9iuv.30
  - key: review-frontdoor-goldens
    title: Sol review front-door and direct-versus-proxy parity PR
    type: task
    priority: 0
    assignee: Sol
    parent_id: ga-p9iuv.30
  - key: active-pack-inventory
    title: Audit active bundled packs against proxied-local operation coverage
    type: task
    priority: 1
    assignee: Sol
    parent_id: ga-p9iuv.30
  - key: release-final-ac
    title: Close final proxy-default release acceptance and publish gates
    type: task
    priority: 0
    assignee: Terra
    parent_id: ga-p9iuv.30
edges:
  - from_key: architecture-contract
    to_key: init-selector-default
    type: blocks
  - from_key: init-selector-default
    to_key: review-init-selector-default
    type: blocks
  - from_key: review-init-selector-default
    to_key: lifecycle-delegation
    type: blocks
  - from_key: review-init-selector-default
    to_key: scope-resolution
    type: blocks
  - from_key: lifecycle-delegation
    to_key: review-lifecycle-delegation
    type: blocks
  - from_key: review-lifecycle-delegation
    to_key: provider-failure-atomicity
    type: blocks
  - from_key: scope-resolution
    to_key: provider-failure-atomicity
    type: blocks
  - from_key: provider-failure-atomicity
    to_key: review-safety-contract
    type: blocks
  - from_key: review-safety-contract
    to_key: frontdoor-goldens
    type: blocks
  - from_key: frontdoor-goldens
    to_key: review-frontdoor-goldens
    type: blocks
  - from_key: review-frontdoor-goldens
    to_key: release-final-ac
    type: blocks
  - from_key: active-pack-inventory
    to_key: release-final-ac
    type: blocks
  - from_key: review-frontdoor-goldens
    to_id: ga-p9iuv.5
    type: blocks
  - from_key: release-final-ac
    to_id: ga-p9iuv.23
    type: blocks
metadata_defaults:
  release.scope: proxy-default-direct-escape
  release.default: proxied-local
  review.required: Sol
  pr.label: status/needs-review-auto
```
