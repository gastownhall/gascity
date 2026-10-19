package convoy

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestApplyConvoyFields(t *testing.T) {
	b := beads.Bead{}
	fields := ConvoyFields{
		Owner:  "mayor",
		Notify: "human",
		Merge:  "direct",
		Target: "main",
	}
	ApplyConvoyFields(&b, fields)

	if b.Metadata["convoy.owner"] != "mayor" {
		t.Errorf("owner = %q, want %q", b.Metadata["convoy.owner"], "mayor")
	}
	if b.Metadata["convoy.notify"] != "human" {
		t.Errorf("notify = %q, want %q", b.Metadata["convoy.notify"], "human")
	}
	if b.Metadata["convoy.merge"] != "direct" {
		t.Errorf("merge = %q, want %q", b.Metadata["convoy.merge"], "direct")
	}
	if b.Metadata["target"] != "main" {
		t.Errorf("target = %q, want %q", b.Metadata["target"], "main")
	}
}

func TestApplyConvoyFieldsSkipsEmpty(t *testing.T) {
	b := beads.Bead{}
	fields := ConvoyFields{Owner: "mayor"}
	ApplyConvoyFields(&b, fields)

	if _, ok := b.Metadata["convoy.notify"]; ok {
		t.Error("empty notify should not be set in metadata")
	}
	if b.Metadata["convoy.owner"] != "mayor" {
		t.Errorf("owner = %q, want %q", b.Metadata["convoy.owner"], "mayor")
	}
}

func TestGetConvoyFields(t *testing.T) {
	b := beads.Bead{
		Metadata: map[string]string{
			"convoy.owner":  "mayor",
			"convoy.notify": "human",
			"convoy.merge":  "mr",
			"target":        "develop",
		},
	}
	fields := GetConvoyFields(b)

	if fields.Owner != "mayor" {
		t.Errorf("Owner = %q, want %q", fields.Owner, "mayor")
	}
	if fields.Notify != "human" {
		t.Errorf("Notify = %q, want %q", fields.Notify, "human")
	}
	if fields.Merge != "mr" {
		t.Errorf("Merge = %q, want %q", fields.Merge, "mr")
	}
	if fields.Target != "develop" {
		t.Errorf("Target = %q, want %q", fields.Target, "develop")
	}
}

func TestGetConvoyFieldsNilMetadata(t *testing.T) {
	b := beads.Bead{}
	fields := GetConvoyFields(b)
	if fields.Owner != "" || fields.Notify != "" || fields.Merge != "" || fields.Target != "" {
		t.Error("expected empty fields for nil metadata")
	}
}

func TestSetConvoyFields(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "test convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}

	fields := ConvoyFields{Owner: "mayor", Target: "main"}
	if err := SetConvoyFields(store, b.ID, fields); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["convoy.owner"] != "mayor" {
		t.Errorf("owner = %q, want %q", got.Metadata["convoy.owner"], "mayor")
	}
	if got.Metadata["target"] != "main" {
		t.Errorf("target = %q, want %q", got.Metadata["target"], "main")
	}
}

// TestIsSynthetic pins which convoys count as system-minted glue: exactly the
// ones stamped gc.synthetic=true, the stamp both minters (graph.v2's
// single-item input convoy and the drain-unit convoy) write.
func TestIsSynthetic(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
		want     bool
	}{
		{name: "nil metadata", metadata: nil, want: false},
		{name: "unstamped", metadata: map[string]string{"target": "main"}, want: false},
		{name: "stamped true", metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"}, want: true},
		{name: "stamped true with padding", metadata: map[string]string{beadmeta.SyntheticMetadataKey: " true "}, want: true},
		{name: "stamped false", metadata: map[string]string{beadmeta.SyntheticMetadataKey: "false"}, want: false},
		{name: "stamped empty", metadata: map[string]string{beadmeta.SyntheticMetadataKey: ""}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{Type: "convoy", Metadata: tc.metadata}
			if got := IsSynthetic(b); got != tc.want {
				t.Errorf("IsSynthetic(%v) = %v, want %v", tc.metadata, got, tc.want)
			}
		})
	}
}
