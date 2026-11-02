package main

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/testutil"
)

// A gc rig resume and a gc rig suspend of another rig that queue on the
// suspension fence while a controller holds it for a stop each read the state
// once the fence is theirs, so neither one's change is replaced by the state
// both would have read before the stop.
func TestRigCommandsQueuedOnTheFenceKeepBothChanges(t *testing.T) {
	cityPath := t.TempDir()
	siteToml := "workspace_name = \"test-city\"\n\n[[rig]]\nname = \"r1\"\npath = \"/some/r1\"\n\n[[rig]]\nname = \"r2\"\npath = \"/some/r2\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n\n[[rigs]]\nname = \"r1\"\n\n[[rigs]]\nname = \"r2\"\n", siteToml)
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "r1", &suspended); err != nil {
		t.Fatalf("seeding r1 suspended: %v", err)
	}
	release, err := suspensionstate.Fence(cityPath)
	if err != nil {
		t.Fatalf("Fence: %v", err)
	}
	var running sync.WaitGroup
	t.Cleanup(func() {
		release()
		running.Wait()
	})

	type command struct {
		name   string
		do     func(fsys.FS, string, string, io.Writer, io.Writer) int
		rig    string
		code   int
		stderr bytes.Buffer
	}
	commands := []*command{
		{name: "gc rig resume r1", do: doRigResume, rig: "r1"},
		{name: "gc rig suspend r2", do: doRigSuspend, rig: "r2"},
	}
	for _, c := range commands {
		running.Go(func() {
			var stdout bytes.Buffer
			c.code = c.do(fsys.OSFS{}, cityPath, c.rig, &stdout, &c.stderr)
		})
	}
	testutil.AwaitFlockWaiters(t, suspensionstate.FenceFile(cityPath), len(commands))
	release()
	finished := make(chan struct{})
	go func() {
		running.Wait()
		close(finished)
	}()
	awaitClose(t, finished, "the rig commands queued on the fence")
	for _, c := range commands {
		if c.code != 0 {
			t.Fatalf("%s = %d: %s", c.name, c.code, c.stderr.String())
		}
	}

	st, err := suspensionstate.Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if suspensionstate.IsRigSuspended(st, "r1") {
		t.Error("r1 is suspended again: the resume was replaced")
	}
	if !suspensionstate.IsRigSuspended(st, "r2") {
		t.Error("r2 is not suspended: the suspend was replaced")
	}
}
