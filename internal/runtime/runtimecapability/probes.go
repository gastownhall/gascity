package runtimecapability

import (
	"context"
	"fmt"
	"strings"
)

// probe verifies one declared capability against a running session. Probes
// run commands inside the session via the exec op and inspect the result, so
// a probe tests the runtime's wiring (did it materialize the workspace?
// install gc? inject the env?), not a self-report. The session is already
// started with the fixture work_dir + identity env.
type probe func(ctx context.Context, r *runner, name string) (Status, string)

// probes maps each catalog capability to its check. TestProbesCoverCatalog
// asserts this covers the catalog exactly.
var probes = map[Code]probe{
	CapWorkspace: probeWorkspace,
	CapTooling:   probeTooling,
	CapIdentity:  probeIdentity,
	CapLedger:    probeLedger,
}

// probeWorkspace verifies the start-config work_dir was materialized: the
// sentinel file (written by the runner into the fixture work_dir) is readable
// from inside the session. The probe uses a relative path so the host↔session
// path remap doesn't matter (exec runs in the session work dir).
func probeWorkspace(ctx context.Context, r *runner, name string) (Status, string) {
	out, code, unsupported := r.execIn(ctx, name, "cat "+sentinelName)
	if unsupported {
		return StatusFail, "declares env.workspace but has no exec op to verify it"
	}
	if code != 0 {
		return StatusFail, fmt.Sprintf("sentinel not readable in session (exit %d) — work_dir not materialized", code)
	}
	if strings.TrimSpace(out) != sentinelContent {
		return StatusFail, fmt.Sprintf("sentinel content = %q, want %q — work_dir not faithfully transferred", out, sentinelContent)
	}
	return StatusPass, "work_dir materialized (sentinel readable in session)"
}

// probeTooling verifies the agent toolchain is runnable in the session. gc is
// the load-bearing one (a runtime must install it); bd/git are stock.
func probeTooling(ctx context.Context, r *runner, name string) (Status, string) {
	for _, tool := range []string{"gc", "bd", "git"} {
		out, code, unsupported := r.execIn(ctx, name, tool+" version")
		if unsupported {
			return StatusFail, "declares env.tooling but has no exec op to verify it"
		}
		if code != 0 {
			return StatusFail, fmt.Sprintf("%q not runnable in session (exit %d) — toolchain not installed", tool, code)
		}
		_ = out
	}
	return StatusPass, "gc, bd, git runnable in session"
}

// probeIdentity verifies the session identity/env was injected: the
// GC_SESSION env var carries the session name and a run-as user is set.
func probeIdentity(ctx context.Context, r *runner, name string) (Status, string) {
	out, code, unsupported := r.execIn(ctx, name, "printenv "+probeSessionEnv)
	if unsupported {
		return StatusFail, "declares env.identity but has no exec op to verify it"
	}
	if code != 0 || strings.TrimSpace(out) != name {
		return StatusFail, fmt.Sprintf("%s in session = %q (exit %d), want %q — identity env not injected", probeSessionEnv, out, code, name)
	}
	who, code, _ := r.execIn(ctx, name, "whoami")
	if code != 0 || strings.TrimSpace(who) == "" {
		return StatusFail, "no run-as user in session (whoami empty)"
	}
	return StatusPass, fmt.Sprintf("%s=%s, user=%s injected", probeSessionEnv, name, strings.TrimSpace(who))
}

// probeLedger verifies the session's bd can reach the work ledger: `bd ready`
// inside the session must succeed, which requires the runtime to have made the
// gc beads API reachable from the session (the GC_BEADS_API endpoint the
// runner injects — pointed at a real endpoint locally, at a sandbox->host
// tunnel for a remote runtime). Transport-agnostic: the probe only asserts bd
// reaches the ledger, not how.
func probeLedger(ctx context.Context, r *runner, name string) (Status, string) {
	out, code, unsupported := r.execIn(ctx, name, "bd ready")
	if unsupported {
		return StatusFail, "declares env.ledger but has no exec op to verify it"
	}
	if code != 0 {
		return StatusFail, fmt.Sprintf("`bd ready` failed in session (exit %d) — bd cannot reach the ledger: %s", code, strings.TrimSpace(out))
	}
	return StatusPass, "bd reaches the work ledger from the session"
}
