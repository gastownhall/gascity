// Package apierr is the registry of machine-readable problem types for the Gas
// City HTTP API. Every error the API can return has a stable code registered
// here; the code is surfaced on the RFC 9457 problem+json body as the canonical
// `type` URN (urn:gascity:error:<code>) plus a convenience `code` member, so an
// autonomous consumer branches on a stable identifier instead of parsing the
// human-readable detail prose.
//
// It mirrors the typed-events registry (internal/events.RegisterPayload): a
// central catalog plus a CI guard that fails the build if the API emits a URN
// that is not registered. Registration happens at package-init time via the
// catalog vars, so Registered() is complete before any route is served.
package apierr

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// URNPrefix is the namespace for every Gas City error type URN. The canonical
// machine code is the segment that follows it: type == URNPrefix + code.
const URNPrefix = "urn:gascity:error:"

// codePattern constrains a machine code to lowercase kebab-case so URNs stay
// stable, greppable, and safe as a wire identifier. A subsystem prefix
// (e.g. "sling-") is encouraged where the code is specific to one surface.
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ProblemType is a registered error kind: a stable machine code, the default
// HTTP status it maps to, a short static Title (RFC 9457), and an optional Doc
// URI for human documentation.
type ProblemType struct {
	Code   string
	Status int
	Title  string
	Doc    string
}

// URN returns the canonical type URN for this problem, urn:gascity:error:<code>.
func (pt ProblemType) URN() string { return URNPrefix + pt.Code }

var (
	mu       sync.RWMutex
	byCode   = map[string]ProblemType{}
	registry []ProblemType
)

// Register records a problem type and returns it, so a catalog entry can be a
// package-level var: `var BeadNotFound = apierr.Register(...)`. It panics on a
// malformed code, a missing status/title, or a conflicting duplicate (a
// programming error surfaced at init). Re-registering an identical entry is a
// no-op, which keeps catalog reloads and test re-imports safe.
func Register(pt ProblemType) ProblemType {
	if !codePattern.MatchString(pt.Code) {
		panic(fmt.Sprintf("apierr: invalid code %q (want lowercase kebab-case)", pt.Code))
	}
	if pt.Status < 400 || pt.Status > 599 {
		panic(fmt.Sprintf("apierr: code %q has non-4xx/5xx status %d", pt.Code, pt.Status))
	}
	if pt.Title == "" {
		panic(fmt.Sprintf("apierr: code %q has empty Title", pt.Code))
	}
	mu.Lock()
	defer mu.Unlock()
	if existing, ok := byCode[pt.Code]; ok {
		if existing != pt {
			panic(fmt.Sprintf("apierr: conflicting re-register of code %q: %+v vs %+v", pt.Code, existing, pt))
		}
		return pt
	}
	byCode[pt.Code] = pt
	registry = append(registry, pt)
	return pt
}

// Lookup returns the problem type for a bare machine code.
func Lookup(code string) (ProblemType, bool) {
	mu.RLock()
	defer mu.RUnlock()
	pt, ok := byCode[code]
	return pt, ok
}

// LookupURN returns the problem type for a full type URN
// (urn:gascity:error:<code>), or false if the string is not such a URN or the
// code is unregistered.
func LookupURN(urn string) (ProblemType, bool) {
	if len(urn) <= len(URNPrefix) || urn[:len(URNPrefix)] != URNPrefix {
		return ProblemType{}, false
	}
	return Lookup(urn[len(URNPrefix):])
}

// Registered returns every registered problem type, sorted by code so callers
// (and the generated spec) get deterministic output.
func Registered() []ProblemType {
	mu.RLock()
	out := make([]ProblemType, len(registry))
	copy(out, registry)
	mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
