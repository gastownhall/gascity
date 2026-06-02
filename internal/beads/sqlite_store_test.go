package beads

import (
	"runtime"
	"testing"
	"time"
)

func TestSQLiteStoreCreatesAndGets(t *testing.T) {
	s, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			c.CloseStore() //nolint:errcheck
		}
	}()

	b := Bead{Title: "hello world", Type: "task"}
	created, err := s.Create(b)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created bead has empty ID")
	}
	if created.Status != "open" {
		t.Fatalf("expected status=open, got %q", created.Status)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "hello world" {
		t.Fatalf("expected title %q, got %q", "hello world", got.Title)
	}
}

func TestSQLiteStoreReady(t *testing.T) {
	s, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			c.CloseStore() //nolint:errcheck
		}
	}()

	// Create an unblocked bead.
	free, err := s.Create(Bead{Title: "free task", Type: "task"})
	if err != nil {
		t.Fatalf("create free: %v", err)
	}

	// Create a blocker and a blocked bead (dependency wired via DepAdd).
	blocker, err := s.Create(Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	blocked, err := s.Create(Bead{Title: "blocked task", Type: "task"})
	if err != nil {
		t.Fatalf("create blocked: %v", err)
	}
	if err := s.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("dep add: %v", err)
	}

	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("ready: %v", err)
	}

	readyIDs := make(map[string]bool)
	for _, b := range ready {
		readyIDs[b.ID] = true
	}
	if !readyIDs[free.ID] {
		t.Errorf("free bead %q should be ready", free.ID)
	}
	if !readyIDs[blocker.ID] {
		t.Errorf("blocker %q should be ready", blocker.ID)
	}
	if readyIDs[blocked.ID] {
		t.Errorf("blocked bead %q should NOT be ready", blocked.ID)
	}
}

func TestSQLiteStoreCloseStore(t *testing.T) {
	settle := func() {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		runtime.GC()
	}

	settle()
	base := runtime.NumGoroutine()

	s, err := OpenSQLiteStore(t.TempDir(),
		WithSQLiteStoreRetention(4*time.Hour, 30*time.Second))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	closer, ok := s.(interface{ CloseStore() error })
	if !ok {
		t.Fatal("SQLiteStore does not implement CloseStore() error")
	}
	if err := closer.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	// Idempotent second call must not error.
	if err := closer.CloseStore(); err != nil {
		t.Fatalf("second CloseStore: %v", err)
	}

	settle()
	residual := runtime.NumGoroutine() - base
	if residual > 5 {
		t.Fatalf("CloseStore leaked goroutines: residual=%d after open+close (want <=5)", residual)
	}
}

// TestSQLiteStoreNoLeakOnDiscard is the goroutine-leak regression test ported
// from investigate/ga-qsvwe1-coordstore-leak @1ea16a7a3. Opening N stores with
// the retention sweeper enabled and calling CloseStore on each must keep the
// goroutine count at ~baseline. Without CloseStore the count would grow by
// >=1 goroutine per store per tick.
func TestSQLiteStoreNoLeakOnDiscard(t *testing.T) {
	const n = 25

	settle := func() {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		runtime.GC()
	}

	settle()
	base := runtime.NumGoroutine()

	for i := 0; i < n; i++ {
		s, err := OpenSQLiteStore(t.TempDir(),
			WithSQLiteStoreRetention(4*time.Hour, 30*time.Second))
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		closer, ok := s.(interface{ CloseStore() error })
		if !ok {
			t.Fatalf("SQLiteStore does not implement CloseStore() error")
		}
		if err := closer.CloseStore(); err != nil {
			t.Fatalf("CloseStore %d: %v", i, err)
		}
	}

	settle()
	residual := runtime.NumGoroutine() - base
	t.Logf("goroutines: base=%d after=%d residual=%d (opened+closed %d stores)",
		base, base+residual, residual, n)

	if residual > 5 {
		t.Fatalf("SQLiteStore CloseStore did not release resources: residual goroutines=%d after %d open+close cycles (want <=5)", residual, n)
	}
}
