package beads

import (
	"testing"
	"time"
)

// The creator is resolved once, when the store is built, and it reads the
// ambient environment through AmbientNativeDoltOpenEnv. A native open elsewhere
// in the process holds nativeDoltOpenEnvMu and withholds the whole BEADS_ and BD_
// namespaces for its window; a bare os.Getenv would see BEADS_ACTOR unset there
// and fall through to git or USER. The store must instead wait the window out
// and take the restored ambient value.
func TestNativeDoltStoreCreatorResolvesAmbientActorAfterOpenWindow(t *testing.T) {
	const ambient = "order:window-test"
	t.Setenv("BEADS_ACTOR", ambient)
	t.Setenv("BD_ACTOR", "")

	end := holdOpenWindow(t)

	built := make(chan *NativeDoltStore, 1)
	go func() { built <- newNativeDoltStoreWithStorage(nil, "") }()

	select {
	case store := <-built:
		end()
		t.Fatalf("store built inside the open window with creator %q; it must wait on nativeDoltOpenEnvMu", store.creator)
	case <-time.After(50 * time.Millisecond):
	}
	end()

	select {
	case store := <-built:
		if store.creator != ambient {
			t.Errorf("creator = %q, want the restored ambient BEADS_ACTOR %q", store.creator, ambient)
		}
	case <-time.After(openWindowPatience):
		t.Fatal("store was not built after the open window ended")
	}
}

// WithNativeCreator overrides whatever the ambient environment says.
func TestNativeDoltStoreWithNativeCreatorOverridesAmbient(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "order:ambient")
	store := newNativeDoltStoreWithStorage(nil, "")
	WithNativeCreator("pinned-creator")(store)
	if store.creator != "pinned-creator" {
		t.Errorf("creator = %q, want %q", store.creator, "pinned-creator")
	}
}
