package main

import (
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	// proxiedNativeAuthorName is the author every gc write through a proxied
	// window carries. PR2 serves no writes, but beads latches the pair AT OPEN
	// (applyConfigDefaults -> the store instance -> commitAuthorString ->
	// DOLT_COMMIT --author), so the value projected here is what PR3's
	// authorship will read.
	proxiedNativeAuthorName = "gc"
	// proxiedNativeAuthorFallbackCity is the email anchor when a city has no
	// name. The anchor is the city NAME and not its path (decision Q4): path and
	// runtime dir both change when a city is moved or reinstalled, which would
	// rewrite authorship for the same logical city, and an author string must
	// never carry a host path.
	proxiedNativeAuthorFallbackCity = "unknown-city"

	// proxiedNativeOneShotMaxConns caps a one-shot CLI open at the single
	// connection a serial command actually uses, exactly as
	// nativeDoltCliPoolCap does for a direct open.
	proxiedNativeOneShotMaxConns = 1
	// proxiedNativeLongLivedMaxConns is the ceiling for a store held for the
	// process lifetime. It is deliberately small: the pool is not gc's server,
	// it is a handful of connections against a proxy bd owns and counts for its
	// own idle policy, and the library's daemon default of 10 would let one
	// controller hold more sockets open against somebody else's proxy than the
	// whole rest of the process needs.
	proxiedNativeLongLivedMaxConns = 4
)

// nativeDoltProxiedOpenEnvForPin builds the environment the linked beads
// library is opened with over bd's proxy, FROM THE PIN AND NOTHING ELSE.
//
// This is the smallest map in the tree that does a real job, and its smallness
// is the design:
//
//   - it names the endpoint the admission pin resolved, so the library dials the
//     generation that was gated rather than whatever a port file says now;
//   - it says SERVER MODE with AUTO_START off, so the library treats the server
//     as external (IsDoltServerMode -> ServerModeExternal -> no auto-start) and
//     gc cannot spawn a Dolt process for a scope bd owns;
//   - it projects the author pair, which the library latches at open;
//   - and it names nothing else at all.
//
// In particular it does NOT call bdRuntimeEnvWithErrorRecoveryContext, the
// projection every other native open path uses. That function resolves a
// MANAGED Dolt server and, with recovery enabled, starts or repairs one. On a
// proxied scope there is no managed server to repair: the Dolt child belongs to
// bd's proxy, and a recovery pass fired from a read-store open would be gc
// reaching into a lifecycle it deliberately does not own. The env window
// (OpenNativeDoltStoreAtProxied) withholds the whole BEADS_ and BD_ namespaces
// around the open, so every key this map omits is actively UNSET for the
// duration — including BEADS_DOLT_PROXIED_SERVER, which beads v1.3.0 reads only
// in cmd/bd (init.go:450, :2938, :3501) and never under the library storage
// path. Scrubbing it therefore cannot change library behavior; it keeps the
// window honest if a future bd moves that read (plan Q3).
func nativeDoltProxiedOpenEnvForPin(cityName string, pin beads.Pin, longLived bool) map[string]string {
	maxConns := proxiedNativeOneShotMaxConns
	if longLived {
		maxConns = proxiedNativeLongLivedMaxConns
	}
	return map[string]string{
		"BEADS_DOLT_SERVER_MODE":     "1",
		"BEADS_DOLT_SERVER_HOST":     proxyendpoint.Host,
		"BEADS_DOLT_SERVER_PORT":     strconv.Itoa(pin.Port()),
		"BEADS_DOLT_SERVER_USER":     proxiedNativeDoltUser,
		"BEADS_DOLT_SERVER_DATABASE": pin.Database(),
		"BEADS_DOLT_AUTO_START":      "0",
		"BEADS_DOLT_MAX_CONNS":       strconv.Itoa(maxConns),
		"GIT_AUTHOR_NAME":            proxiedNativeAuthorName,
		"GIT_AUTHOR_EMAIL":           proxiedNativeAuthorEmail(cityName),
	}
}

// proxiedNativeDoltUser is the user bd's proxy authenticates its own sessions
// as, and the one proxyendpoint's probe already uses. A proxied database is not
// credentialed for gc separately: the proxy is loopback-only by construction and
// the Dolt child behind it is bd's.
const proxiedNativeDoltUser = "root"

// proxiedNativeAuthorEmail renders gc@<city name>, falling back to a constant
// when the city has no usable name.
//
// The city NAME is the anchor (decision Q4) because it is the one identity that
// survives a move or a reinstall: a path-derived anchor would rewrite the author
// of the same logical city's future commits and would put a host path inside a
// Dolt commit that anybody can read.
//
// Note for the next reader: the decision doc cites citylayout's GC_CITY as "the
// name". It is not — CityRuntimeEnvMapForRuntimeDir sets GC_CITY to the city
// ROOT PATH (runtime.go), so using that anchor would have produced exactly the
// leak the decision forbids. The configured workspace name is the value the
// decision describes, and anything that still looks like a path (or carries
// whitespace) is refused here rather than trusted.
func proxiedNativeAuthorEmail(cityName string) string {
	name := strings.TrimSpace(cityName)
	if name == "" || strings.ContainsAny(name, "/\\ \t") {
		name = proxiedNativeAuthorFallbackCity
	}
	return proxiedNativeAuthorName + "@" + name
}

// proxiedNativeAuthorCityName is the configured workspace name, resolved once at
// opener construction so a per-open projection never re-reads the city config.
func proxiedNativeAuthorCityName(cfg *config.City) string {
	return config.EffectiveCityName(cfg, "")
}
