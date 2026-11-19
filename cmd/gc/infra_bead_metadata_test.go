package main

import (
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCloneBeadMetadataCopiesWithoutSharing(t *testing.T) {
	source := beads.StringMap{"kept": "a"}

	got := cloneBeadMetadata(source)
	got["added"] = "b"
	got["kept"] = "changed"

	if want := (beads.StringMap{"kept": "a"}); !maps.Equal(source, want) {
		t.Errorf("writing the clone changed the source: %v, want %v", source, want)
	}
}

func TestCloneBeadMetadataOfNilIsWritable(t *testing.T) {
	got := cloneBeadMetadata(nil)
	if got == nil {
		t.Fatal("cloneBeadMetadata(nil) = nil, want an empty writable map")
	}
	got["key"] = "value"

	if want := (beads.StringMap{"key": "value"}); !maps.Equal(got, want) {
		t.Errorf("clone = %v, want %v", got, want)
	}
}
