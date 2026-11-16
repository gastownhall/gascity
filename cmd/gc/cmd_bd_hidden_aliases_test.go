package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
)

// TestBdMutationWriteIDsAcceptsBdHiddenAliases: bd accepts these value flags
// without listing them in its help. The exact-id guard used to read each as
// an unknown flag and stop the write with "cannot safely verify bead IDs".
func TestBdMutationWriteIDsAcceptsBdHiddenAliases(t *testing.T) {
	for _, args := range [][]string{
		{"close", "bt-1", "--resolution", "done"},
		{"close", "bt-1", "-m", "done"},
		{"close", "bt-1", "--message", "done"},
		{"close", "bt-1", "--comment", "done"},
		{"update", "bt-1", "--body", "text"},
		{"update", "bt-1", "-m", "text"},
		{"update", "bt-1", "--message", "text"},
		{"update", "bt-1", "--description-file", "d.md"},
		{"update", "bt-1", "--acceptance-criteria", "x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			ids, ok, ambiguous := bdMutationWriteIDs(args)
			if !ok {
				t.Fatalf("%v not recognized as a write mutation", args)
			}
			if ambiguous {
				t.Fatalf("`gc bd %v` scans as ambiguous, so gc aborts without writing", args)
			}
			if len(ids) != 1 || ids[0] != "bt-1" {
				t.Fatalf("ids = %v, want [bt-1]: the alias's value must not be read as a bead id", ids)
			}
		})
	}
}

// TestBdCreateDescriptionFileConsumesNextToken: bd reads the token after
// --description-file as its value, whatever it looks like, so a --metadata
// there is a file name, not a metadata flag gc should check.
func TestBdCreateDescriptionFileConsumesNextToken(t *testing.T) {
	cfg := &config.City{}
	metadata := fmt.Sprintf(`{%q:"ghost/worker"}`, beadmeta.RoutedToMetadataKey)

	if _, refused := bdRigQualifiedMetadataRefusal(cfg, t.TempDir(), []string{"create", "t", "--metadata", metadata}); !refused {
		t.Fatal("fixture: a route to an unconfigured rig must be refused, or this test proves nothing")
	}
	args := []string{"create", "t", "--description-file", "--metadata", metadata}
	if msg, refused := bdRigQualifiedMetadataRefusal(cfg, t.TempDir(), args); refused {
		t.Fatalf("`gc bd %v` was refused (%q); --metadata is --description-file's value", args, msg)
	}
}
