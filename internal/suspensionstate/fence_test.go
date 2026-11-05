package suspensionstate

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A write to the suspension state waits for the fence: a controller that
// confirmed a scope is still suspended and is stopping its store holds it,
// so a resume cannot land between that confirmation and the stop.
func TestSaveWaitsForTheFence(t *testing.T) {
	cityDir := t.TempDir()
	release, err := Fence(cityDir)
	if err != nil {
		t.Fatalf("Fence: %v", err)
	}
	saved := make(chan error, 1)
	go func() {
		saved <- SetRigSuspended(fsys.OSFS{}, cityDir, "r1", boolPtr(false))
	}()
	select {
	case err := <-saved:
		release()
		t.Fatalf("Save completed while the fence was held (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-saved:
		if err != nil {
			t.Fatalf("Save after release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Save did not complete after the fence was released")
	}
	st, err := Load(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := ExplicitRig(st, "r1"); !ok || v {
		t.Fatalf("rig override = %v (set %v), want an explicit resume", v, ok)
	}
}

// The fence orders processes; an in-memory filesystem has no other process
// to order against, so a fake-backed Save neither takes nor waits for it.
func TestSaveOnAnInMemoryFSIgnoresTheFence(t *testing.T) {
	cityDir := t.TempDir()
	release, err := Fence(cityDir)
	if err != nil {
		t.Fatalf("Fence: %v", err)
	}
	defer release()
	saved := make(chan error, 1)
	go func() {
		saved <- Save(fsys.NewFake(), cityDir, State{})
	}()
	select {
	case err := <-saved:
		if err != nil {
			t.Fatalf("Save on a fake FS: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Save on a fake FS waited for the OS fence")
	}
}

// Releasing the fence twice is harmless, so a caller can both defer the
// release and release early.
func TestFenceReleaseIsIdempotent(t *testing.T) {
	cityDir := t.TempDir()
	release, err := Fence(cityDir)
	if err != nil {
		t.Fatalf("Fence: %v", err)
	}
	release()
	release()
	again, err := Fence(cityDir)
	if err != nil {
		t.Fatalf("Fence after release: %v", err)
	}
	again()
}
