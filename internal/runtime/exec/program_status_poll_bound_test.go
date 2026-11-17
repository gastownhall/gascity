//go:build unix

package exec

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestListInventoryHungStatusReturnsWithinPollBound(t *testing.T) {
	oldPoll := programStatusPollBound
	programStatusPollBound = 100 * time.Millisecond
	t.Cleanup(func() { programStatusPollBound = oldPoll })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := inventoryStatusPack(t, 3)
			returned := statusFIFO(t, pk, "inventory.returned")
			started, release := statusFIFO(t, pk, "started"), statusFIFO(t, pk, "release")
			// The timestamp precedes the inventory op's return, not a status op's
			// start. Delaying the poll itself therefore consumes this budget too.
			pk.put(t, "inventory.body", `printf A > "$D/inventory.returned"; cat "$D/listing"`)
			pk.put(t, "program-status.body", `printf A > "$D/started"; dd bs=1 count=1 < "$D/release" >/dev/null 2>&1; echo '{"state":"idle"}'`)
			p := ctor(pk.script)
			raw := statusRaw(t, p)
			raw.SetProgramStatusMode(runtime.ProgramStatusOn)
			type listing struct {
				names   []string
				entries map[string]runtime.InventoryEntry
				err     error
				at      time.Time
			}
			done := make(chan listing, 1)
			t0 := time.Now() // Before even invoking inventory, a stricter bound than its return.
			go func() {
				names, entries, err := p.(runtime.InventoryLister).ListInventory("")
				done <- listing{names, entries, err, time.Now()}
			}()
			statusSignal(t, returned)
			// One second of slack covers process startup and scheduling under -race.
			deadline := t0.Add(programStatusPollBound + time.Second)
			statusSignal(t, started)
			select {
			case got := <-done:
				if got.at.After(deadline) {
					t.Errorf("listing exceeded poll bound plus 1s slack: %s", got.at.Sub(t0))
				}
				if got.err != nil || len(got.names) != 3 || len(got.entries) != 3 {
					t.Fatalf("listing failed: %+v", got)
				}
				for name, e := range got.entries {
					if e.Program.Validity != runtime.ProgramStatusUnread {
						t.Errorf("hung entry %s: %+v", name, e.Program)
					}
				}
			case <-time.After(testutil.ExecRaceTimeout):
				t.Fatal("listing did not return")
			}
			// Release every possible reader and acquire every slot so no child
			// can write into the fixture directory during its removal.
			if _, err := release.Write([]byte{1, 1, 1}); err != nil {
				t.Fatal(err)
			}
			finished := time.After(testutil.ExecRaceTimeout)
			for range cap(raw.programs.slots) {
				select {
				case raw.programs.slots <- struct{}{}:
				case <-finished:
					t.Fatal("hung readers did not finish")
				}
			}
		})
	}
}
