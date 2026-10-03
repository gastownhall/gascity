package beadmeta

import "strings"

// IsExpandedWorkflowRoot reports whether meta describes a graph workflow root
// whose steps were materialized as child beads: gc.kind is KindWorkflow AND
// WorkflowExpandedMetadataKey is "true". Such a root is a container; its child
// steps are the work.
//
// Both halves are required. Retry and ralph attempt roots carry the marker
// with gc.kind=task and are real work, so a marker-only rule would hide them;
// a root-only workflow root carries the kind without the marker and is itself
// the unit of work.
//
// The predicate has no assignee clause: whether an expanded root already held
// by a session is still relevant is each call site's decision.
func IsExpandedWorkflowRoot(meta map[string]string) bool {
	if strings.TrimSpace(meta[KindMetadataKey]) != KindWorkflow {
		return false
	}
	return strings.TrimSpace(meta[WorkflowExpandedMetadataKey]) == "true"
}
