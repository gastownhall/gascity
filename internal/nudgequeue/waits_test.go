package nudgequeue

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type listQueryCaptureStore struct {
	beads.Store
	queries []beads.ListQuery
}

type nudgeExactLimitStore struct {
	beads.Store
}

type nudgeMarkFailStore struct {
	*beads.MemStore
}

type nudgeSelectiveMarkFailStore struct {
	*beads.MemStore
	failID string
}

type nudgeReenqueueStore struct {
	*beads.MemStore
	cityPath string
	nudgeID  string
	injected bool
}

type nudgeReenqueueBeforeTerminalStore struct {
	*beads.MemStore
	cityPath      string
	nudgeID       string
	replacementID string
	injected      bool
}

type queueLockDetectStore struct {
	*beads.MemStore
	cityPath string
}

func (s *listQueryCaptureStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return s.Store.List(query)
}

func (s nudgeExactLimitStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	return nudgeItems(query, NudgeLookupLimit), nil
}

func nudgeItems(query beads.ListQuery, count int) []beads.Bead {
	items := make([]beads.Bead, count)
	for i := range items {
		items[i] = beads.Bead{
			ID:     "nudge",
			Status: "open",
			Labels: []string{query.Label},
		}
	}
	return items
}

func (s nudgeMarkFailStore) SetMetadataBatch(string, map[string]string) error {
	return errors.New("mark terminal failed")
}

func (s nudgeSelectiveMarkFailStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.failID {
		return errors.New("mark terminal failed")
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *nudgeReenqueueStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if !s.injected {
		s.injected = true
		created, err := s.Create(beads.Bead{
			Title:  "replacement nudge",
			Labels: []string{"nudge:" + s.nudgeID},
		})
		if err != nil {
			return err
		}
		replacement := Item{
			ID:        s.nudgeID,
			BeadID:    created.ID,
			Agent:     "worker",
			Source:    "wait",
			Message:   "ready again",
			CreatedAt: time.Now().Add(time.Minute).UTC(),
			ExpiresAt: time.Now().Add(time.Hour).UTC(),
		}
		if err := WithState(s.cityPath, func(state *State) error {
			state.Pending = append(state.Pending, replacement)
			return nil
		}); err != nil {
			return err
		}
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *nudgeReenqueueBeforeTerminalStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == "nudge:"+s.nudgeID {
		if err := s.injectReplacement(); err != nil {
			return nil, err
		}
	}
	return s.MemStore.List(query)
}

func (s *nudgeReenqueueBeforeTerminalStore) Get(id string) (beads.Bead, error) {
	if err := s.injectReplacement(); err != nil {
		return beads.Bead{}, err
	}
	return s.MemStore.Get(id)
}

func (s *nudgeReenqueueBeforeTerminalStore) injectReplacement() error {
	if s.injected {
		return nil
	}
	s.injected = true
	created, err := s.Create(beads.Bead{
		Title:  "replacement nudge",
		Labels: []string{"nudge:" + s.nudgeID},
	})
	if err != nil {
		return err
	}
	s.replacementID = created.ID
	replacement := Item{
		ID:        s.nudgeID,
		BeadID:    created.ID,
		Agent:     "worker",
		Source:    "wait",
		Message:   "ready again",
		CreatedAt: time.Now().Add(time.Minute).UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
	return WithState(s.cityPath, func(state *State) error {
		state.Pending = append(state.Pending, replacement)
		return nil
	})
}

func (s queueLockDetectStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if err := s.requireQueueLockAvailable(); err != nil {
		return nil, err
	}
	return s.MemStore.List(query)
}

func (s queueLockDetectStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.requireQueueLockAvailable(); err != nil {
		return err
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s queueLockDetectStore) Close(id string) error {
	if err := s.requireQueueLockAvailable(); err != nil {
		return err
	}
	return s.MemStore.Close(id)
}

func (s queueLockDetectStore) requireQueueLockAvailable() error {
	lockFile, err := os.OpenFile(LockPath(s.cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("bead store work ran while nudge queue lock was held")
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return nil
}

func TestMarkTerminalUsesBoundedNudgeLookup(t *testing.T) {
	mem := beads.NewMemStore()
	nudge, err := mem.Create(beads.Bead{
		Title:  "nudge",
		Labels: []string{"nudge:nudge-123"},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	store := &listQueryCaptureStore{Store: mem}

	if err := markTerminal(store, "nudge-123", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("markTerminal: %v", err)
	}

	if len(store.queries) != 1 {
		t.Fatalf("List calls = %d, want 1", len(store.queries))
	}
	if got := store.queries[0].Limit; got != NudgeLookupLimit+1 {
		t.Fatalf("List limit = %d, want %d", got, NudgeLookupLimit+1)
	}
	if got := store.queries[0].Sort; got != beads.SortCreatedDesc {
		t.Fatalf("List sort = %q, want %q", got, beads.SortCreatedDesc)
	}
	updated, err := mem.Get(nudge.ID)
	if err != nil {
		t.Fatalf("Get(nudge): %v", err)
	}
	if updated.Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", updated.Status)
	}
}

func TestTerminalNudgeBeads_AllowsExactLookupLimit(t *testing.T) {
	items, err := terminalNudgeBeads(nudgeExactLimitStore{Store: beads.NewMemStore()}, "nudge-123")
	if err != nil {
		t.Fatalf("terminalNudgeBeads: %v", err)
	}
	if len(items) != NudgeLookupLimit {
		t.Fatalf("nudge count = %d, want %d", len(items), NudgeLookupLimit)
	}
}

func TestMarkTerminalTerminalizesVisibleOpenNudgeWhenLookupCaps(t *testing.T) {
	mem := beads.NewMemStore()
	var visible []beads.Bead
	for i := 0; i < NudgeLookupLimit+1; i++ {
		item, err := mem.Create(beads.Bead{
			Title:  "open nudge",
			Labels: []string{"nudge:nudge-123"},
		})
		if err != nil {
			t.Fatalf("create open nudge %d: %v", i, err)
		}
		visible = append(visible, item)
	}

	if err := markTerminal(mem, "nudge-123", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("markTerminal: %v", err)
	}

	for _, item := range visible {
		updated, err := mem.Get(item.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", item.ID, err)
		}
		if updated.Status != "closed" {
			t.Fatalf("nudge %s status = %q, want closed", item.ID, updated.Status)
		}
		if got := updated.Metadata["terminal_reason"]; got != "wait-canceled" {
			t.Fatalf("nudge %s terminal_reason = %q, want wait-canceled", item.ID, got)
		}
	}
}

func TestWithdrawWaitNudges_TerminalizesOutsideQueueLock(t *testing.T) {
	cityPath := t.TempDir()
	item := Item{
		ID:        "nudge-123",
		Agent:     "worker",
		Source:    "wait",
		Message:   "ready",
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, item)
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}
	mem := beads.NewMemStore()
	nudge, err := mem.Create(beads.Bead{
		Title:  "nudge",
		Labels: []string{"nudge:" + item.ID},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	store := queueLockDetectStore{MemStore: mem, cityPath: cityPath}

	if err := WithdrawWaitNudges(store, cityPath, []string{item.ID}); err != nil {
		t.Fatalf("WithdrawWaitNudges: %v", err)
	}
	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending queue = %#v, want empty", state.Pending)
	}
	updated, err := mem.Get(nudge.ID)
	if err != nil {
		t.Fatalf("Get(nudge): %v", err)
	}
	if updated.Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", updated.Status)
	}
}

func TestWithdrawWaitNudges_LeavesQueueIntactOnMarkTerminalFailure(t *testing.T) {
	cityPath := t.TempDir()
	item := Item{
		ID:        "nudge-123",
		Agent:     "worker",
		Source:    "wait",
		Message:   "ready",
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, item)
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{
		Title:  "nudge",
		Labels: []string{"nudge:" + item.ID},
	}); err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}

	err := WithdrawWaitNudges(nudgeMarkFailStore{MemStore: mem}, cityPath, []string{item.ID})
	if err == nil || !strings.Contains(err.Error(), "mark terminal failed") {
		t.Fatalf("WithdrawWaitNudges error = %v, want mark terminal failed", err)
	}

	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != item.ID {
		t.Fatalf("pending queue = %#v, want original item", state.Pending)
	}
}

func TestWithdrawWaitNudges_RemovesTerminalizedSiblingsOnLaterFailure(t *testing.T) {
	cityPath := t.TempDir()
	now := time.Now().UTC()
	goodItem := Item{
		ID:        "nudge-good",
		Agent:     "worker",
		Source:    "wait",
		Message:   "ready",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	badItem := Item{
		ID:        "nudge-bad",
		Agent:     "worker",
		Source:    "wait",
		Message:   "ready",
		CreatedAt: now.Add(time.Second),
		ExpiresAt: now.Add(time.Hour),
	}
	mem := beads.NewMemStore()
	goodNudge, err := mem.Create(beads.Bead{
		Title:  "good nudge",
		Labels: []string{"nudge:" + goodItem.ID},
	})
	if err != nil {
		t.Fatalf("create good nudge bead: %v", err)
	}
	badNudge, err := mem.Create(beads.Bead{
		Title:  "bad nudge",
		Labels: []string{"nudge:" + badItem.ID},
	})
	if err != nil {
		t.Fatalf("create bad nudge bead: %v", err)
	}
	goodItem.BeadID = goodNudge.ID
	badItem.BeadID = badNudge.ID
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, goodItem, badItem)
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}

	err = WithdrawWaitNudges(nudgeSelectiveMarkFailStore{MemStore: mem, failID: badNudge.ID}, cityPath, []string{goodItem.ID, badItem.ID})
	if err == nil || !strings.Contains(err.Error(), "mark terminal failed") {
		t.Fatalf("WithdrawWaitNudges error = %v, want mark terminal failed", err)
	}

	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != badItem.ID {
		t.Fatalf("pending queue = %#v, want only failed item %q", state.Pending, badItem.ID)
	}
	updatedGood, err := mem.Get(goodNudge.ID)
	if err != nil {
		t.Fatalf("Get(good): %v", err)
	}
	if updatedGood.Status != "closed" {
		t.Fatalf("good nudge status = %q, want closed", updatedGood.Status)
	}
}

func TestWithdrawWaitNudges_KeepsReenqueuedSameIDItem(t *testing.T) {
	cityPath := t.TempDir()
	now := time.Now().UTC()
	item := Item{
		ID:        "nudge-123",
		Agent:     "worker",
		Source:    "wait",
		Message:   "ready",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	mem := beads.NewMemStore()
	nudge, err := mem.Create(beads.Bead{
		Title:  "nudge",
		Labels: []string{"nudge:" + item.ID},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	item.BeadID = nudge.ID
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, item)
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}
	store := &nudgeReenqueueStore{
		MemStore: mem,
		cityPath: cityPath,
		nudgeID:  item.ID,
	}

	if err := WithdrawWaitNudges(store, cityPath, []string{item.ID}); err != nil {
		t.Fatalf("WithdrawWaitNudges: %v", err)
	}

	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending queue = %#v, want replacement item", state.Pending)
	}
	if state.Pending[0].ID != item.ID {
		t.Fatalf("replacement ID = %q, want %q", state.Pending[0].ID, item.ID)
	}
	if state.Pending[0].BeadID == item.BeadID {
		t.Fatalf("replacement bead ID = original %q, want new bead", item.BeadID)
	}
}

func TestWithdrawWaitNudges_KeepsSameIDReenqueueBeforeTerminalLookup(t *testing.T) {
	cityPath := t.TempDir()
	now := time.Now().UTC()
	item := Item{
		ID:        "nudge-123",
		Agent:     "worker",
		Source:    "wait",
		Message:   "ready",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	mem := beads.NewMemStore()
	nudge, err := mem.Create(beads.Bead{
		Title:  "nudge",
		Labels: []string{"nudge:" + item.ID},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	item.BeadID = nudge.ID
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, item)
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}
	store := &nudgeReenqueueBeforeTerminalStore{
		MemStore: mem,
		cityPath: cityPath,
		nudgeID:  item.ID,
	}

	if err := WithdrawWaitNudges(store, cityPath, []string{item.ID}); err != nil {
		t.Fatalf("WithdrawWaitNudges: %v", err)
	}

	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending queue = %#v, want replacement item", state.Pending)
	}
	if state.Pending[0].BeadID != store.replacementID {
		t.Fatalf("replacement bead ID = %q, want %q", state.Pending[0].BeadID, store.replacementID)
	}
	original, err := mem.Get(nudge.ID)
	if err != nil {
		t.Fatalf("Get(original): %v", err)
	}
	if original.Status != "closed" {
		t.Fatalf("original status = %q, want closed", original.Status)
	}
	replacement, err := mem.Get(store.replacementID)
	if err != nil {
		t.Fatalf("Get(replacement): %v", err)
	}
	if replacement.Status != "open" {
		t.Fatalf("replacement status = %q, want open", replacement.Status)
	}
}
