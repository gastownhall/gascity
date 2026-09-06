package beads

import (
	"encoding/json"
	"strings"
)

// DepMetadataReader is the optional surface a Store implements when it can
// report the opaque payload stored on one dependency edge.
//
// It is optional because the payload is not part of the Dep wire model: Dep
// carries the pair and the type, and a caller that needs to know whether an
// edge holds more than that has to ask the store directly. Assert once and
// treat a store that does not implement it as UNABLE TO ANSWER — never as
// answering no. The infra-class migration copied edges for months on exactly
// that conflation.
type DepMetadataReader interface {
	// DepMetadata returns the payload on the issueID -> dependsOnID edge and
	// whether one is carried. A missing edge and an edge with no payload both
	// answer ("", false, nil); only a failed read is an error.
	DepMetadata(issueID, dependsOnID string) (string, bool, error)
}

// DepMetadataWriter is the optional surface a Store implements when it can
// record an edge TOGETHER WITH the payload it carried.
//
// It is the write half of DepMetadataReader and exists for the same reason: Dep
// carries the pair and the type, so DepAdd has no argument a payload could
// travel in. A copy that re-added edges through DepAdd alone did not merely
// fail to carry the payload — on a store that keeps the payload in a sidecar,
// DepAdd CLEARS it — so the missing write surface was a silent destruction, not
// a silent omission.
//
// It is deliberately not an expansion of Dep. That model is stable and read by
// every store in the tree; the payload is a projection two engines happen to
// keep, and widening the wire model to carry it would put a field on every
// DepList result that almost no caller can populate. Assert for this interface
// and treat a store that does not implement it as UNABLE TO CARRY — a copy that
// treated it as "nothing to carry" would be making the same conflation
// DepMetadataReader's doc comment warns about, from the other side.
type DepMetadataWriter interface {
	// DepAddWithMetadata records the issueID -> dependsOnID edge and stores
	// metadata as its payload. An empty metadata leaves the edge carrying
	// nothing, which must stay distinguishable from carrying an empty payload:
	// callers hand this the value a DepMetadataReader gave them, and both
	// engines report "carries nothing" as the empty string.
	DepAddWithMetadata(issueID, dependsOnID, depType, metadata string) error
}

// DepMetadataCarries reports whether a metadata column value is an actual
// payload rather than an engine's rendering of none.
//
// The two engines spell "no payload" differently. SQLite stores nothing at all,
// so it reads back as the empty string. Dolt types the column as JSON, so an
// edge added with no metadata reads back as "{}" — not empty and not NULL.
// Calling that a payload would make every Dolt-sourced edge look like it held
// one, which turns a migration's refusal into a blanket refusal and teaches an
// operator to ignore it. So a value that decodes to nothing — empty object,
// empty array, null — carries nothing.
//
// A value that is not JSON at all is treated as carrying. The callers are
// checks that exist so a copy cannot silently drop a payload, and the safe
// answer to "I cannot tell what this is" is that something is there.
func DepMetadataCarries(metadata string) bool {
	trimmed := strings.TrimSpace(metadata)
	if trimmed == "" {
		return false
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return true
	}
	switch value := decoded.(type) {
	case nil:
		return false
	case map[string]any:
		return len(value) > 0
	case []any:
		return len(value) > 0
	default:
		return true
	}
}
