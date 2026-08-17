package beadmeta

// JSONPath renders a bead-metadata key as a MySQL JSON path into the bd
// issues.metadata column, e.g. `$."gc.kind"`. The key is double-quoted inside
// the path because gc.* keys contain dots, which MySQL would otherwise treat
// as path separators. Direct-SQL readers build JSON_EXTRACT expressions from
// this helper so the key spelling stays anchored to the vocabulary constants.
func JSONPath(key string) string {
	return `$."` + key + `"`
}
