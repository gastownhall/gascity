package beads

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/fanout"
)

// Cross-store references: the one place gc decides what a blocking edge's
// target names and how to read the beads such targets name.
//
// bd (beads v1.3.2, issueops.IsExternalDepTarget) never blocks a dependent on
// an "external:" target or on a target whose id prefix differs from the
// dependent's own. gc keeps that answer byte for byte on a city with no class
// binding. On a split city gc enforces the owner rule instead — a blocker
// held in another store blocks until it is closed wherever it is held — and to
// do that it must say which bead an edge names:
//
//   - "external:<id>" names the bead <id> held somewhere else: the prefix is
//     stripped and the real bead is looked up.
//   - "external:<rig>:<capability>" names a capability, not a bead. bd treats
//     it as never blocking, and so does gc.
//   - anything else names a bead id as written.

// externalDepPrefix is bd's marker for a dependency target outside the store.
const externalDepPrefix = "external:"

// ExternalBlockerTarget reports which bead a blocking edge's target names.
// capability is true for an "external:<rig>:<capability>" ref, which names no
// bead and never blocks; target is then "". Otherwise target is the bead id,
// with an "external:" marker stripped.
func ExternalBlockerTarget(dependsOnID string) (target string, capability bool) {
	id := strings.TrimSpace(dependsOnID)
	rest, ok := strings.CutPrefix(id, externalDepPrefix)
	if !ok {
		return id, false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.Contains(rest, ":") {
		return "", true
	}
	return rest, false
}

// BdIDPrefix is bd's id prefix (types.ExtractPrefix): everything up to and
// including the first "-", or "" for an id with none. bd judges a dependency
// target with the same prefix as its dependent as held by the dependent's own
// database, and any other as external.
func BdIDPrefix(id string) string {
	idx := strings.Index(id, "-")
	if idx < 0 {
		return ""
	}
	return id[:idx+1]
}

// getManyChunk bounds one batched read's id list.
const getManyChunk = 200

// getManyFanOut bounds the concurrent Gets that read a store with no exact
// batch read.
const getManyFanOut = 8

// GetMany reads the beads ids name from store, open and closed alike, and
// returns the ones the store holds. An id the store does not hold is simply
// absent from the result; a failed read is an error.
//
// Every read is keyed by id and goes through the store's LIVE handle, so a
// caching store answers from its backing store, never from its read cache:
//
//   - Where the live handle reaches an exact batch read (ExactBatchGetter: bd,
//     the native Dolt store and the proxied store, through each wrapper that
//     forwards it), one batched read per getManyChunk ids. An id the batch
//     leaves unresolved reads as absent. bd's batch is `bd show`, which reads
//     only the issues table, so a wisp a bd ledger holds reads as absent here.
//   - Everywhere else, one Get per id, at most getManyFanOut at once. A miss
//     (a substring collision included), a Get that answers a different bead,
//     and a store stating it holds no row under a relocated class's reserved
//     prefix (ErrBdSQLClassRelocated) read as absent.
//
// It never lists. Neither bd (`bd list` has no --id) nor the native store can
// narrow a listing to ids, so a listing by id walks the whole ledger, both
// tiers, closed rows included (#8360 review, iteration 2).
func GetMany(store Store, ids []string) (map[string]Bead, error) {
	out := make(map[string]Bead, len(ids))
	if store == nil || len(ids) == 0 {
		return out, nil
	}
	seen := make(map[string]bool, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	live := HandlesFor(store).Live
	getter, batched := live.(ExactBatchGetter)
	for start := 0; start < len(unique); start += getManyChunk {
		chunk := unique[start:min(start+getManyChunk, len(unique))]
		if batched {
			found, _, err := getter.GetExactBatch(chunk)
			if err == nil {
				for _, id := range chunk {
					if b, ok := found[id]; ok {
						out[id] = b
					}
				}
				continue
			}
			if !errors.Is(err, ErrExactBatchGetUnsupported) {
				return nil, err
			}
			batched = false
		}
		if err := getEach(live, chunk, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// getEach reads ids with one Get each through live, at most getManyFanOut at
// once, and adds every bead the store holds under its exact id to out.
func getEach(live LiveReader, ids []string, out map[string]Bead) error {
	got := make([]Bead, len(ids))
	errs := make([]error, len(ids))
	fanout.Bounded(len(ids), getManyFanOut, func(i int) {
		got[i], errs[i] = live.Get(ids[i])
	})
	for i, id := range ids {
		switch err := errs[i]; {
		case err == nil:
			if got[i].ID == id {
				out[id] = got[i]
			}
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrBdSQLClassRelocated):
		default:
			return fmt.Errorf("getting bead %q: %w", id, err)
		}
	}
	return nil
}
