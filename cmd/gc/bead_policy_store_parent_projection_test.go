package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type recordingParentProjectionStore struct {
	beads.Store
	ctx   context.Context
	args  [3]string
	calls int
	err   error
}

func (s *recordingParentProjectionStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	s.ctx = ctx
	s.args = [3]string{id, oldParentID, newParentID}
	s.calls++
	return s.err
}

type graphParentProjectionStore struct {
	*recordingParentProjectionStore
}

func (s *graphParentProjectionStore) ApplyGraphPlan(context.Context, *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	return nil, nil
}

func TestBeadPolicyParentProjectionFactoryPaths(t *testing.T) {
	for _, tc := range []struct {
		name      string
		graph     bool
		withCache bool
	}{
		{name: "plain"},
		{name: "graph", graph: true},
		{name: "cached", withCache: true},
		// The production chain: policy(CachingStore(graph-capable backend)).
		{name: "graph cached", graph: true, withCache: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := &recordingParentProjectionStore{Store: beads.NewMemStore()}
			var inner beads.Store = backing
			if tc.graph {
				inner = &graphParentProjectionStore{recordingParentProjectionStore: backing}
			}
			if tc.withCache {
				inner = beads.NewCachingStore(inner, nil)
			}
			wrapped := wrapStoreWithBeadPolicies(inner, &config.City{})
			if tc.graph {
				if _, ok := wrapped.(*beadPolicyGraphStore); !ok {
					t.Fatalf("factory returned %T, want *beadPolicyGraphStore", wrapped)
				}
			} else {
				if _, ok := wrapped.(*beadPolicyStore); !ok {
					t.Fatalf("factory returned %T, want *beadPolicyStore", wrapped)
				}
			}
			waiter, ok := wrapped.(beads.ParentProjectionWaiter)
			if !ok {
				t.Fatalf("policy-wrapped store %T hides ParentProjectionWaiter", wrapped)
			}
			ctx := context.WithValue(t.Context(), parentProjectionContextKey{}, tc.name)
			if err := waiter.WaitForParentProjection(ctx, "child", "old", "new"); err != nil {
				t.Fatalf("WaitForParentProjection: %v", err)
			}
			if backing.calls != 1 || backing.ctx != ctx || backing.args != [3]string{"child", "old", "new"} {
				t.Fatalf("forwarded call = (%d, %v, %v), want one call with original context and IDs", backing.calls, backing.ctx, backing.args)
			}
		})
	}
}

type parentProjectionContextKey struct{}

func TestBeadPolicyParentProjectionErrorsAndClearedParent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "generic", err: errors.New("projection unavailable")},
		{name: "superseded", err: errors.Join(errors.New("projection changed"), beads.ErrParentProjectionSuperseded)},
		{name: "context canceled", err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := &recordingParentProjectionStore{Store: beads.NewMemStore(), err: tc.err}
			wrapped := wrapStoreWithBeadPolicies(backing, nil)
			waiter, ok := wrapped.(beads.ParentProjectionWaiter)
			if !ok {
				t.Fatalf("policy-wrapped store %T hides ParentProjectionWaiter", wrapped)
			}
			err := waiter.WaitForParentProjection(t.Context(), "child", "old", "")
			if !errors.Is(err, tc.err) {
				t.Fatalf("forwarded error = %v, want %v", err, tc.err)
			}
			if tc.name == "superseded" && !errors.Is(err, beads.ErrParentProjectionSuperseded) {
				t.Fatalf("forwarded error = %v, want supersession identity", err)
			}
			if backing.args != [3]string{"child", "old", ""} {
				t.Fatalf("forwarded IDs = %v, want cleared parent", backing.args)
			}
		})
	}
}

func TestBeadPolicyParentProjectionWithoutInnerWaiter(t *testing.T) {
	waiter, ok := wrapStoreWithBeadPolicies(beads.NewMemStore(), nil).(beads.ParentProjectionWaiter)
	if !ok {
		t.Fatal("policy-wrapped store hides ParentProjectionWaiter")
	}
	if err := waiter.WaitForParentProjection(t.Context(), "child", "old", "new"); err != nil {
		t.Fatalf("WaitForParentProjection without an inner waiter: %v", err)
	}
}
