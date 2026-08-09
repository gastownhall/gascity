package storebinding

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestComposeRetainedWorkTopologyPreservesMemberConfigOrder(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("unified-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "unified-workspace"),
		workMember(RigScope("zeta"), "z", 1, false, "unified-workspace"),
		workMember(RigScope("alpha"), "a", 2, true, "unified-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	handle, err := NewRetainedWorkWorkspaceWithViews(participant, retainedPrefixViews(participant), func() error { return nil })
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspace(): %v", err)
	}

	topology, err := ComposeRetainedWorkTopology([]RetainedWorkWorkspace{handle})
	if err != nil {
		t.Fatalf("ComposeRetainedWorkTopology(): %v", err)
	}
	rigs := topology.RigsInConfigOrder(true)
	if len(rigs) != 2 || rigs[0].Scope != RigScope("zeta") || rigs[1].Scope != RigScope("alpha") {
		t.Fatalf("config-order rigs = %#v, want zeta then alpha", rigs)
	}
}

func TestRetainedUnifiedWorkspacePreservesPrefixViewsWithOnePhysicalHandle(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("unified-prefix-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "unified-prefix-workspace"),
		workMember(RigScope("rig"), "rig", 1, false, "unified-prefix-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	sharedStore := beads.NewMemStore()
	var closeCalls atomic.Int32
	handle, err := NewRetainedWorkWorkspaceWithViews(participant, retainedPrefixViewsOver(participant, sharedStore), func() error {
		closeCalls.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspace(): %v", err)
	}

	hqView, err := handle.View(HQScope())
	if err != nil {
		t.Fatalf("View(HQ): %v", err)
	}
	rigView, err := handle.View(RigScope("rig"))
	if err != nil {
		t.Fatalf("View(rig): %v", err)
	}
	if prefix, ok := hqView.Store.(interface{ IDPrefix() string }); !ok || prefix.IDPrefix() != "hq" {
		t.Fatalf("HQ view prefix = %#v, want hq", hqView.Store)
	}
	if prefix, ok := rigView.Store.(interface{ IDPrefix() string }); !ok || prefix.IDPrefix() != "rig" {
		t.Fatalf("rig view prefix = %#v, want rig", rigView.Store)
	}
	created, err := rigView.Store.Create(beads.Bead{Title: "shared physical work"})
	if err != nil {
		t.Fatalf("rig view Create(): %v", err)
	}
	if created.ID != "rig-1" {
		t.Fatalf("rig view Create() ID = %q, want rig-1", created.ID)
	}
	if _, err := hqView.Store.Get(created.ID); err != nil {
		t.Fatalf("HQ view could not read work created through unified rig view: %v", err)
	}

	topology, err := ComposeRetainedWorkTopology([]RetainedWorkWorkspace{handle})
	if err != nil {
		t.Fatalf("ComposeRetainedWorkTopology(): %v", err)
	}
	if scope, err := topology.ScopeForID("rig-123"); err != nil || scope != RigScope("rig") {
		t.Fatalf("ScopeForID(rig-123) = %s, %v; want rig scope", scope, err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("first Close(): %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("second Close(): %v", err)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("physical close calls = %d, want 1", got)
	}
}

func TestNewRetainedWorkWorkspaceRejectsSingleStoreForDistinctPinnedPrefixes(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("unified-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "unified-workspace"),
		workMember(RigScope("rig"), "rig", 1, false, "unified-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	_, err = NewRetainedWorkWorkspace(participant, retainedPrefixView{Store: beads.NewMemStore(), prefix: "hq"}, func() error { return nil })
	if !errors.Is(err, ErrInvalidWorkParticipant) {
		t.Fatalf("NewRetainedWorkWorkspace() error = %v, want ErrInvalidWorkParticipant", err)
	}
}

func TestRetainedWorkWorkspaceRevalidatesFrozenPrefixWithoutReadingMutableIDPrefix(t *testing.T) {
	participant, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("frozen-prefix-workspace"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "frozen-prefix-workspace"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(): %v", err)
	}
	store := &mutablePrefixProbe{Store: beads.NewMemStore(), frozen: "hq"}
	workspace, err := NewRetainedWorkWorkspace(participant, store, func() error { return nil })
	if err != nil {
		t.Fatalf("NewRetainedWorkWorkspace(): %v", err)
	}
	if store.idPrefixCalls != 0 {
		t.Fatalf("NewRetainedWorkWorkspace() read mutable IDPrefix %d times", store.idPrefixCalls)
	}
	store.frozen = "changed-after-open"
	if err := workspace.Validate(); !errors.Is(err, ErrInvalidWorkParticipant) {
		t.Fatalf("RetainedWorkWorkspace.Validate() error = %v, want ErrInvalidWorkParticipant", err)
	}
	if _, err := ComposeRetainedWorkTopology([]RetainedWorkWorkspace{workspace}); !errors.Is(err, ErrInvalidWorkParticipant) {
		t.Fatalf("ComposeRetainedWorkTopology() error = %v, want ErrInvalidWorkParticipant", err)
	}
	if store.idPrefixCalls != 0 {
		t.Fatalf("retained workspace reread mutable IDPrefix %d times", store.idPrefixCalls)
	}
}

func TestRetainedWorkWorkspaceConstructorsCloseRejectedResources(t *testing.T) {
	oneMember, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("rejected-one-member"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "rejected-one-member"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(one): %v", err)
	}
	twoMembers, err := NewWorkWorkspaceParticipant(ProviderID("builtin-work"), ComponentID("work"), PhysicalIdentity("rejected-two-members"), []WorkWorkspaceMember{
		workMember(HQScope(), "hq", 0, false, "rejected-two-members"),
		workMember(RigScope("rig"), "rig", 1, false, "rejected-two-members"),
	})
	if err != nil {
		t.Fatalf("NewWorkWorkspaceParticipant(two): %v", err)
	}

	for _, test := range []struct {
		name  string
		build func(func() error) error
	}{
		{
			name: "invalid participant",
			build: func(closeFn func() error) error {
				_, err := NewRetainedWorkWorkspace(WorkWorkspaceParticipant{}, retainedPrefixView{Store: beads.NewMemStore(), prefix: "hq"}, closeFn)
				return err
			},
		},
		{
			name: "typed nil store",
			build: func(closeFn func() error) error {
				var store *mutablePrefixProbe
				_, err := NewRetainedWorkWorkspace(oneMember, store, closeFn)
				return err
			},
		},
		{
			name: "typed nil view store",
			build: func(closeFn func() error) error {
				var store *mutablePrefixProbe
				_, err := NewRetainedWorkWorkspaceWithViews(oneMember, []RetainedWorkMemberView{{Scope: HQScope(), Prefix: "hq", Store: store}}, closeFn)
				return err
			},
		},
		{
			name: "prefix mismatch",
			build: func(closeFn func() error) error {
				view := retainedPrefixView{Store: beads.NewMemStore(), prefix: "hq"}
				_, err := NewRetainedWorkWorkspaceWithViews(oneMember, []RetainedWorkMemberView{{Scope: HQScope(), Prefix: "other", Store: view}}, closeFn)
				return err
			},
		},
		{
			name: "scope mismatch",
			build: func(closeFn func() error) error {
				view := retainedPrefixView{Store: beads.NewMemStore(), prefix: "hq"}
				_, err := NewRetainedWorkWorkspaceWithViews(oneMember, []RetainedWorkMemberView{{Scope: RigScope("other"), Prefix: "hq", Store: view}}, closeFn)
				return err
			},
		},
		{
			name: "duplicate scope",
			build: func(closeFn func() error) error {
				_, err := NewRetainedWorkWorkspaceWithViews(twoMembers, []RetainedWorkMemberView{
					{Scope: HQScope(), Prefix: "hq", Store: retainedPrefixView{Store: beads.NewMemStore(), prefix: "hq"}},
					{Scope: HQScope(), Prefix: "rig", Store: retainedPrefixView{Store: beads.NewMemStore(), prefix: "rig"}},
				}, closeFn)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			closeErr := errors.New("close rejected retained workspace")
			var closeCalls atomic.Int32
			err := test.build(func() error {
				closeCalls.Add(1)
				return closeErr
			})
			if !errors.Is(err, ErrInvalidWorkParticipant) {
				t.Fatalf("rejected constructor error = %v, want ErrInvalidWorkParticipant", err)
			}
			if !errors.Is(err, closeErr) {
				t.Fatalf("rejected constructor error = %v, want joined close error", err)
			}
			if calls := closeCalls.Load(); calls != 1 {
				t.Fatalf("rejected constructor close calls = %d, want 1", calls)
			}
		})
	}
}

func TestRetainedWorkWorkspaceRejectedConstructorRetainsRetryCleanup(t *testing.T) {
	closeErr := errors.New("rejected retained workspace close failed")
	attempts := 0
	_, err := NewRetainedWorkWorkspace(WorkWorkspaceParticipant{}, retainedPrefixView{Store: beads.NewMemStore(), prefix: "hq"}, func() error {
		attempts++
		if attempts == 1 {
			return closeErr
		}
		return nil
	})
	if !errors.Is(err, ErrInvalidWorkParticipant) || !errors.Is(err, closeErr) {
		t.Fatalf("NewRetainedWorkWorkspace() error = %v, want invalid participant and close error", err)
	}
	var pending *RejectedRetainedWorkWorkspaceCleanupError
	if !errors.As(err, &pending) {
		t.Fatalf("NewRetainedWorkWorkspace() error = %T, want *RejectedRetainedWorkWorkspaceCleanupError", err)
	}
	if err := pending.RetryCleanup(); err != nil {
		t.Fatalf("RetryCleanup(): %v", err)
	}
	if err := pending.RetryCleanup(); err != nil {
		t.Fatalf("idempotent RetryCleanup(): %v", err)
	}
	if attempts != 2 {
		t.Fatalf("rejected workspace close attempts = %d, want failed then successful retry", attempts)
	}
}

func workMember(scope WorkScope, prefix string, order int, suspended bool, identity PhysicalIdentity) WorkWorkspaceMember {
	return WorkWorkspaceMember{
		Scope:            scope,
		Prefix:           prefix,
		ConfigContext:    testConfigDigest("work-config"),
		Suspended:        suspended,
		ConfigOrder:      order,
		Provider:         ProviderID("builtin-work"),
		Component:        ComponentID("work"),
		PhysicalIdentity: identity,
	}
}

func retainedPrefixViews(participant WorkWorkspaceParticipant) []RetainedWorkMemberView {
	return retainedPrefixViewsOver(participant, beads.NewMemStore())
}

func retainedPrefixViewsOver(participant WorkWorkspaceParticipant, store beads.Store) []RetainedWorkMemberView {
	backing := &retainedPrefixBacking{aliases: make(map[string]string)}
	views := make([]RetainedWorkMemberView, len(participant.Members))
	for index, member := range participant.Members {
		views[index] = RetainedWorkMemberView{
			Scope:  member.Scope,
			Prefix: member.Prefix,
			Store: retainedPrefixView{
				Store:   store,
				prefix:  member.Prefix,
				backing: backing,
			},
		}
	}
	return views
}

type retainedPrefixBacking struct {
	mu      sync.Mutex
	next    int
	aliases map[string]string
}

type retainedPrefixView struct {
	beads.Store
	prefix  string
	backing *retainedPrefixBacking
}

func (v retainedPrefixView) IDPrefix() string { return v.prefix }

func (v retainedPrefixView) FrozenWorkPrefix() string { return v.prefix }

func (v retainedPrefixView) Create(bead beads.Bead) (beads.Bead, error) {
	created, err := v.Store.Create(bead)
	if err != nil {
		return beads.Bead{}, err
	}
	if v.backing == nil {
		return created, nil
	}
	v.backing.mu.Lock()
	defer v.backing.mu.Unlock()
	v.backing.next++
	visibleID := fmt.Sprintf("%s-%d", v.prefix, v.backing.next)
	v.backing.aliases[visibleID] = created.ID
	created.ID = visibleID
	return created, nil
}

func (v retainedPrefixView) Get(id string) (beads.Bead, error) {
	if v.backing == nil {
		return v.Store.Get(id)
	}
	v.backing.mu.Lock()
	backingID := v.backing.aliases[id]
	v.backing.mu.Unlock()
	if backingID == "" {
		backingID = id
	}
	got, err := v.Store.Get(backingID)
	if err != nil {
		return beads.Bead{}, err
	}
	if backingID != id {
		got.ID = id
	}
	return got, nil
}

type mutablePrefixProbe struct {
	beads.Store
	frozen        string
	idPrefixCalls int
}

func (s *mutablePrefixProbe) IDPrefix() string {
	s.idPrefixCalls++
	return s.frozen
}

func (s *mutablePrefixProbe) FrozenWorkPrefix() string { return s.frozen }
