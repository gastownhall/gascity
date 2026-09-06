package beads

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestNativeStepDependenciesReadsOnlyCanonicalMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		stepID   string
		want     *[]string
	}{
		{name: "missing is unknown", stepID: "step-b"},
		{name: "known root", stepID: "step-root", metadata: map[string]string{beadmeta.NativeStepDependenciesMetadataKey: "[]"}, want: ptr([]string{})},
		{name: "canonical dependency list", stepID: "step-b", metadata: map[string]string{beadmeta.NativeStepDependenciesMetadataKey: `["step-a","step-c"]`}, want: ptr([]string{"step-a", "step-c"})},
		{name: "noncanonical ordering is unknown", stepID: "step-c", metadata: map[string]string{beadmeta.NativeStepDependenciesMetadataKey: `["step-b","step-a"]`}},
		{name: "self edge is unknown", stepID: "step-a", metadata: map[string]string{beadmeta.NativeStepDependenciesMetadataKey: `["step-a"]`}},
		{name: "malformed is unknown", stepID: "step-b", metadata: map[string]string{beadmeta.NativeStepDependenciesMetadataKey: `not-json`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NativeStepDependencies(tc.metadata, tc.stepID); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NativeStepDependencies() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func ptr(values []string) *[]string { return &values }
