//go:build linux

package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
	"golang.org/x/sys/unix"
)

func TestGraphInspectionDoesNotAdvertiseFencingForUnqualifiedExactFilesystem(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	writer := openGraphSource(t, graphDir)
	if _, err := writer.Create(beads.Bead{Title: "static filesystem qualification"}); err != nil {
		t.Fatalf("creating Graph source: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing Graph source: %v", err)
	}

	originalProbe := probeSQLiteOFD
	probeSQLiteOFD = func(*os.File) error { return unix.EINVAL }
	t.Cleanup(func() { probeSQLiteOFD = originalProbe })

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
		t.Fatal("Graph descriptor advertised fencing without exact-VFS qualification")
	}
}

func TestGraphInspectionDefersDescriptorWhenSourceChangesDuringVFSQualification(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	writer := openGraphSource(t, graphDir)
	if _, err := writer.Create(beads.Bead{Title: "static VFS qualification race"}); err != nil {
		t.Fatalf("creating Graph source: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing Graph source: %v", err)
	}
	marker := filepath.Join(root, graphMarkerFilename)

	originalProbe := probeSQLiteOFD
	probeSQLiteOFD = func(*os.File) error {
		if err := os.WriteFile(marker, []byte("changed during qualification\n"), 0o600); err != nil {
			t.Fatalf("changing Graph source during VFS qualification: %v", err)
		}
		return nil
	}
	t.Cleanup(func() { probeSQLiteOFD = originalProbe })

	inspection, err := InspectGraph(context.Background(), storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: ProviderID,
		Path:     root,
	})
	if err != nil {
		t.Fatalf("InspectGraph: %v", err)
	}
	if inspection.Descriptor != nil {
		t.Fatalf("InspectGraph descriptor = %#v, want deferred descriptor after source change", inspection.Descriptor)
	}
}

func TestGraphFenceRejectsUnqualifiedOFDLockFilesystem(t *testing.T) {
	root := t.TempDir()
	graphDir := filepath.Join(root, graphDirectoryName)
	writer := openGraphSource(t, graphDir)
	if _, err := writer.Create(beads.Bead{Title: "filesystem qualification"}); err != nil {
		t.Fatalf("creating Graph source: %v", err)
	}
	if err := writer.CloseStore(); err != nil {
		t.Fatalf("closing Graph source: %v", err)
	}
	inspection := inspectProcessGraph(t, root)

	originalProbe := probeSQLiteOFD
	t.Cleanup(func() { probeSQLiteOFD = originalProbe })
	for _, unsupported := range []error{unix.EINVAL, unix.ENOTSUP, unix.ENOSYS} {
		t.Run(unsupported.Error(), func(t *testing.T) {
			probeSQLiteOFD = func(*os.File) error { return unsupported }
			_, err := acquireTestGraphFenceForRoot(t, root, storebinding.FenceRequest{
				Target:             inspection.Target,
				ExpectedGeneration: 61,
				Components:         []storebinding.ComponentID{GraphComponentID},
				Role:               storebinding.FenceRoleSource,
			})
			if !errors.Is(err, ErrSQLiteFenceFilesystemUnqualified) {
				t.Fatalf("acquiring fence error = %v, want unqualified filesystem", err)
			}
			if !errors.Is(err, unsupported) {
				t.Fatalf("acquiring fence error = %v, want %v", err, unsupported)
			}
			var qualification *FenceFilesystemError
			if !errors.As(err, &qualification) {
				t.Fatalf("acquiring fence error = %T, want *FenceFilesystemError", err)
			}
		})
	}
}
