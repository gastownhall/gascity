package beadmeta

import "testing"

// IsBoundWorkflowStep names the release-binding domain (city-8ym): a bead
// stamped with a foreign gc.root_bead_id is a workflow member no release
// path may detach, unless its kind names an executable unit of its own.
func TestIsBoundWorkflowStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		meta map[string]string
		want bool
	}{
		{"a compiler-minted work step under a foreign root", "gc-2", map[string]string{RootBeadIDMetadataKey: "gc-1"}, true},
		{"a typed work step under a foreign root", "gc-2", map[string]string{RootBeadIDMetadataKey: "gc-1", KindMetadataKey: "build"}, true},
		{"a spec sidecar under a foreign root", "gc-2", map[string]string{RootBeadIDMetadataKey: "gc-1", KindMetadataKey: KindSpec}, true},
		{"a control bead under a foreign root", "gc-2", map[string]string{RootBeadIDMetadataKey: "gc-1", KindMetadataKey: KindRetry}, true},
		{"the root itself is self-referential", "gc-1", map[string]string{RootBeadIDMetadataKey: "gc-1"}, false},
		{"no root stamp", "gc-2", map[string]string{KindMetadataKey: "build"}, false},
		{"a blank root stamp", "gc-2", map[string]string{RootBeadIDMetadataKey: "   "}, false},
		{"no metadata at all", "gc-2", nil, false},
		{"a dispatch attempt root", "gc-2", map[string]string{RootBeadIDMetadataKey: "gc-1", KindMetadataKey: KindTask}, false},
		{"a workflow root", "gc-2", map[string]string{RootBeadIDMetadataKey: "gc-1", KindMetadataKey: KindWorkflow}, false},
		{"a wisp root", "gc-2", map[string]string{RootBeadIDMetadataKey: "gc-1", KindMetadataKey: KindWisp}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBoundWorkflowStep(tc.id, tc.meta); got != tc.want {
				t.Fatalf("IsBoundWorkflowStep(%q, %v) = %v, want %v", tc.id, tc.meta, got, tc.want)
			}
		})
	}
}
