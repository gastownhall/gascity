package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestRigCloneOwnershipManifestPersistsAcrossRetryAndSweep(t *testing.T) {
	store := beads.NewMemStore()
	record, err := store.Create(beads.Bead{Title: "rig clone ownership"})
	if err != nil {
		t.Fatal(err)
	}
	before := RigProvisionManifest{RigName: "app", CreatedDir: "/external/project", CreatedDirIdentity: "17:531", CreatedDirIdentityRequired: true}
	if err := persistManifest(store, record.ID, before); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The rig name is normally persisted when the idempotency row is born.
	if err := store.SetMetadataBatch(record.ID, map[string]string{metaIdemRigName: "app"}); err != nil {
		t.Fatal(err)
	}
	if got.Metadata[metaIdemCreatedDirIdentity] != "17:531" {
		t.Fatalf("physical identity not durable: %+v", got.Metadata)
	}
	if err := persistManifest(store, record.ID, RigProvisionManifest{DoltDB: "owned_app"}); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	after := manifestFromRecord(&got)
	if after.CreatedDir != before.CreatedDir || after.CreatedDirIdentity != before.CreatedDirIdentity || after.DoltDB != "owned_app" || after.RigName != "app" || !after.CreatedDirIdentityRequired {
		t.Fatalf("retry/sweep manifest lost ownership: %+v", after)
	}
}

func TestRigCloneLegacyManifestDoesNotInventExternalOwnership(t *testing.T) {
	got := manifestFromRecord(&beads.Bead{Metadata: map[string]string{metaIdemRigName: "old", metaIdemCreatedDir: "/external/old-project"}})
	if got.CreatedDirIdentity != "" || got.CreatedDir != "/external/old-project" || got.CreatedDirIdentityRequired {
		t.Fatalf("legacy manifest invented ownership: %+v", got)
	}
}

func TestRigCloneRequiredIdentityIntentCannotBecomeLegacy(t *testing.T) {
	store := beads.NewMemStore()
	record, err := store.Create(beads.Bead{Title: "absolute intent"})
	if err != nil {
		t.Fatal(err)
	}
	first := RigProvisionManifest{CreatedDir: "/city/inside", CreatedDirIdentityRequired: true}
	if err := persistManifest(store, record.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := persistManifest(store, record.ID, RigProvisionManifest{CreatedDir: first.CreatedDir}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{got.Metadata[metaIdemCreatedDirIdentityRequired], "malformed-future-value"} {
		got.Metadata[metaIdemCreatedDirIdentityRequired] = marker
		m := manifestFromRecord(&got)
		if !m.CreatedDirIdentityRequired || m.CreatedDirIdentity != "" {
			t.Fatalf("incomplete absolute intent became legacy: marker=%q manifest=%+v", marker, m)
		}
	}
}
