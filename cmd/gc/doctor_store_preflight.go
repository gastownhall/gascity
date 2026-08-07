package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// doctorBeadStorePreflightTimeout bounds the single store probe that gates
// store-dependent doctor checks (gastownhall/gascity#5064).
const doctorBeadStorePreflightTimeout = 5 * time.Second

// City + per-rig store-dependent checks skipped on outage-shaped preflight
// failure. Keep these in sync with the register sites in buildDoctorChecks.
const (
	doctorCityStoreCheckCount   = 11
	doctorPerRigStoreCheckCount = 3
)

// doctorBeadStorePreflight probes CITY bead-store reachability once before
// store-dependent checks are registered. The probe uses city runtime env
// (bd list --limit 1); per-rig endpoints may differ. Tests override this hook.
// Also runs when buildDoctorChecks builds the gc start warmup check set.
var doctorBeadStorePreflight = defaultDoctorBeadStorePreflight

func defaultDoctorBeadStorePreflight(cityPath string, _ func(string) (beads.Store, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), doctorBeadStorePreflightTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	// NoRecovery + context-bound runner: O(1) list, no managed-dolt recovery,
	// and process-group kill on timeout (ga-cdmx6x / #5064).
	env, err := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, false)
	if err != nil {
		return err
	}
	_, err = beads.ExecCommandRunnerWithEnvContext(ctx, env)(cityPath, "bd", "list", "--json", "--limit", "1")
	return err
}

// isBeadStoreUnreachable reports whether err looks like a live store outage
// (circuit breaker, connection failure, pool exhaustion, timeout) rather
// than a missing/uninitialized store that individual checks should still report.
func isBeadStoreUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"dolt circuit breaker is open",
		"server appears down",
		"dolt server unreachable",
		"dolt server not reachable",
		"max waiting connections",
		"client rejected",
		"too many connections",
		"connection refused",
		"dial tcp",
		"bad connection",
		"invalid connection",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"timed out after",
		"context deadline exceeded",
		"unexpected eof",
		"use of closed network connection",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

func beadStorePreflightSkipCount(activeRigCount int) int {
	return doctorCityStoreCheckCount + doctorPerRigStoreCheckCount*activeRigCount
}

func beadStorePreflightSkipMessage(skipCount, rigCount int, probeErr error) string {
	// Probe is city-scoped (bd list under city runtime env). Per-rig checks may
	// resolve different endpoints (explicit rig Dolt, doltlite serverless); this
	// skip is therefore a city-outage gate, not a guarantee every rig endpoint is down.
	base := fmt.Sprintf(
		"bead store unreachable — skipped %d store checks (%d city, %d rigs); city store was probed (per-rig endpoints, including doltlite, may differ)",
		skipCount, doctorCityStoreCheckCount, rigCount,
	)
	if probeErr == nil {
		return base
	}
	return fmt.Sprintf("%s: %v", base, probeErr)
}
