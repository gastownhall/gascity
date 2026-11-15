package beads

import (
	"slices"
	"strings"
)

// bdListIDsChunkSize bounds how many ids one `bd list --id` read carries. bd
// answers an id IN-list in near-constant time up to about a thousand ids and
// super-linearly past that, and a single argv element dies of E2BIG between
// 10k and 15k ids; 500 keeps a read well inside both limits.
const bdListIDsChunkSize = 500

// bdListIDChunks turns a ListQuery's ids into the --id values of one or more
// bd list reads: blank ids dropped (bd reads a blank --id as "no filter" and
// answers with the whole ledger), duplicates dropped, first-seen order kept,
// and the rest split into chunks of at most bdListIDsChunkSize. No chunks means
// no id survived, which Matches answers with no rows, so no bd read is owed.
func bdListIDChunks(ids []string) [][]string {
	seen := make(map[string]struct{}, len(ids))
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if _, dup := seen[id]; id == "" || dup {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
	}
	return slices.Collect(slices.Chunk(clean, bdListIDsChunkSize))
}
