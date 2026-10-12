package packcap

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/orders"
)

// Verdict classifies the difference between two manifests.
type Verdict string

// The four verdicts, in order of precedence: BREAKING > UNCLASSIFIED >
// ADDITIVE > NONE. The overall verdict is the highest that applies.
const (
	// Breaking means a commitment was removed or changed.
	Breaking Verdict = "BREAKING"
	// Unclassified means a file moved that no computable change accounts
	// for, and no commitment was removed or changed.
	Unclassified Verdict = "UNCLASSIFIED"
	// Additive means a commitment was added, none was removed or changed,
	// and every moved file belongs to an added provider.
	Additive Verdict = "ADDITIVE"
	// None means the manifests are identical.
	None Verdict = "NONE"
)

// ExitCode is the process exit status for a verdict: 2 for BREAKING, 1 for
// UNCLASSIFIED, and 0 for ADDITIVE and NONE.
func (v Verdict) ExitCode() int {
	switch v {
	case Breaking:
		return 2
	case Unclassified:
		return 1
	default:
		return 0
	}
}

// Change says whether values appear only in the newer manifest or only in
// the older one.
type Change string

// The two directions of change.
const (
	Added   Change = "added"
	Removed Change = "removed"
)

// Finding is one categorized difference: values of one property that were
// added or removed, and how that change is classified.
type Finding struct {
	Property Property `json:"property"`
	Change   Change   `json:"change"`
	Verdict  Verdict  `json:"verdict"`
	Values   []string `json:"values"`
}

// Caption describes the finding in words, for human output.
func (f Finding) Caption() string {
	switch f.Property {
	case Mandates, Norms:
		if f.Change == Removed {
			return string(f.Property) + " no longer stated"
		}
		return string(f.Property) + " newly stated"
	case Demands:
		if f.Change == Added {
			return string(f.Property) + " added: a new requirement on callers"
		}
		return string(f.Property) + " dropped"
	default:
		return string(f.Property) + " " + string(f.Change)
	}
}

// Result is the classified difference between two manifests.
type Result struct {
	// Verdict is the overall classification.
	Verdict Verdict
	// Findings are the computable changes, in property order.
	Findings []Finding
	// MovedFiles are the files whose OPAQUE digest differs, that were
	// added, or that were removed, sorted, leaving out a file added with an
	// added PROVIDES value or removed with a removed one (for example
	// formulas/review.toml with formula:review). Any moved file makes the
	// verdict at least UNCLASSIFIED.
	MovedFiles []string
}

// classify returns how a change to a property is classified. PROVIDES and
// USES: removal breaks, addition is additive. MANDATES and NORMS: any change
// breaks, since a rule stated in prose is still a rule. DEMANDS: addition
// breaks, since it is a new requirement on callers, and removal is additive.
func classify(p Property, c Change) Verdict {
	switch p {
	case Mandates, Norms:
		return Breaking
	case Demands:
		if c == Added {
			return Breaking
		}
		return Additive
	default:
		if c == Removed {
			return Breaking
		}
		return Additive
	}
}

// Diff classifies the change from manifest a (older) to manifest b (newer).
func Diff(a, b Manifest) Result {
	var r Result
	for _, p := range Properties {
		if p == Opaque {
			continue
		}
		removed, added := setDiff(a.Values(p), b.Values(p))
		order := []Change{Removed, Added}
		if p == Demands {
			order = []Change{Added, Removed}
		}
		for _, c := range order {
			values := removed
			if c == Added {
				values = added
			}
			if len(values) == 0 {
				continue
			}
			r.Findings = append(r.Findings, Finding{Property: p, Change: c, Verdict: classify(p, c), Values: values})
		}
	}

	r.MovedFiles = movedFiles(a, b)

	r.Verdict = None
	for _, f := range r.Findings {
		if f.Verdict == Breaking {
			r.Verdict = Breaking
			break
		}
		r.Verdict = Additive
	}
	if r.Verdict != Breaking && len(r.MovedFiles) > 0 {
		r.Verdict = Unclassified
	}
	return r
}

// movedFiles returns the files whose OPAQUE digest moved that no added or
// removed PROVIDES value accounts for. A file accounts for itself only when
// it appears or disappears together with the provider it belongs to; a
// changed file is never accounted for.
func movedFiles(a, b Manifest) []string {
	oldDigests, newDigests := opaqueByPath(a.Values(Opaque)), opaqueByPath(b.Values(Opaque))
	removedProviders, addedProviders := setDiff(a.Values(Provides), b.Values(Provides))
	accounted := func(path string, providers []string) bool {
		owners := providerOwners(path)
		for _, p := range providers {
			for _, o := range owners {
				if p == o {
					return true
				}
			}
		}
		return false
	}

	var out []string
	for path, d := range oldDigests {
		nd, ok := newDigests[path]
		switch {
		case !ok && accounted(path, removedProviders):
		case !ok || nd != d:
			out = append(out, path)
		}
	}
	for path := range newDigests {
		if _, ok := oldDigests[path]; !ok && !accounted(path, addedProviders) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// providerOwners returns the PROVIDES values a pack-relative file belongs to
// by the loader's conventions: agents/<name>/..., roles/agents/<name>/...,
// commands/<words...>/..., formulas/<name>.toml and orders/<name>.toml.
func providerOwners(path string) []string {
	parts := strings.Split(path, "/")
	switch {
	case len(parts) >= 3 && parts[0] == "agents":
		return []string{"agent:" + parts[1]}
	case len(parts) >= 4 && parts[0] == "roles" && parts[1] == "agents":
		return []string{"agent:" + parts[2]}
	case len(parts) >= 3 && parts[0] == "commands":
		var out []string
		for n := 1; n <= len(parts)-2; n++ {
			out = append(out, "command:"+strings.Join(parts[1:1+n], " "))
		}
		return out
	case len(parts) == 2 && parts[0] == "formulas":
		if name, ok := formula.TrimTOMLFilename(parts[1]); ok && name != "" {
			return []string{"formula:" + name}
		}
	case len(parts) == 2 && parts[0] == "orders":
		if name, ok := orders.TrimFlatOrderFilename(parts[1]); ok && name != "" {
			return []string{"order:" + name}
		}
	}
	return nil
}

// opaqueByPath maps each OPAQUE value, "<path> <digest>", to its path and
// digest.
func opaqueByPath(values []string) map[string]string {
	out := make(map[string]string, len(values))
	for _, v := range values {
		if i := strings.LastIndexByte(v, ' '); i >= 0 {
			out[v[:i]] = v[i+1:]
		}
	}
	return out
}

// setDiff returns the values only in a and the values only in b, sorted.
func setDiff(a, b []string) (onlyA, onlyB []string) {
	inA := make(map[string]bool, len(a))
	for _, v := range a {
		inA[v] = true
	}
	inB := make(map[string]bool, len(b))
	for _, v := range b {
		inB[v] = true
		if !inA[v] {
			onlyB = append(onlyB, v)
		}
	}
	for _, v := range a {
		if !inB[v] {
			onlyA = append(onlyA, v)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return onlyA, onlyB
}
