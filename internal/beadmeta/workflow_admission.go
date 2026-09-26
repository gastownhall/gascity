package beadmeta

import "strings"

// IsExpandedWorkflow reports whether a root has compiled child work. Such a
// root retains its route for attribution and an existing owner's continuation,
// but does not admit a fresh worker. Root-only launches omit the expansion stamp.
func IsExpandedWorkflow(metadata map[string]string) bool {
	return strings.TrimSpace(metadata[KindMetadataKey]) == KindWorkflow &&
		strings.TrimSpace(metadata[WorkflowExpandedMetadataKey]) == "true"
}
