package eventexport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func TestMuxSource_YieldsAndPicksUpNewCity(t *testing.T) {
	var pmu sync.Mutex
	provs := map[string]events.Provider{"c1": events.NewFake()}
	providers := func() map[string]events.Provider {
		pmu.Lock()
		defer pmu.Unlock()
		out := make(map[string]events.Provider, len(provs))
		for k, v := range provs {
			out[k] = v
		}
		return out
	}

	// cursors() advances as the collector consumes, so resume moves forward.
	var cmu sync.Mutex
	consumed := map[string]uint64{}
	cursors := func() map[string]uint64 {
		cmu.Lock()
		defer cmu.Unlock()
		out := make(map[string]uint64, len(consumed))
		for k, v := range consumed {
			out[k] = v
		}
		return out
	}

	src := NewMuxSource(providers, cursors, 15*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gotMu sync.Mutex
	got := map[string][]uint64{}
	go func() {
		for {
			te, err := src.Next(ctx)
			if err != nil {
				return
			}
			gotMu.Lock()
			got[te.City] = append(got[te.City], te.Seq)
			gotMu.Unlock()
			cmu.Lock()
			if te.Seq > consumed[te.City] {
				consumed[te.City] = te.Seq
			}
			cmu.Unlock()
		}
	}()

	// c1 is present + empty at first build (floor 0): live records are delivered.
	time.Sleep(40 * time.Millisecond)
	f1 := provs["c1"].(*events.Fake)
	f1.Record(events.Event{Seq: 1, Type: "bead.closed", Ts: time.Now(), Actor: "a", Subject: "mc-1"})
	f1.Record(events.Event{Seq: 2, Type: "order.fired", Ts: time.Now(), Actor: "a", Subject: "sweep"})

	has := func(city string, seq uint64) bool {
		gotMu.Lock()
		defer gotMu.Unlock()
		for _, s := range got[city] {
			if s == seq {
				return true
			}
		}
		return false
	}
	waitFor(t, 2*time.Second, func() bool { return has("c1", 1) && has("c1", 2) })

	// add a second city after launch; it must be picked up on a rebuild.
	f2 := events.NewFake()
	pmu.Lock()
	provs["c2"] = f2
	pmu.Unlock()
	time.Sleep(40 * time.Millisecond) // let a rebuild floor c2 at 0
	f2.Record(events.Event{Seq: 1, Type: "bead.created", Ts: time.Now(), Actor: "b", Subject: "mc-9"})
	waitFor(t, 2*time.Second, func() bool { return has("c2", 1) })
}
