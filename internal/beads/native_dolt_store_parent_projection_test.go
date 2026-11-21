package beads

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// These tests run under testing/synctest so their deadlines use the bubble's
// simulated clock rather than racing the wall clock.

func TestNativeDoltStoreParentProjectionIncludesEphemeralChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		result, err := store.ApplyGraphPlanWithStorage(t.Context(), &GraphApplyPlan{
			CommitMessage: "create ephemeral reparent fixture",
			Nodes: []GraphApplyNode{
				{Key: "old", Title: "Old parent"},
				{Key: "new", Title: "New parent"},
				{Key: "child", Title: "Child", ParentKey: "old"},
			},
		}, StorageEphemeral)
		if err != nil {
			t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
		}
		childID, oldParentID, newParentID := result.IDs["child"], result.IDs["old"], result.IDs["new"]
		child, err := store.Get(childID)
		if err != nil {
			t.Fatalf("Get child: %v", err)
		}
		if !child.Ephemeral || child.ParentID != oldParentID {
			t.Fatalf("created child = %+v, want ephemeral under old parent", child)
		}
		if err := store.Update(childID, UpdateOpts{ParentID: &newParentID}); err != nil {
			t.Fatalf("Update parent: %v", err)
		}
		newChildren, err := store.Children(newParentID, WithBothTiers)
		if err != nil {
			t.Fatalf("Children with both tiers: %v", err)
		}
		if !beadSliceContains(newChildren, childID) {
			t.Fatalf("new parent's both-tier children = %v, want ephemeral child %q", newChildren, childID)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if err := store.WaitForParentProjection(ctx, childID, oldParentID, newParentID); err != nil {
			t.Fatalf("WaitForParentProjection: %v", err)
		}
	})
}

func TestNativeDoltStoreParentProjectionIncludesClosedChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		result, err := store.ApplyGraphPlan(t.Context(), &GraphApplyPlan{
			CommitMessage: "create closed reparent fixture",
			Nodes: []GraphApplyNode{
				{Key: "old", Title: "Old parent"},
				{Key: "new", Title: "New parent"},
				{Key: "child", Title: "Child", ParentKey: "old"},
			},
		})
		if err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		childID, oldParentID, newParentID := result.IDs["child"], result.IDs["old"], result.IDs["new"]
		if err := store.Update(childID, UpdateOpts{ParentID: &newParentID}); err != nil {
			t.Fatalf("Update parent: %v", err)
		}
		if err := store.Close(childID); err != nil {
			t.Fatalf("Close child: %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		if err := store.WaitForParentProjection(ctx, childID, oldParentID, newParentID); err != nil {
			t.Fatalf("WaitForParentProjection of closed child: %v", err)
		}
	})
}

func TestNativeDoltStoreParentProjectionStopsWhenChildDeleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		err := store.WaitForParentProjection(ctx, "missing-child", "old", "new")
		if !errors.Is(err, ErrNotFound) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitForParentProjection after delete = %v, want immediate ErrNotFound", err)
		}
	})
}

// A caller that gives up between the two listings must not pay for the second.
func TestNativeDoltStoreParentProjectionMatchesStopsBetweenListings(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	result, err := store.ApplyGraphPlan(t.Context(), &GraphApplyPlan{
		CommitMessage: "create reparent fixture",
		Nodes: []GraphApplyNode{
			{Key: "old", Title: "Old parent"},
			{Key: "new", Title: "New parent"},
			{Key: "child", Title: "Child", ParentKey: "old"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	childID, oldParentID, newParentID := result.IDs["child"], result.IDs["old"], result.IDs["new"]
	if err := store.Update(childID, UpdateOpts{ParentID: &newParentID}); err != nil {
		t.Fatalf("Update parent: %v", err)
	}
	// The old parent no longer lists the child, so only the ctx check stands
	// between the first listing and the second.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.parentProjectionMatches(ctx, childID, oldParentID, newParentID, false); !errors.Is(err, context.Canceled) || !errors.Is(err, errParentProjectionCutShort) {
		t.Fatalf("parentProjectionMatches with canceled ctx = %v, want context.Canceled marked as cut short before the new-parent listing", err)
	}
}
