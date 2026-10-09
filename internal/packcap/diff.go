package packcap

import (
	"sort"
	"strings"
)

// Verdict classifies the difference between two manifests.
type Verdict string

// The four verdicts, from most to least severe.
const (
	// Breaking means a commitment was removed or changed.
	Breaking Verdict = "BREAKING"
	// Additive means a commitment was added and none was removed.
	Additive Verdict = "ADDITIVE"
	// Unclassified means prose changed and no computable commitment did.
	Unclassified Verdict = "UNCLASSIFIED"
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
	// MovedFiles are the prose files whose OPAQUE digest differs, were
	// added, or were removed, sorted.
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

	removed, added := setDiff(a.Values(Opaque), b.Values(Opaque))
	r.MovedFiles = opaquePaths(append(removed, added...))

	r.Verdict = None
	for _, f := range r.Findings {
		if f.Verdict == Breaking {
			r.Verdict = Breaking
			break
		}
		r.Verdict = Additive
	}
	if r.Verdict == None && len(r.MovedFiles) > 0 {
		r.Verdict = Unclassified
	}
	return r
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

// opaquePaths returns the sorted, distinct file paths of OPAQUE values,
// which have the form "<path> <digest>".
func opaquePaths(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		path := v
		if i := strings.LastIndexByte(v, ' '); i >= 0 {
			path = v[:i]
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}
