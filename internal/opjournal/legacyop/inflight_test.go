package legacyop

import (
	"sync"
	"sync/atomic"
	"testing"
)

// Entries are released only by their own holder, by pointer identity; a held
// id answers a snapshot that can neither release nor accept for the holder.
func TestInflightReleaseByIdentity(t *testing.T) {
	f := NewInflight()
	e, ok := f.Reserve("op-1", "d1")
	if !ok || e.Digest != "d1" || e.Accepted() {
		t.Fatalf("Reserve = %+v, %t", e, ok)
	}
	snap, ok := f.Reserve("op-1", "d2")
	if ok || snap == e || snap.Digest != "d1" {
		t.Fatalf("second Reserve = %+v (holder %p), %t", snap, e, ok)
	}
	f.Accept("op-1", snap, "forged")
	f.Release("op-1", snap)
	if again, ok := f.Reserve("op-1", "d1"); ok || again.Accepted() {
		t.Fatalf("a snapshot released or accepted for the holder: %+v, %t", again, ok)
	}
	f.Accept("op-1", e, "answer")
	if held, _ := f.Reserve("op-1", "d1"); !held.Accepted() || held.Replay() != "answer" {
		t.Fatalf("holder's answer not visible: %+v", held)
	}
	f.Release("op-2", e)
	if f.Len() != 1 {
		t.Fatalf("Release of another id removed the hold (len %d)", f.Len())
	}
	f.Release("op-1", e)
	if f.Len() != 0 {
		t.Fatalf("len %d after the holder released", f.Len())
	}
	if _, ok := f.Reserve("op-1", "d3"); !ok {
		t.Fatal("released id not reservable")
	}
	var zero Inflight
	if _, ok := zero.Reserve("x", "d"); !ok {
		t.Fatal("zero registry refused a reserve")
	}
}

// Thirty-two goroutines reserving one id: exactly one holds it.
func TestInflightConcurrentReserve(t *testing.T) {
	f := NewInflight()
	var holders atomic.Int32
	var held *Entry
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e, ok := f.Reserve("op-1", "d"); ok {
				holders.Add(1)
				mu.Lock()
				held = e
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if holders.Load() != 1 || f.Len() != 1 {
		t.Fatalf("%d holders, len %d", holders.Load(), f.Len())
	}
	f.Release("op-1", held)
	if f.Len() != 0 {
		t.Fatalf("len %d after release", f.Len())
	}
}
