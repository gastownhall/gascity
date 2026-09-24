package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// fakeBdProjection is a scripted bd that models the tier and status rules the
// parent-projection wait depends on: `bd show` and `bd list` see only the
// issues tier, `bd query ephemeral=true` sees only wisps, and both omit closed
// rows unless passed --all. Its `bd show` is the legacy one that misses wisps
// (newer bd returns them directly; see
// TestBdStoreGetUsesDirectShowForEphemeralRows), so it exercises the harder
// wisp-fallback path of Get. It models projection lag by serving the child
// under its old parent for staleReads listings of that parent.
type fakeBdProjection struct {
	mu           sync.Mutex
	rows         []Bead
	staleParent  string
	staleReads   int
	wispQueryErr error
	showGate     chan struct{}
	shows        int
	listed       []string
}

func (f *fakeBdProjection) run(_, _ string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "show" && f.showGate != nil {
		<-f.showGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	all := containsArg(args, "--all")
	switch {
	case len(args) == 3 && args[0] == "show":
		f.shows++
		for _, row := range f.rows {
			if row.ID == args[2] && !row.Ephemeral {
				return encodeFakeBdRows([]Bead{row}), nil
			}
		}
		return nil, fmt.Errorf("issue %s not found", args[2])
	case len(args) > 2 && args[0] == "query" && strings.HasPrefix(args[2], "ephemeral=true AND id="):
		if f.wispQueryErr != nil {
			return nil, f.wispQueryErr
		}
		return encodeFakeBdRows(f.children("", strings.TrimPrefix(args[2], "ephemeral=true AND id="), true, all)), nil
	case len(args) > 2 && args[0] == "query" && strings.HasPrefix(args[2], "ephemeral=true AND parent="):
		return encodeFakeBdRows(f.children(strings.TrimPrefix(args[2], "ephemeral=true AND parent="), "", true, all)), nil
	case len(args) > 0 && args[0] == "list":
		for i := range args[:len(args)-1] {
			if args[i] == "--parent" {
				return encodeFakeBdRows(f.children(args[i+1], "", false, all)), nil
			}
		}
	}
	return nil, fmt.Errorf("unexpected bd command: %s", strings.Join(args, " "))
}

// children returns rows in one tier under parent (or with id), applying the
// status filter and the scripted lag. Callers hold f.mu.
func (f *fakeBdProjection) children(parent, id string, ephemeral, includeClosed bool) []Bead {
	if parent != "" {
		f.listed = append(f.listed, parent)
	}
	stale := parent != "" && parent == f.staleParent && f.staleReads > 0
	servedStale := false
	var out []Bead
	for _, row := range f.rows {
		if row.Ephemeral != ephemeral || (row.Status == "closed" && !includeClosed) {
			continue
		}
		switch {
		case id != "" && row.ID == id, parent != "" && row.ParentID == parent:
			out = append(out, row)
		case stale && row.ID == "bd-child":
			// A lagging projection still files the child under the old parent.
			staleRow := row
			staleRow.ParentID = parent
			out = append(out, staleRow)
			servedStale = true
		}
	}
	if servedStale {
		f.staleReads--
	}
	return out
}

func encodeFakeBdRows(rows []Bead) []byte {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"title":"t","status":%q,"issue_type":"task","parent":%q,"ephemeral":%t}`,
			row.ID, row.Status, row.ParentID, row.Ephemeral))
	}
	return []byte("[" + strings.Join(parts, ",") + "]")
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// The projection tests run under testing/synctest: every poll runs on its own
// goroutine, so their deadlines use the bubble's simulated clock rather than
// racing the wall clock.

func TestBdStoreParentProjectionIncludesEphemeralChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bd := &fakeBdProjection{
			rows:        []Bead{{ID: "bd-child", Status: "open", ParentID: "bd-new", Ephemeral: true}},
			staleParent: "bd-old",
			staleReads:  1,
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if err := NewBdStore("/city", bd.run).WaitForParentProjection(ctx, "bd-child", "bd-old", "bd-new"); err != nil {
			t.Fatalf("WaitForParentProjection of ephemeral child: %v", err)
		}
		if bd.staleReads != 0 {
			t.Fatalf("stale old-parent reads left = %d; the wait returned before the old parent's wisp listing converged", bd.staleReads)
		}
		// The first poll must have seen the stale listing and polled again.
		if bd.shows < 2 {
			t.Fatalf("waiter polled %d time(s), want a second poll after the stale old-parent listing", bd.shows)
		}
	})
}

func TestBdStoreParentProjectionIncludesClosedChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bd := &fakeBdProjection{rows: []Bead{{ID: "bd-child", Status: "closed", ParentID: "bd-new"}}}
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		if err := NewBdStore("/city", bd.run).WaitForParentProjection(ctx, "bd-child", "bd-old", "bd-new"); err != nil {
			t.Fatalf("WaitForParentProjection of closed child: %v", err)
		}
	})
}

func TestBdStoreParentProjectionStopsWhenChildDeleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bd := &fakeBdProjection{}
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		err := NewBdStore("/city", bd.run).WaitForParentProjection(ctx, "bd-child", "bd-old", "bd-new")
		if !errors.Is(err, ErrNotFound) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitForParentProjection after delete = %v, want immediate ErrNotFound", err)
		}
	})
}

// A wisp-tier read failure proves nothing about absence, so the wait must keep
// polling rather than report the child as deleted.
func TestBdStoreParentProjectionWispReadFailureIsNotADelete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bd := &fakeBdProjection{wispQueryErr: errors.New("dial tcp 127.0.0.1:3307: connection refused")}
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		err := NewBdStore("/city", bd.run).WaitForParentProjection(ctx, "bd-child", "bd-old", "bd-new")
		if errors.Is(err, ErrNotFound) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitForParentProjection with failing wisp read = %v, want deadline carrying the read error, not ErrNotFound", err)
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("WaitForParentProjection error = %v, want the wisp read failure as the last check error", err)
		}
	})
}

func TestBdStoreParentProjectionHonorsDeadlineDuringStalledRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bd := &fakeBdProjection{
			rows:     []Bead{{ID: "bd-child", Status: "open", ParentID: "bd-new"}},
			showGate: make(chan struct{}),
		}
		defer close(bd.showGate)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			result <- NewBdStore("/city", bd.run).WaitForParentProjection(ctx, "bd-child", "bd-old", "bd-new")
		}()
		select {
		case err := <-result:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("WaitForParentProjection during stalled bd show = %v, want context.DeadlineExceeded", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("WaitForParentProjection is still blocked in bd show after its 100ms deadline")
		}
	})
}

// A caller that gives up between the two listings must not pay for the second.
func TestBdStoreParentProjectionMatchesStopsBetweenListings(t *testing.T) {
	bd := &fakeBdProjection{rows: []Bead{{ID: "bd-child", Status: "open", ParentID: "bd-new"}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewBdStore("/city", bd.run).parentProjectionMatches(ctx, "bd-child", "bd-old", "bd-new", false)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errParentProjectionCutShort) {
		t.Fatalf("parentProjectionMatches with canceled ctx = %v, want context.Canceled marked as cut short", err)
	}
	if slices.Contains(bd.listed, "bd-new") {
		t.Fatalf("listed parents %v, want no new-parent listing after cancellation", bd.listed)
	}
}
