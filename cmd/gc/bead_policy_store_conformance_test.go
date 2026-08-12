package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type recordingPolicyReadStore struct {
	*beads.MemStore
	listQueries  []beads.ListQuery
	readyQueries []beads.ReadyQuery
}

func (s *recordingPolicyReadStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listQueries = append(s.listQueries, query)
	return s.MemStore.List(query)
}

func (s *recordingPolicyReadStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	q := beads.ReadyQuery{}
	if len(query) > 0 {
		q = query[0]
	}
	s.readyQueries = append(s.readyQueries, q)
	return s.MemStore.Ready(q)
}

func (s *recordingPolicyReadStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.Ready(query...)
}

func TestBeadPolicyStoreReadHelperTierConformance(t *testing.T) {
	backing := &recordingPolicyReadStore{MemStore: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})

	parent, err := backing.Create(beads.Bead{Title: "parent", Type: "molecule"})
	if err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if _, err := backing.Create(beads.Bead{
		Title:    "child",
		ParentID: parent.ID,
		Labels:   []string{"scope"},
		Assignee: "worker",
		Metadata: map[string]string{"route": "a"},
	}); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	listCases := []struct {
		name string
		run  func() error
		want func(beads.ListQuery) error
	}{
		{
			name: "List expands default issues tier to both tiers",
			run: func() error {
				_, err := store.List(beads.ListQuery{Label: "scope"})
				return err
			},
			want: func(q beads.ListQuery) error {
				if q.Label != "scope" || q.TierMode != beads.TierBoth {
					return fmt.Errorf("query = %#v, want label scope and TierBoth", q)
				}
				return nil
			},
		},
		{
			name: "List preserves explicit wisps tier",
			run: func() error {
				_, err := store.List(beads.ListQuery{Label: "scope", TierMode: beads.TierWisps})
				return err
			},
			want: func(q beads.ListQuery) error {
				if q.Label != "scope" || q.TierMode != beads.TierWisps {
					return fmt.Errorf("query = %#v, want label scope and TierWisps", q)
				}
				return nil
			},
		},
		{
			name: "Children expands default issues tier to both tiers",
			run: func() error {
				_, err := store.Children(parent.ID)
				return err
			},
			want: func(q beads.ListQuery) error {
				if q.ParentID != parent.ID || q.Sort != beads.SortCreatedAsc || q.TierMode != beads.TierBoth {
					return fmt.Errorf("query = %#v, want parent, created asc, and TierBoth", q)
				}
				return nil
			},
		},
		{
			name: "ListByLabel expands default issues tier to both tiers",
			run: func() error {
				_, err := store.ListByLabel("scope", 7)
				return err
			},
			want: func(q beads.ListQuery) error {
				if q.Label != "scope" || q.Limit != 7 || q.TierMode != beads.TierBoth {
					return fmt.Errorf("query = %#v, want label scope, limit 7, and TierBoth", q)
				}
				return nil
			},
		},
		{
			name: "ListByAssignee expands default issues tier to both tiers",
			run: func() error {
				_, err := store.ListByAssignee("worker", "open", 3)
				return err
			},
			want: func(q beads.ListQuery) error {
				if q.Assignee != "worker" || q.Status != "open" || q.Limit != 3 || q.TierMode != beads.TierBoth {
					return fmt.Errorf("query = %#v, want assignee/status/limit and TierBoth", q)
				}
				return nil
			},
		},
		{
			name: "ListByMetadata expands default issues tier to both tiers",
			run: func() error {
				_, err := store.ListByMetadata(map[string]string{"route": "a"}, 4)
				return err
			},
			want: func(q beads.ListQuery) error {
				if q.Metadata["route"] != "a" || q.Limit != 4 || q.TierMode != beads.TierBoth {
					return fmt.Errorf("query = %#v, want metadata route=a, limit 4, and TierBoth", q)
				}
				return nil
			},
		},
		{
			name: "ListOpen expands default issues tier to both tiers",
			run: func() error {
				_, err := store.ListOpen()
				return err
			},
			want: func(q beads.ListQuery) error {
				if q.Status != "open" || !q.AllowScan || q.TierMode != beads.TierBoth {
					return fmt.Errorf("query = %#v, want open scan and TierBoth", q)
				}
				return nil
			},
		},
	}

	for _, tc := range listCases {
		t.Run(tc.name, func(t *testing.T) {
			backing.listQueries = nil
			if err := tc.run(); err != nil {
				t.Fatalf("read helper: %v", err)
			}
			if len(backing.listQueries) != 1 {
				t.Fatalf("captured list queries = %#v, want exactly one", backing.listQueries)
			}
			if err := tc.want(backing.listQueries[0]); err != nil {
				t.Fatal(err)
			}
		})
	}

	readyCases := []struct {
		name string
		run  func() error
		want beads.TierMode
	}{
		{
			name: "Ready expands default issues tier to both tiers",
			run: func() error {
				_, err := store.Ready()
				return err
			},
			want: beads.TierBoth,
		},
		{
			name: "Ready preserves explicit wisps tier",
			run: func() error {
				_, err := store.Ready(beads.ReadyQuery{TierMode: beads.TierWisps})
				return err
			},
			want: beads.TierWisps,
		},
	}

	for _, tc := range readyCases {
		t.Run(tc.name, func(t *testing.T) {
			backing.readyQueries = nil
			if err := tc.run(); err != nil {
				t.Fatalf("ready helper: %v", err)
			}
			if len(backing.readyQueries) != 1 {
				t.Fatalf("captured ready queries = %#v, want exactly one", backing.readyQueries)
			}
			if backing.readyQueries[0].TierMode != tc.want {
				t.Fatalf("Ready TierMode = %v, want %v", backing.readyQueries[0].TierMode, tc.want)
			}
		})
	}
}

func TestBeadPolicyStoreHandleReadsArePolicyAware(t *testing.T) {
	backing := &recordingPolicyReadStore{MemStore: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})
	handles := beads.HandlesFor(store)

	handleCases := []struct {
		name string
		run  func() error
	}{
		{
			name: "cached list",
			run: func() error {
				_, err := handles.Cached.List(beads.ListQuery{Status: "open"})
				return err
			},
		},
		{
			name: "live list",
			run: func() error {
				_, err := handles.Live.List(beads.ListQuery{Status: "open"})
				return err
			},
		},
	}
	for _, tc := range handleCases {
		t.Run(tc.name, func(t *testing.T) {
			backing.listQueries = nil
			if err := tc.run(); err != nil {
				t.Fatalf("handle list: %v", err)
			}
			if len(backing.listQueries) != 1 {
				t.Fatalf("captured list queries = %#v, want exactly one", backing.listQueries)
			}
			if backing.listQueries[0].TierMode != beads.TierBoth {
				t.Fatalf("handle List TierMode = %v, want TierBoth", backing.listQueries[0].TierMode)
			}
		})
	}

	readyHandleCases := []struct {
		name string
		run  func() error
	}{
		{
			name: "cached ready",
			run: func() error {
				_, err := handles.Cached.Ready()
				return err
			},
		},
		{
			name: "live ready",
			run: func() error {
				_, err := handles.Live.Ready()
				return err
			},
		},
	}
	for _, tc := range readyHandleCases {
		t.Run(tc.name, func(t *testing.T) {
			backing.readyQueries = nil
			if err := tc.run(); err != nil {
				t.Fatalf("handle ready: %v", err)
			}
			if len(backing.readyQueries) != 1 {
				t.Fatalf("captured ready queries = %#v, want exactly one", backing.readyQueries)
			}
			if backing.readyQueries[0].TierMode != beads.TierBoth {
				t.Fatalf("handle Ready TierMode = %v, want TierBoth", backing.readyQueries[0].TierMode)
			}
		})
	}
}

func TestBeadPolicyStoreContextReadyIsPolicyAware(t *testing.T) {
	backing := &recordingPolicyReadStore{MemStore: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})
	reader, ok := store.(beads.ContextReadyReader)
	if !ok {
		t.Fatalf("policy store type %T does not preserve ContextReadyReader", store)
	}

	if _, err := reader.ReadyContext(context.Background()); err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
	if len(backing.readyQueries) != 1 || backing.readyQueries[0].TierMode != beads.TierBoth {
		t.Fatalf("ReadyContext queries = %#v, want one TierBoth query", backing.readyQueries)
	}
}
