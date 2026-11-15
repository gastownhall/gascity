//go:build gascity_native_beads

package beads

import (
	"reflect"
	"testing"
)

// DoltliteReadStore is a read store over bd: it must not acquire guarded
// batches through anything it embeds, or a caller requiring atomic batches
// would accept a store that cannot apply one.
func TestDoltliteReadStoreIsNotAGuardedBatcher(t *testing.T) {
	storeType := reflect.TypeOf((*DoltliteReadStore)(nil))
	for _, capability := range []reflect.Type{
		reflect.TypeOf((*GuardedBatcher)(nil)).Elem(),
		reflect.TypeOf((*GuardedBatcherHandleProvider)(nil)).Elem(),
	} {
		if storeType.Implements(capability) {
			t.Fatalf("%s implements %s", storeType, capability)
		}
	}
}
