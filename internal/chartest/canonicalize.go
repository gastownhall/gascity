// Package chartest provides the reusable, transport-agnostic core of the CLI
// handler unification's three-lane characterization harness: first-occurrence
// canonicalization of volatile tokens and per-lane golden comparison. The
// main-package glue that drives the route<X>/CityClient seams and stands up the
// in-process server lives in cmd/gc test files; the logic that is worth unit
// testing on its own lives here.
package chartest

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
)

// Rule maps a volatile-token pattern to a placeholder category. Category is the
// placeholder prefix (e.g. "BEAD", "T", "CONVOY"); Pattern matches the raw
// token. Patterns handed to one Canonicalizer should be mutually non-overlapping
// in the tokens they match — a real bead id should match only the BEAD rule.
type Rule struct {
	Category string
	Pattern  *regexp.Regexp
}

// Stream is a named byte stream (e.g. "stdout", "json") for cross-surface
// canonicalization in an explicit order.
type Stream struct {
	Name string
	Data []byte
}

// Canonicalizer replaces volatile tokens (minted ids, timestamps) with stable
// first-occurrence placeholders (BEAD-1, T-1, …) so per-lane goldens are
// deterministic. A distinct real token is assigned "<Category>-<n>" the first
// time it is seen and reuses that placeholder everywhere afterward — including
// across every stream fed to the same Canonicalizer — so cross-surface identity
// is asserted rather than erased. Not safe for concurrent use.
//
// Ordering caveat — a canonicalized golden does NOT protect the relative order
// of rows that differ ONLY by volatile tokens. Placeholders are numbered in
// first-occurrence order, so two rows whose sole difference is a minted id or a
// timestamp canonicalize to byte-identical text regardless of the order they
// were emitted in ("b-a …X\nb-b …X" and its reverse both become
// "BEAD-1 …X\nBEAD-2 …X"). A lost or inverted sort among such rows therefore
// passes a byte-exact golden comparison. When characterizing a MULTI-ROW command
// whose output order is a behavioral contract, seed each row with a distinct
// STABLE column (e.g. distinct titles) so a reorder changes the canonicalized
// bytes; do not rely on canonicalization alone to catch a row-order regression.
// See TestCanonicalize_RowOrderBlindSpot and
// TestCanonicalize_StableColumnMakesOrderObservable.
type Canonicalizer struct {
	rules []Rule
	seen  map[string]string
	next  map[string]int
}

// NewCanonicalizer returns a Canonicalizer applying the given rules.
func NewCanonicalizer(rules ...Rule) *Canonicalizer {
	return &Canonicalizer{
		rules: rules,
		seen:  make(map[string]string),
		next:  make(map[string]int),
	}
}

// Canonicalize replaces every rule match in b with its stable placeholder.
// Matches are numbered in left-to-right position order; where two matches
// overlap, the one starting earlier wins and, at the same start, the longer
// one (ties break to the earlier rule).
func (c *Canonicalizer) Canonicalize(b []byte) []byte {
	type match struct {
		start, end int
		category   string
		text       string
		ruleIdx    int
	}
	var matches []match
	for ri, r := range c.rules {
		for _, loc := range r.Pattern.FindAllIndex(b, -1) {
			matches = append(matches, match{
				start:    loc[0],
				end:      loc[1],
				category: r.Category,
				text:     string(b[loc[0]:loc[1]]),
				ruleIdx:  ri,
			})
		}
	}
	if len(matches) == 0 {
		return b
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		if matches[i].end != matches[j].end {
			return matches[i].end > matches[j].end // longer match first at the same start
		}
		return matches[i].ruleIdx < matches[j].ruleIdx
	})

	var out bytes.Buffer
	pos := 0
	for _, m := range matches {
		if m.start < pos {
			continue // overlaps an already-emitted match
		}
		out.Write(b[pos:m.start])
		out.WriteString(c.placeholder(m.category, m.text))
		pos = m.end
	}
	out.Write(b[pos:])
	return out.Bytes()
}

// CanonicalizeStreams canonicalizes each stream in the given order, sharing one
// token→placeholder map so identity holds across surfaces. Numbering follows
// the slice order.
func (c *Canonicalizer) CanonicalizeStreams(streams []Stream) []Stream {
	out := make([]Stream, len(streams))
	for i, s := range streams {
		out[i] = Stream{Name: s.Name, Data: c.Canonicalize(s.Data)}
	}
	return out
}

func (c *Canonicalizer) placeholder(category, text string) string {
	if ph, ok := c.seen[text]; ok {
		return ph
	}
	c.next[category]++
	ph := fmt.Sprintf("%s-%d", category, c.next[category])
	c.seen[text] = ph
	return ph
}
