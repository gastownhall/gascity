package extmsg

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestSharedBindingLockPoolIsPerPhysicalStore pins the pool's key: every
// service over one store must take its conversation locks from one pool, so
// the controller's services (handed the messaging handle) and a session sweep
// (handed a messaging handle over its sessions handle) exclude each other.
// Keying on the handle value would give the two different pools.
func TestSharedBindingLockPoolIsPerPhysicalStore(t *testing.T) {
	store := beads.NewMemStore()
	direct := sharedBindingLockPool(beadstest.Mail(store))
	if nested := sharedBindingLockPool(beadstest.Mail(beadstest.Sessions(store))); nested != direct {
		t.Error("a messaging handle over a sessions handle over the store got its own lock pool")
	}
	if other := sharedBindingLockPool(beadstest.Mail(beads.NewMemStore())); other == direct {
		t.Error("two stores share one lock pool")
	}
}
