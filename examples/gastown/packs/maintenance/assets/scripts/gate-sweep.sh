#!/usr/bin/env bash
# gate-sweep — evaluate and close pending gates.
#
# Runs as an exec order (no LLM, no agent, no wisp). bd dispatches per
# type. `|| true` is load-bearing: bd shells out to `gh` for gh:run /
# gh:pr gates, and fresh cities without `gh auth` would otherwise fail
# this order on every 30s cooldown. bd's combined output reaches the
# controller log only on non-zero exit (cmd/gc/order_dispatch.go:466-475),
# so the suppression also hides real bd errors — diagnose by hand.
#
# Bead-type gates are skipped: in beads v1.0.2, checkBeadGate is
# hard-coded to fail because cross-rig routing was removed upstream.
# Restore `bd gate check --type=bead --escalate` when beads adds it back.
set -euo pipefail

bd gate check --type=timer --escalate || true
bd gate check --type=gh --escalate || true
