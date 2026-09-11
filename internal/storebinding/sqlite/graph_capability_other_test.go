//go:build !linux

package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestGraphDescriptorDoesNotAdvertiseUnavailableWriterFencing(t *testing.T) {
	root := t.TempDir()
	writer := openGraphSource(t, filepath.Join(root, graphDirectoryName))
	if _, err := writer.Create(beads.Bead{Title: "static graph source"}); err != nil {
		t.Fatalf("creating Graph source bead: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing Graph source writer: %v", err)
	}

	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	if inspection.Descriptor == nil {
		t.Fatal("InspectGraph returned no descriptor")
	}
	if inspection.Descriptor.Capabilities.WriterFencing {
		t.Fatal("non-Linux Graph descriptor advertised unavailable writer fencing")
	}
}
