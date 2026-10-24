package nudgequeue

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func stateInode(t *testing.T, cityPath string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(StatePath(cityPath), &st); err != nil {
		t.Fatalf("stat state.json: %v", err)
	}
	return st.Ino
}

func seedState(t *testing.T, cityPath string, items ...Item) {
	t.Helper()
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, items...)
		SortState(state)
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// A file with no debts and no retired items is byte-identical to the format
// before debts existed: the new fields are omitted.
func TestStateWithoutDebtsOmitsTheNewFields(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	data, err := json.Marshal(State{Pending: []Item{{ID: "n-1", Agent: "a", CreatedAt: now}}, Dead: []Item{{ID: "n-2", DeadAt: now}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"shadow"`, `"retired"`} {
		if strings.Contains(string(data), key) {
			t.Fatalf("marshaled state %s carries %s", data, key)
		}
	}
}

// SortState orders retired items by the time of their debt, so the file stays
// deterministic.
func TestSortStateOrdersRetiredByDebtTime(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	state := State{Retired: []Item{
		{ID: "late", Shadow: &ShadowDebt{State: "injected", At: now.Add(time.Minute)}},
		{ID: "b", Shadow: &ShadowDebt{State: "injected", At: now}},
		{ID: "a", Shadow: &ShadowDebt{State: "injected", At: now}},
	}}
	SortState(&state)
	var ids []string
	for _, item := range state.Retired {
		ids = append(ids, item.ID)
	}
	if got := strings.Join(ids, ","); got != "a,b,late" {
		t.Fatalf("retired order = %s, want a,b,late", got)
	}
}

// A hold whose callback changes nothing does not rewrite state.json.
func TestWithStateSkipsTheRewriteWhenNothingChanged(t *testing.T) {
	cityPath := t.TempDir()
	seedState(t, cityPath, Item{ID: "n-1", Agent: "a"})
	before := stateInode(t, cityPath)
	if err := WithState(cityPath, func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if after := stateInode(t, cityPath); after != before {
		t.Fatalf("a no-op hold rewrote state.json (inode %d -> %d)", before, after)
	}
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "n-2", Agent: "a"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after := stateInode(t, cityPath); after == before {
		t.Fatal("a changing hold did not rewrite state.json")
	}
}

// UpdateIfChanged answers a callback that changes nothing from a lock-free
// snapshot: it takes no lock and writes nothing.
func TestUpdateIfChangedTakesNoLockForANoOp(t *testing.T) {
	cityPath := t.TempDir()
	seedState(t, cityPath, Item{ID: "n-1", Agent: "a"})
	before := stateInode(t, cityPath)
	release := holdQueueLockForTest(t, cityPath)
	defer release()

	calls := 0
	var seen []string
	start := time.Now()
	err := UpdateIfChangedWithin(cityPath, "test", time.Second, func(state *State) error {
		calls++
		seen = seen[:0]
		for _, item := range state.Pending {
			seen = append(seen, item.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateIfChanged: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("a no-op waited %s for the held lock", elapsed)
	}
	if calls != 1 || len(seen) != 1 || seen[0] != "n-1" {
		t.Fatalf("calls = %d, seen = %v; want one dry run over [n-1]", calls, seen)
	}
	if after := stateInode(t, cityPath); after != before {
		t.Fatal("a no-op rewrote state.json")
	}
}

// A callback that changes the state takes the lock, reruns on the state read
// under it, and persists that run's result.
func TestUpdateIfChangedLocksAndPersistsAChange(t *testing.T) {
	cityPath := t.TempDir()
	seedState(t, cityPath, Item{ID: "n-1", Agent: "a"})
	add := func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "n-2", Agent: "a"})
		return nil
	}

	release := holdQueueLockForTest(t, cityPath)
	err := UpdateIfChangedWithin(cityPath, "test", 50*time.Millisecond, add)
	release()
	if !errors.Is(err, ErrQueueBusy) {
		t.Fatalf("a change under a held lock: err = %v, want ErrQueueBusy", err)
	}

	runs := 0
	if err := UpdateIfChangedWithin(cityPath, "test", time.Second, func(state *State) error {
		runs++
		return add(state)
	}); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("runs = %d, want a dry run and a locked run", runs)
	}
	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 2 {
		t.Fatalf("pending = %+v, want n-1 and n-2", state.Pending)
	}
}

// A missing state file read by a no-op leaves the file absent.
func TestUpdateIfChangedNoOpOnAMissingFileCreatesNothing(t *testing.T) {
	cityPath := t.TempDir()
	if err := UpdateIfChanged(cityPath, func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(StatePath(cityPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat state.json = %v, want absent", err)
	}
}

// The lock observer sees every acquisition: its verb, a busy wait, and a hold.
func TestLockObserverReportsVerbOutcomeAndTimes(t *testing.T) {
	cityPath := t.TempDir()
	var samples []LockSample
	SetLockObserver(func(s LockSample) { samples = append(samples, s) })
	t.Cleanup(func() { SetLockObserver(nil) })

	if err := WithStateFor(cityPath, "seed", func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "n-1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	release := holdQueueLockForTest(t, cityPath)
	_ = withStateBounded(cityPath, "blocked", 30*time.Millisecond, func(*State) error { return nil })
	release()
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want two", samples)
	}
	if s := samples[0]; s.Verb != "seed" || s.Outcome != LockOutcomeOK {
		t.Fatalf("first sample = %+v", s)
	}
	if s := samples[1]; s.Verb != "blocked" || s.Outcome != LockOutcomeBusy || s.Wait < 30*time.Millisecond || s.Hold != 0 {
		t.Fatalf("busy sample = %+v", s)
	}
}

// A caller-chosen wait of zero tries the lock once: it takes a free lock, and
// gives up on a held one at once, without running its callback.
func TestWithStateWithinZeroWaitTriesTheLockOnce(t *testing.T) {
	cityPath := t.TempDir()
	seedState(t, cityPath, Item{ID: "n-1", Agent: "a"})
	add := func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "n-2", Agent: "a"})
		return nil
	}

	release := holdQueueLockForTest(t, cityPath)
	start := time.Now()
	ran := false
	err := WithStateWithin(cityPath, "test", 0, func(state *State) error {
		ran = true
		return add(state)
	})
	release()
	if !errors.Is(err, ErrQueueBusy) || ran {
		t.Fatalf("a zero wait on a held lock: err = %v, ran = %v; want ErrQueueBusy and no run", err, ran)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("a zero wait on a held lock took %s", elapsed)
	}

	if err := WithStateWithin(cityPath, "test", 0, add); err != nil {
		t.Fatalf("a zero wait on a free lock: %v", err)
	}
	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 2 {
		t.Fatalf("pending = %+v, want n-1 and n-2", state.Pending)
	}
	if LockWait() != defaultLockWaitTimeout {
		t.Fatalf("LockWait() = %s, want the default %s", LockWait(), defaultLockWaitTimeout)
	}
}
