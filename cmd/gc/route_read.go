package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/api"
)

// fallbackAfterFetch is a sentinel an apiFetch closure may return to force a
// fallback to the local path AFTER a successful API round-trip — for a command
// whose response can indicate "the API can't serve this, use the richer local
// path" (e.g. convoy status on a graph/workflow convoy). routeRead renders it as
// route=fallback reason=<Reason>, bypassing error classification.
type fallbackAfterFetch struct{ Reason string }

func (f fallbackAfterFetch) Error() string { return "fallback-after-fetch: " + f.Reason }

// errorAfterFetch is a sentinel an apiFetch closure may return to force a hard
// api error (route=api reason=error, exit 1) AFTER a successful round-trip — for
// a response that is itself an error condition (e.g. mail count partial results).
// routeRead prints "gc <cmd>: <Detail>", bypassing fallback classification.
type errorAfterFetch struct{ Detail string }

func (e errorAfterFetch) Error() string { return e.Detail }

// routeRead runs the canonical read-path routing ladder shared by every routed
// read command, so the "try the API, classify the error, fall back to the local
// path" fork lives in ONE place instead of being copy-pasted per command. It is
// the collapse of the six-row matrix's routing logic onto a single helper.
//
//   - c == nil: the controller is down or the escape hatch is set — take the
//     local path, logging route=fallback reason=<nilReason>.
//   - c != nil: run apiFetch. On success, log route=api and render via apiRender.
//     On a non-fallbackable error (a remote city never falls back — gate G1),
//     log route=api reason=error, print the error, and exit 1. On a fallbackable
//     error, log route=fallback reason=<classified> and take the local path.
//
// apiFetch performs only the API round-trip(s) and stashes results in closure
// state; apiRender renders them. Keeping fetch and render separate preserves the
// exact stderr ordering — the single route= line precedes any render output.
func routeRead(c *api.Client, cmdName, nilReason string, stderr io.Writer, apiFetch func() error, apiRender func() int, localRender func() int) int {
	if c == nil {
		logRoute(stderr, cmdName, "fallback", nilReason)
		return localRender()
	}
	err := apiFetch()
	if err == nil {
		logRoute(stderr, cmdName, "api", "")
		return apiRender()
	}
	var faf fallbackAfterFetch
	if errors.As(err, &faf) {
		// A remote city is authoritative and must never fall back to the caller's
		// LOCAL store (gate G1). An after-fetch fallback sentinel means the remote
		// API round-tripped but cannot serve this response shape (e.g. a
		// graph/workflow convoy); for a remote client that is a hard error, not a
		// cue to read the operator's own store. Only the local/serverless lane may
		// take the richer local path here. This mirrors the ShouldFallbackForRead
		// gate below, which the errors.As short-circuit would otherwise bypass.
		if c.IsRemote() {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc %s: remote city cannot serve this read (%s)\n", cmdName, faf.Reason) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", faf.Reason)
		return localRender()
	}
	var eaf errorAfterFetch
	if errors.As(err, &eaf) || !api.ShouldFallbackForRead(c, err) {
		logRoute(stderr, cmdName, "api", "error")
		fmt.Fprintf(stderr, "gc %s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	return localRender()
}

// routeReadCmd collapses the resolve boilerplate shared by every routed read
// command's CLI entry point: resolveReadTarget then dispatch — remote → route
// with the remote client and no fallback (gate G1); local → resolve the
// per-command loopback seam and route with fallback. localSeam is the command's
// overridable *APIClient seam (kept per-command so the six-row matrix tests can
// inject fakes); route invokes the command's routeX with the resolved
// (cityPath, client, nilReason). Together with routeRead this is the
// resolver + routing unification the CityClient design calls for.
func routeReadCmd(cmdName string, stderr io.Writer, localSeam func(cityPath string) (*api.Client, string), route func(cityPath string, c *api.Client, nilReason string) int) int {
	return routeReadCmdWithHooks(cmdName, stderr, readCmdHooks{}, localSeam, route)
}

// readCmdHooks are optional overrides for routeReadCmdWithHooks. A zero value
// reproduces routeReadCmd exactly, so all plain callers are unaffected.
type readCmdHooks struct {
	// guard runs AFTER resolveReadTarget (so a resolve error still takes
	// precedence) and BEFORE both the remote dispatch and the local seam — a
	// short-circuit therefore never touches the seam's side effects (the
	// classifyGCNoAPI stderr warning, the controller-liveness probe, config.Load;
	// the exact b4592cb79/6f09f2172 ordering break). It returns (code, stop):
	// stop=true short-circuits the command with code.
	guard func() (code int, stop bool)
	// onResolveErr replaces the default "gc <cmd>: <err>" + exit 1 on a
	// resolveReadTarget error — e.g. mail peek falls back to a local read instead
	// of failing. When nil the default print+exit-1 applies.
	onResolveErr func(err error) int
}

// routeReadCmdWithHooks is routeReadCmd with optional hooks. Ordering contract:
// resolveReadTarget → onResolveErr (or default print+exit-1) on error → guard
// (post-resolve, pre-dispatch) → remote route | local seam+route.
func routeReadCmdWithHooks(cmdName string, stderr io.Writer, hooks readCmdHooks, localSeam func(cityPath string) (*api.Client, string), route func(cityPath string, c *api.Client, nilReason string) int) int {
	remoteC, isRemote, cityPath, err := resolveReadTarget()
	if err != nil {
		if hooks.onResolveErr != nil {
			return hooks.onResolveErr(err)
		}
		fmt.Fprintf(stderr, "gc %s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if hooks.guard != nil {
		if code, stop := hooks.guard(); stop {
			return code
		}
	}
	if isRemote {
		return route("", remoteC, "")
	}
	c, reason := localSeam(cityPath)
	return route(cityPath, c, reason)
}
