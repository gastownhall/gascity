//go:build linux

package sqlite

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestGraphInspectorFencedAdmissionRejectsSchemaDriftWithoutSourceMutation(t *testing.T) {
	for _, test := range graphSchemaDriftTestCases() {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			graphDir := prepareGraphSchemaFixture(t, root, true, false)
			mutateGraphSchemaFixture(t, graphDir, test.apply)
			path, err := GraphPath(root)
			if err != nil {
				t.Fatalf("GraphPath(): %v", err)
			}
			state, err := captureGraphSource(path)
			if err != nil {
				t.Fatalf("capturing drifted Graph source: %v", err)
			}
			target, err := newGraphTarget(path, state)
			if err != nil {
				t.Fatalf("creating drifted Graph target: %v", err)
			}
			inspector, err := NewGraphInspector(graphBindingSpec(root))
			if err != nil {
				t.Fatalf("NewGraphInspector(): %v", err)
			}
			fence, err := acquireTestGraphFence(t, inspector, storebinding.FenceRequest{
				Target:             target,
				ExpectedGeneration: 81,
				Components:         []storebinding.ComponentID{GraphComponentID},
				Role:               storebinding.FenceRoleSource,
			})
			if err != nil {
				t.Fatalf("acquiring drifted Graph fence: %v", err)
			}
			t.Cleanup(func() { _ = fence.Release(context.Background()) })
			before := snapshotGraphSource(t, graphDir)

			descriptor, err := inspector.InspectFenced(context.Background(), storebinding.FencedInspectionRequest{
				Target:             target,
				Fence:              fence,
				ExpectedGeneration: 81,
			})
			if err == nil || !strings.Contains(err.Error(), "unsupported sqlite schema") {
				t.Fatalf("GraphInspector.InspectFenced() error = %v, descriptor = %#v; want unsupported sqlite schema", err, descriptor)
			}
			if !reflect.DeepEqual(descriptor, storebinding.Descriptor{}) {
				t.Fatalf("GraphInspector.InspectFenced() descriptor = %#v, want zero descriptor on validation failure", descriptor)
			}
			if err := fence.Release(context.Background()); err != nil {
				t.Fatalf("releasing drifted Graph fence: %v", err)
			}

			after := snapshotGraphSource(t, graphDir)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected fenced Graph inspection mutated source:\n--- before ---\n%#v\n--- after ---\n%#v", before, after)
			}
		})
	}
}
