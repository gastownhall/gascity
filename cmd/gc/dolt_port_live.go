package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Live managed-dolt port resolution (city-scale plan P1.7, incident class 4).
//
// .beads/dolt-server.port and .beads/proxied_server_client_info.json are
// status files. gc never trusts them for endpoint resolution: the port file
// is a compatibility mirror for raw bd (bd owns the writes), and it has lied
// in production — a surviving writer clobbered it with a proxy's ephemeral
// port, even after the pooling fix. The resolution order here is
// live state only:
//
//  1. managed-server live handle — published/provider Dolt runtime state,
//     validated against the live process (PID alive, port reachable,
//     data-dir match) by currentResolvableManagedDoltPort.
//  2. process-table discovery — a live `dolt sql-server` listener whose
//     --config or --data-dir argv matches the city's managed runtime layout.
//  3. error — never the status files.

// Live source labels recorded in PortResolutionAttempt.Source for the two
// live resolution steps.
const (
	liveDoltHandleSource  = "managed dolt runtime state"
	liveDoltProcessSource = "live dolt process table"
)

// errNoLiveDoltEndpoint reports that no live managed dolt endpoint could be
// resolved from runtime state or the process table. It is deliberately NOT
// recoverable via .beads/dolt-server.port: that file is a status file and is
// never consulted (city-scale plan P1.7).
var errNoLiveDoltEndpoint = errors.New("no live managed dolt endpoint")

// liveDoltPortResolution is the outcome of the live resolution chain.
// Attempts records every source consulted, in order and in the same shape
// the cleanup port resolver reports, so callers can splice the trail into
// operator-facing fallback warnings.
type liveDoltPortResolution struct {
	Port     int
	Source   string
	Attempts []PortResolutionAttempt
}

// liveDoltPortResolver resolves the managed dolt SQL port for a city from
// live state only. The function fields are seams for unit tests; production
// callers construct it via newLiveDoltPortResolver.
type liveDoltPortResolver struct {
	// managedHandlePort returns the validated managed-server port from the
	// live runtime handle, or "" when unavailable.
	managedHandlePort func(cityPath string) string
	// runtimeLayout resolves the city's managed dolt runtime layout (data
	// dir and config file) used to match process-table candidates.
	runtimeLayout func(cityPath string) (managedDoltRuntimeLayout, error)
	// discoverProcesses enumerates live `dolt sql-server` processes with
	// their listening ports (lsof/ps on hosts without /proc).
	discoverProcesses func() ([]DoltProcInfo, error)
}

// newLiveDoltPortResolver returns the production resolver wired to the
// managed runtime handle and the process-table discovery helpers. Its match
// layout honors the GC_DOLT_* environment (suitable for operating within a
// city's own runtime context).
func newLiveDoltPortResolver() liveDoltPortResolver {
	return liveDoltPortResolver{
		managedHandlePort: currentResolvableManagedDoltPort,
		runtimeLayout:     resolveManagedDoltRuntimeLayout,
		discoverProcesses: discoverDoltProcesses,
	}
}

// newLiveDoltPortResolverForExplicitCity is the resolver for destructive,
// target-selecting callers (gc dolt cleanup) where an explicit --city must be
// authoritative. It resolves the match layout strictly from cityPath, ignoring
// the GC_DOLT_*/GC_PACK_STATE_DIR overrides, so ambient env from a different
// city cannot redirect the process match onto a foreign live server (which
// cleanup would then resolve and connect to). See
// resolveManagedDoltRuntimeLayoutStrict.
func newLiveDoltPortResolverForExplicitCity() liveDoltPortResolver {
	r := newLiveDoltPortResolver()
	r.runtimeLayout = resolveManagedDoltRuntimeLayoutStrict
	return r
}

// resolve runs the live resolution chain for cityPath. On success the
// returned resolution carries the port and the winning source; on failure
// the error explains why and the attempt trail records every source
// consulted. A nil error implies a valid Port.
//
// Attempts is load-bearing, not just diagnostics. ResolveDoltPort treats any
// attempt with Status "error" as a hard stop: it returns Port 0 instead of
// falling through to LegacyDefaultDoltPort (3307). So a garbled managed
// handle — a runtime handle carrying an unparseable or out-of-range port —
// is recorded here as "error" and, if no later step resolves a port,
// suppresses the legacy default for the whole chain. That is deliberate and
// conservative: it mirrors the historical bad-port-file hard stop. A handle
// we cannot parse means the managed runtime state is corrupt, and guessing
// 3307 would point a destructive caller (gc dolt cleanup) at whatever
// happens to be listening there. A merely absent handle records "not-found"
// instead and does leave the legacy default reachable.
//
// An invalid handle does not by itself fail the chain: if step 2 resolves an
// unambiguous live listener, that wins (see
// TestLiveDoltPortResolver_InvalidHandleValueFallsThrough). The suppression
// only bites when the chain would otherwise reach the legacy default.
func (r liveDoltPortResolver) resolve(cityPath string) (liveDoltPortResolution, error) {
	res := liveDoltPortResolution{}
	if strings.TrimSpace(cityPath) == "" {
		res.Attempts = append(res.Attempts,
			PortResolutionAttempt{Source: liveDoltHandleSource, Status: "not-provided"},
			PortResolutionAttempt{Source: liveDoltProcessSource, Status: "not-provided"},
		)
		return res, fmt.Errorf("no city path provided: %w", errNoLiveDoltEndpoint)
	}

	// Step 1: managed-server live handle.
	if raw := strings.TrimSpace(r.managedHandlePort(cityPath)); raw != "" {
		port, err := strconv.Atoi(raw)
		if err == nil && validDoltPort(port) {
			res.Attempts = append(res.Attempts, PortResolutionAttempt{
				Source: liveDoltHandleSource,
				Status: "found",
				Detail: raw,
			})
			res.Port = port
			res.Source = liveDoltHandleSource
			return res, nil
		}
		res.Attempts = append(res.Attempts, PortResolutionAttempt{
			Source: liveDoltHandleSource,
			Status: "error",
			Detail: fmt.Sprintf("invalid managed runtime port %q", raw),
		})
	} else {
		res.Attempts = append(res.Attempts, PortResolutionAttempt{Source: liveDoltHandleSource, Status: "not-found"})
	}

	// Step 2: process-table discovery scoped to the managed runtime layout.
	layout, err := r.runtimeLayout(cityPath)
	if err != nil {
		res.Attempts = append(res.Attempts, PortResolutionAttempt{
			Source: liveDoltProcessSource,
			Status: "error",
			Detail: fmt.Sprintf("resolve managed dolt runtime layout: %v", err),
		})
		return res, fmt.Errorf("resolving managed dolt runtime layout for %s: %w", cityPath, err)
	}
	procs, err := r.discoverProcesses()
	if err != nil {
		res.Attempts = append(res.Attempts, PortResolutionAttempt{
			Source: liveDoltProcessSource,
			Status: "error",
			Detail: fmt.Sprintf("discover dolt processes: %v", err),
		})
		return res, fmt.Errorf("discovering dolt processes for %s: %w", cityPath, err)
	}
	ports := map[int]struct{}{}
	for _, proc := range procs {
		if !doltProcMatchesManagedLayout(proc, layout) {
			continue
		}
		for _, port := range proc.Ports {
			if validDoltPort(port) {
				ports[port] = struct{}{}
			}
		}
	}
	switch len(ports) {
	case 0:
		res.Attempts = append(res.Attempts, PortResolutionAttempt{Source: liveDoltProcessSource, Status: "not-found"})
		return res, fmt.Errorf("%w for %s", errNoLiveDoltEndpoint, cityPath)
	case 1:
		for port := range ports {
			res.Attempts = append(res.Attempts, PortResolutionAttempt{
				Source: liveDoltProcessSource,
				Status: "found",
				Detail: strconv.Itoa(port),
			})
			res.Port = port
			res.Source = liveDoltProcessSource
		}
		return res, nil
	default:
		sorted := make([]int, 0, len(ports))
		for port := range ports {
			sorted = append(sorted, port)
		}
		sort.Ints(sorted)
		detail := fmt.Sprintf("ambiguous live dolt listeners for %s on ports %v; pass --port to disambiguate", cityPath, sorted)
		res.Attempts = append(res.Attempts, PortResolutionAttempt{
			Source: liveDoltProcessSource,
			Status: "error",
			Detail: detail,
		})
		return res, errors.New(detail)
	}
}

// doltProcMatchesManagedLayout reports whether a discovered dolt sql-server
// process serves the city's managed runtime layout, matched by its --config
// or --data-dir argv value (path-normalized via samePath).
func doltProcMatchesManagedLayout(p DoltProcInfo, layout managedDoltRuntimeLayout) bool {
	if cfg := extractConfigPath(p.Argv); cfg != "" && strings.TrimSpace(layout.ConfigFile) != "" && samePath(cfg, layout.ConfigFile) {
		return true
	}
	if dd, ok := argvFlagValue(p.Argv, "--data-dir"); ok && dd != "" && strings.TrimSpace(layout.DataDir) != "" && samePath(dd, layout.DataDir) {
		return true
	}
	return false
}
