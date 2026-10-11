// Package rollout is gascity's rollout-gate (feature-flag) subsystem: a typed
// registry of infrastructure rollout/migration gates plus a general
// capability-resolution model that selects between two mechanical code paths.
//
// A rollout gate is NOT an agent-capability flag. It gates internal transport
// paths (which store CAS verb to call, which migration branch to run) that are
// invisible to prompts and cannot express per-agent behavior — the design keeps
// the "no capability flags" exclusion intact.
//
// The package is deliberately narrow in its dependencies: it imports only the
// standard library and internal/config (and, reserved, internal/deps). It must
// NEVER import internal/beads or any consumer package — the capability model is
// general and beads CAS is merely its first consumer. The allowlist is enforced
// by TestRolloutImportBoundary.
//
// The package holds no process-level mutable state and reads no environment at
// init: a Flags value is computed once from merged config plus env overrides via
// Resolve, then threaded by value. Tests build isolated Flags with ForTest.
package rollout
