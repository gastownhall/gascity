// Package rig owns rig-add provisioning. It holds the pure orchestration —
// validation, prefix derivation and collision checks, re-add detection, the
// comment-preserving city.toml append, routes.jsonl, the deferred packs.lock
// commit, and atomic topology rollback — extracted from cmd/gc so that both the
// CLI (gc rig add) and the controller/API drive a single provisioning path
// (DESIGN-BRIEF Decision 7).
//
// Filesystem access and the cmd/gc-resident steps that internal/rig cannot
// import (bead-store init, pack compose/install, agent hooks, formula
// resolution, controller reload) are supplied by the caller through Deps,
// mirroring the internal/sling.SlingDeps injection pattern. The provisioning
// core never imports package main and never writes to stdout/stderr: it returns
// a structured ProvisionResult and pushes incremental progress through
// Deps.OnStep, which the CLI renders as text and the API projects onto events.
package rig
