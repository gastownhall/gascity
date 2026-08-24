package splittest

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// The pinned-id fence, as the kit models it.
//
// A store serving a class binding is fenced to the namespaces that binding
// claims: minting settles the ids a store GENERATES, and Create honors a pinned
// id verbatim, so an unfenced binding accepts a foreign bead and its namespace
// claim quietly stops holding. Both shipped providers apply the rule
// (internal/storebinding/sqlite and .../beadsworkspace both pass
// storebinding.EngineReservedPrefixes into the store option), which is why the
// kit's class leaves have to apply it too — a leaf that accepted what the real
// binding refuses is green on writes production would have rejected.
//
// This is a SECOND spelling of internal/beads/pinned_id_fence.go's rule, and
// that file's own doc says a second spelling is how two fences drift apart. It
// is here anyway because the alternative is exporting a production API for a
// test kit's benefit. What holds the two together is not the code: it is that
// this leaf is run against the same PROVIDER contract the real stores are,
// beadstest.RunPinnedIDFenceConformance (see fence_conformance_test.go). A
// divergence fails that suite here exactly as it would fail it there.
//
// The rules below are the ones the conformance suite pins, in its words:
// membership is tested on the SEPARATOR (plain containment admits "gcnx-1",
// which belongs to no binding at all), on the id EXACTLY as it will be stored
// (trimming first certifies a membership the persisted row does not have), and
// with case folded but not rewritten. An empty id is a MINT request rather than
// a pin, and an empty namespace set leaves the store unfenced — the shipped
// default everywhere the store is not a class binding.
func fencedPinnedID(id string, namespaces []string) error {
	if len(namespaces) == 0 || id == "" {
		return nil
	}
	lowered := strings.ToLower(id)
	for _, prefix := range namespaces {
		if strings.HasPrefix(lowered, prefix+"-") {
			return nil
		}
	}
	return fmt.Errorf("strict create: id %q is outside this store's namespaces (%s): a pinned id must carry one of them, or use the foreign-id create the store migration uses: %w", id, strings.Join(namespaces, ", "), beads.ErrPinnedIDOutsideNamespace)
}

// normalizeNamespaces prepares a configured namespace set for the fence,
// dropping the empties normalization produces.
//
// Like production's collectReservedIDPrefixes it does NOT branch on the result
// being empty: an empty set has to reach the unfenced case, which is the
// control that tells a fence from a blanket refusal.
func normalizeNamespaces(prefixes []string) []string {
	if len(prefixes) == 0 {
		return nil
	}
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if p = normalizePrefix(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
