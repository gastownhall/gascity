package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper pins the stage-3
// wiring hazard: every factory store is policy-wrapped, and interface
// embedding hides the factory's conditional-writes stamp — without the
// wrapper's declared resolution target, a require deployment would silently
// resolve unset→legacy through the wrapper on every consumer.
func TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper(t *testing.T) {
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Require,
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}

	wrapped := wrapStoreWithBeadPolicies(result.Store, nil)
	if _, _, ok := unwrapBeadPolicyStore(wrapped); !ok {
		t.Fatalf("test premise: store %T is not policy-wrapped", wrapped)
	}

	writer, diag, resolveErr := beads.ResolveConditionalWriter(wrapped)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve through policy wrapper = diag %v err %v, want the stamped store's writer", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("resolve through policy wrapper returned no writer: the require stamp was hidden by interface embedding")
	}
}

func TestBeadPolicyStorePreservesAtomicTransactionAndIDNamespace(t *testing.T) {
	opened, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("mail"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	closer := opened.(interface{ CloseStore() error })
	t.Cleanup(func() {
		if err := closer.CloseStore(); err != nil {
			t.Errorf("CloseStore: %v", err)
		}
	})

	wrapped := wrapStoreWithBeadPolicies(opened, nil)
	if !beads.StoreSupportsAtomicTx(wrapped) {
		t.Fatalf("StoreSupportsAtomicTx(%T) = false, want the SQLite backing's true guarantee", wrapped)
	}
	prefixer, ok := wrapped.(interface{ IDPrefix() string })
	if !ok || prefixer.IDPrefix() != "mail" {
		t.Fatalf("policy-wrapped IDPrefix = (%T, %q), want mail", wrapped, func() string {
			if !ok {
				return ""
			}
			return prefixer.IDPrefix()
		}())
	}

	if beads.StoreSupportsAtomicTx(wrapStoreWithBeadPolicies(beads.NewMemStore(), nil)) {
		t.Fatal("policy wrapper invented atomic rollback over MemStore")
	}
}
