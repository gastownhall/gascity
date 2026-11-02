package main

import (
	"fmt"
	"strconv"
	"strings"
)

// LegacyDefaultDoltPort is the historical hard-coded port used by the
// shell-side cleanup script when no other source can be resolved.
const LegacyDefaultDoltPort = 3307

const maxTCPPort = 65535

const flagDoltPortSource = "--port flag"

const cityConfigDoltPortSource = "city config dolt.port"

// PortResolverInput bundles the inputs needed for the dolt port discovery
// chain (per AD-04 §4.1, live-state revision per city-scale plan P1.7).
type PortResolverInput struct {
	// Flag carries the --port flag value (empty if not provided).
	Flag string
	// CityPort is the city.toml [dolt] port. Zero means "not set".
	CityPort int
	// CityPath roots the live managed-dolt resolution steps (runtime
	// handle, then process-table discovery). Empty records both live
	// sources as not-provided.
	CityPath string
	// LiveResolve overrides the live resolution chain in tests. Nil uses
	// newLiveDoltPortResolver().resolve.
	LiveResolve func(cityPath string) (liveDoltPortResolution, error)
}

// resolverRig is the minimum rig info needed by ResolveDoltPort. It is
// intentionally not the same type as RigListItem so the resolver does not
// reach into HTTP/CLI types.
type resolverRig struct {
	Name string
	Path string
	HQ   bool
}

// PortResolution describes the outcome of the dolt port discovery chain.
// Source identifies the winning input; Tried records every source consulted,
// in order, so callers can render a port-fallback warning that explains why
// each higher-priority source missed.
type PortResolution struct {
	Port     int
	Source   string
	Fallback bool
	Tried    []PortResolutionAttempt
}

// PortResolutionAttempt captures a single source consulted by the resolver.
// Status is one of: "not-provided", "not-set", "not-found", "found", "error".
type PortResolutionAttempt struct {
	Source string
	Status string
	Detail string
}

// ResolveDoltPort applies the discovery chain (AD-04 §4.1, revised by the
// city-scale plan P1.7 port-file de-authority):
//
//	--port flag > city.toml dolt.port > live managed dolt (runtime handle, then process table) > legacy default 3307
//
// <rigRoot>/.beads/dolt-server.port is deliberately NOT in the chain: it is
// a bd compatibility status file whose surviving writer has clobbered it in
// production (incident class 4); see dolt_port_live.go.
//
// Returns a PortResolution; Fallback is true only when the legacy default
// is selected. Never returns an error — caller decides whether the warn
// state is fatal. A live-step error (ambiguous listeners, discovery
// failure) stops the chain with Port 0 instead of falling through to the
// legacy default, mirroring the historical bad-port-file hard stop.
func ResolveDoltPort(in PortResolverInput) PortResolution {
	res := PortResolution{}

	attempt, port, ok := tryFlagPort(in.Flag)
	if ok {
		res.Tried = append(res.Tried, attempt)
		res.Port = port
		res.Source = attempt.Source
		return res
	}
	res.Tried = append(res.Tried, attempt)

	attempt, port, ok = tryCityConfigPort(in.CityPort)
	if ok {
		res.Tried = append(res.Tried, attempt)
		res.Port = port
		res.Source = attempt.Source
		return res
	}
	res.Tried = append(res.Tried, attempt)

	liveResolve := in.LiveResolve
	if liveResolve == nil {
		liveResolve = newLiveDoltPortResolverForExplicitCity().resolve
	}
	live, liveErr := liveResolve(in.CityPath)
	res.Tried = append(res.Tried, live.Attempts...)
	if liveErr == nil && validDoltPort(live.Port) {
		res.Port = live.Port
		res.Source = live.Source
		return res
	}
	for _, attempt := range live.Attempts {
		if attempt.Status == "error" {
			res.Source = attempt.Source
			return res
		}
	}

	// Legacy default — record an attempt for the trail.
	res.Tried = append(res.Tried, PortResolutionAttempt{
		Source: "legacy default",
		Status: "found",
		Detail: strconv.Itoa(LegacyDefaultDoltPort),
	})
	res.Port = LegacyDefaultDoltPort
	res.Source = "legacy default"
	res.Fallback = true
	return res
}

func tryFlagPort(flag string) (PortResolutionAttempt, int, bool) {
	src := flagDoltPortSource
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return PortResolutionAttempt{Source: src, Status: "not-provided"}, 0, false
	}
	port, err := strconv.Atoi(flag)
	if err != nil {
		return PortResolutionAttempt{
			Source: src,
			Status: "error",
			Detail: fmt.Sprintf("invalid port %q: %v", flag, err),
		}, 0, false
	}
	if !validDoltPort(port) {
		return PortResolutionAttempt{
			Source: src,
			Status: "error",
			Detail: invalidDoltPortMessage(port),
		}, 0, false
	}
	return PortResolutionAttempt{Source: src, Status: "found", Detail: strconv.Itoa(port)}, port, true
}

func tryCityConfigPort(port int) (PortResolutionAttempt, int, bool) {
	src := cityConfigDoltPortSource
	if port == 0 {
		return PortResolutionAttempt{Source: src, Status: "not-set"}, 0, false
	}
	if !validDoltPort(port) {
		return PortResolutionAttempt{
			Source: src,
			Status: "error",
			Detail: invalidDoltPortMessage(port),
		}, 0, false
	}
	return PortResolutionAttempt{Source: src, Status: "found", Detail: strconv.Itoa(port)}, port, true
}

func validDoltPort(port int) bool {
	return port >= 1 && port <= maxTCPPort
}

func invalidDoltPortMessage(port int) string {
	return fmt.Sprintf("invalid port %d (must be between 1 and %d)", port, maxTCPPort)
}

// orderRigsHQFirst returns the rigs reordered so the HQ rig (if any) is
// consulted before non-HQ rigs. Original order is preserved among HQ rigs
// and among non-HQ rigs respectively.
func orderRigsHQFirst(rigs []resolverRig) []resolverRig {
	out := make([]resolverRig, 0, len(rigs))
	for _, r := range rigs {
		if r.HQ {
			out = append(out, r)
		}
	}
	for _, r := range rigs {
		if !r.HQ {
			out = append(out, r)
		}
	}
	return out
}
