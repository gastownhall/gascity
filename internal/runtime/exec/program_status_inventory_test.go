package exec

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

func inventoryStatusPack(t *testing.T, n int) opsPack {
	pk := newOpsPack(t, `"observe.v1","report-attachment","program-status"`)
	var sessions []string
	for i := range n {
		sessions = append(sessions, fmt.Sprintf(`{"name":"s%02d","state":"running","object":{"id":"b","created":"t"}}`, i))
	}
	pk.put(t, "listing", `{"complete":true,"sessions":[`+strings.Join(sessions, ",")+`]}`)
	pk.put(t, "inventory.body", `cat "$D/listing"`)
	pk.put(t, "program-status.body", `echo '{"state":"idle","incarnation":"run"}'`)
	return pk
}

func TestListRunningAndProcessScanSendNoStatusOp(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := inventoryStatusPack(t, 2)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			names, err := p.ListRunning("")
			if err != nil || len(names) != 2 {
				t.Fatalf("%v %v", names, err)
			}
			// Exercise the scanner's own listing path as well as ListRunning.
			var raw *Provider
			switch v := p.(type) {
			case *Provider:
				raw = v
			case *seamBackedProvider:
				raw = v.raw
			}
			if _, err := raw.listedSessionIDs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(pk.log(t), "OP program-status") {
				t.Fatal("ListRunning or the process scanner's listing polled status")
			}
			_, entries, err := p.(runtime.InventoryLister).ListInventory("")
			if err != nil || !entries["s00"].Program.Valid() {
				t.Fatalf("%+v %v", entries, err)
			}
		})
	}
}

func TestPollFailureLeavesListingAlone(t *testing.T) {
	for name, ctor := range opsConstructors {
		for _, body := range []string{`exit 66`, `echo malformed`, `exit 2`} {
			t.Run(name+body, func(t *testing.T) {
				pk := inventoryStatusPack(t, 1)
				pk.put(t, "program-status.body", body)
				p := ctor(pk.script)
				p.(interface {
					SetProgramStatusMode(runtime.ProgramStatusMode)
				}).SetProgramStatusMode(runtime.ProgramStatusShadow)
				names, entries, err := p.(runtime.InventoryLister).ListInventory("")
				if err != nil || len(names) != 1 || entries["s00"].Program.Validity != runtime.ProgramStatusUnread || !p.(runtime.ListingAttestation).ListRunningComplete() {
					t.Fatalf("%v %+v %v", names, entries, err)
				}
			})
		}
	}
}

func TestProgramStatusCacheDroppedOnlyOnCompleteAbsence(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := inventoryStatusPack(t, 1)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			ps := p.(runtime.ProgramStatusProvider)
			old, err := ps.ProgramStatus("s00")
			if err != nil {
				t.Fatal(err)
			}
			pk.put(t, "program-status.body", `echo '{"state":"working","incarnation":"run"}'`)
			pk.put(t, "listing", `{"complete":false,"sessions":[]}`)
			_, _ = p.ListRunning("")
			got, _ := ps.ProgramStatus("s00")
			if got.State != runtime.ProgramIdle {
				t.Error("partial listing dropped cache")
			}
			pk.put(t, "inventory.body", `exit 1`)
			_, _ = p.ListRunning("")
			got, _ = ps.ProgramStatus("s00")
			if got.State != runtime.ProgramIdle {
				t.Error("failed listing dropped cache")
			}
			pk.put(t, "inventory.body", `cat "$D/listing"`)
			pk.put(t, "listing", `{"complete":true,"sessions":[]}`)
			_, _ = p.ListRunning("")
			got, err = ps.ProgramStatus("s00")
			if err != nil || got.State != runtime.ProgramWorking || got.Nonce == old.Nonce {
				t.Errorf("absence failed reset: %+v %v", got, err)
			}
		})
	}
}

func TestPollSkipsDeadBoxes(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := inventoryStatusPack(t, 0)
			pk.put(t, "listing", `{"complete":true,"sessions":[{"name":"dead","state":"dead","object":{"id":"b","created":"t"}}]}`)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			_, _, err := p.(runtime.InventoryLister).ListInventory("")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(pk.log(t), "OP program-status") {
				t.Fatal("dead box polled")
			}
		})
	}
}

func TestProgramStatusFreshFailureReplacesSuccess(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := inventoryStatusPack(t, 1)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			ps := p.(runtime.ProgramStatusProvider)
			first, err := ps.ProgramStatus("s00")
			if err != nil || !first.Valid() {
				t.Fatalf("%+v %v", first, err)
			}
			pk.put(t, "program-status.body", `exit 1`)
			if _, _, err := p.(runtime.InventoryLister).ListInventory(""); err != nil {
				t.Fatal(err)
			}
			got, err := ps.ProgramStatus("s00")
			if err == nil || got.Valid() {
				t.Errorf("served old success after failure: %+v %v", got, err)
			}
			if n := strings.Count(pk.log(t), "OP program-status"); n != 2 {
				t.Errorf("cached failure issued %d total ops", n)
			}
		})
	}
}

func TestPollNeedsAnInventory(t *testing.T) {
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := newOpsPack(t, `"program-status"`)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			if _, _, err := p.(runtime.InventoryLister).ListInventory(""); err == nil {
				t.Error("inventory gate accepted a pack without observe.v1")
			}
			if strings.Contains(pk.log(t), "OP program-status") {
				t.Error("status polled without an inventory")
			}
		})
	}
}

func TestPollAbandonedReadsDoNotPileUp(t *testing.T) {
	oldPoll, oldRead := programStatusPollBound, programStatusReadTimeout
	programStatusPollBound, programStatusReadTimeout = 20*time.Millisecond, 15*time.Millisecond
	t.Cleanup(func() { programStatusPollBound, programStatusReadTimeout = oldPoll, oldRead })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			pk := inventoryStatusPack(t, 20)
			// A TERM that lands before the trap kills its script and frees the
			// slot, so more than 8 ops may start. Count scripts between HELD and
			// FREED instead: each holds a slot, so more than 8 at once means more
			// than 8 slot holders.
			pk.put(t, "program-status.body", `trap '' TERM; echo HELD >> "$D/log"; sleep 0.5; echo FREED >> "$D/log"; echo '{"state":"idle"}'`)
			p := ctor(pk.script)
			p.(interface {
				SetProgramStatusMode(runtime.ProgramStatusMode)
			}).SetProgramStatusMode(runtime.ProgramStatusOn)
			for range 3 {
				if _, _, err := p.(runtime.InventoryLister).ListInventory(""); err != nil {
					t.Fatal(err)
				}
			}
			// An abandoned op keeps its slot until its script exits, so
			// taking every slot ensures no script writes into the pack dir
			// after cleanup.
			var raw *Provider
			switch v := p.(type) {
			case *Provider:
				raw = v
			case *seamBackedProvider:
				raw = v.raw
			default:
				t.Fatalf("unexpected provider %T", p)
			}
			finished := time.After(testutil.ExecRaceTimeout)
			for range cap(raw.programs.slots) {
				select {
				case raw.programs.slots <- struct{}{}:
				case <-finished:
					t.Fatal("abandoned scripts never finished")
				}
			}
			held, most := 0, 0
			for _, line := range strings.Split(pk.log(t), "\n") {
				switch line {
				case "HELD":
					held++
					most = max(most, held)
				case "FREED":
					held--
				}
			}
			if most == 0 || most > 8 {
				t.Errorf("%d abandoned scripts ran at once; want 1 to 8", most)
			}
		})
	}
}

func TestPollBoundsAndRotation(t *testing.T) {
	workers := programStatusPollWorkers
	programStatusPollWorkers = 2
	t.Cleanup(func() { programStatusPollWorkers = workers })
	for name, ctor := range opsConstructors {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := ctor("unused")
				var raw *Provider
				switch v := p.(type) {
				case *Provider:
					raw = v
				case *seamBackedProvider:
					raw = v.raw
				}
				entries := map[string]runtime.InventoryEntry{}
				for i := range 20 {
					entries[fmt.Sprintf("s%02d", i)] = runtime.InventoryEntry{DeadKnown: true}
				}
				covered := map[string]bool{}
				// Two 100ms reads finish in each 150ms pass. Reads started in
				// the last 50ms must not lose priority merely because they began.
				for range 10 {
					ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
					start := time.Now()
					raw.pollProgramStatusRead(ctx, entries, func(ctx context.Context, _ string, _ time.Duration) (runtime.ProgramStatus, error) {
						select {
						case <-time.After(100 * time.Millisecond):
							return runtime.ProgramStatus{Validity: runtime.ProgramStatusValid, State: runtime.ProgramIdle}, nil
						case <-ctx.Done():
							return runtime.ProgramStatus{}, ctx.Err()
						}
					})
					cancel()
					if time.Since(start) > 150*time.Millisecond {
						t.Fatal("poll exceeded total bound")
					}
					for name, e := range entries {
						if e.Program.Valid() {
							covered[name] = true
						}
						e.Program = runtime.ProgramStatus{}
						entries[name] = e
					}
				}
				if len(covered) != 20 {
					t.Errorf("read %d/20 sessions in 10 passes; budget-abandoned names starved", len(covered))
				}
			})
		})
	}
}
