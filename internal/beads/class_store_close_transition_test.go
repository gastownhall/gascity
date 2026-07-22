package beads

import "testing"

type storeWithoutCloseTransitioner struct {
	Store
}

func closeTransitionClassStores() []struct {
	name string
	wrap func(Store) Store
} {
	return []struct {
		name string
		wrap func(Store) Store
	}{
		{name: "WorkStore", wrap: func(store Store) Store { return WorkStore{Store: store} }},
		{name: "GraphStore", wrap: func(store Store) Store { return GraphStore{Store: store} }},
		{name: "SessionStore", wrap: func(store Store) Store { return SessionStore{Store: store} }},
		{name: "MailStore", wrap: func(store Store) Store { return MailStore{Store: store} }},
		{name: "OrdersStore", wrap: func(store Store) Store { return OrdersStore{Store: store} }},
		{name: "NudgesStore", wrap: func(store Store) Store { return NudgesStore{Store: store} }},
	}
}

func TestClassStoresForwardCloseTransitionerHandle(t *testing.T) {
	for _, tt := range closeTransitionClassStores() {
		t.Run(tt.name, func(t *testing.T) {
			base := NewMemStore()
			wrapped := tt.wrap(base)

			transitioner, ok := CloseTransitionerFor(wrapped)
			if !ok {
				t.Fatalf("CloseTransitionerFor(%T) reported unsupported", wrapped)
			}
			if transitioner != CloseTransitioner(base) {
				t.Fatalf("CloseTransitionerFor(%T) returned %T, want exact underlying %T", wrapped, transitioner, base)
			}

			created, err := wrapped.Create(Bead{Title: "class-wrapped close target"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			transition, err := transitioner.CloseWithReasonIfOpen(created.ID, "forwarded close")
			if err != nil {
				t.Fatalf("CloseWithReasonIfOpen: %v", err)
			}
			if !transition.Transitioned {
				t.Fatal("Transitioned = false, want true")
			}
			if got := transition.After.Metadata["close_reason"]; got != "forwarded close" {
				t.Fatalf("After close_reason = %q, want forwarded close", got)
			}
		})
	}
}

func TestClassStoresReportMissingCloseTransitionerCapability(t *testing.T) {
	for _, tt := range closeTransitionClassStores() {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := tt.wrap(storeWithoutCloseTransitioner{Store: NewMemStore()})

			transitioner, ok := CloseTransitionerFor(wrapped)
			if ok {
				t.Fatalf("CloseTransitionerFor(%T) reported supported with hidden capability", wrapped)
			}
			if transitioner != nil {
				t.Fatalf("CloseTransitionerFor(%T) = %T, want nil", wrapped, transitioner)
			}
		})
	}
}

func TestClassStoresForwardCloseObserverSuppressorHandle(t *testing.T) {
	for _, tt := range closeTransitionClassStores() {
		t.Run(tt.name, func(t *testing.T) {
			base := NewCachingStoreForTest(NewMemStore(), nil)
			wrapped := tt.wrap(base)

			suppressor, ok := CloseObserverSuppressorFor(wrapped)
			if !ok {
				t.Fatalf("CloseObserverSuppressorFor(%T) reported unsupported", wrapped)
			}
			if suppressor != CloseObserverSuppressor(base) {
				t.Fatalf("CloseObserverSuppressorFor(%T) returned %T, want exact underlying %T", wrapped, suppressor, base)
			}
		})
	}
}

func TestClassStoresReportMissingCloseObserverSuppressorCapability(t *testing.T) {
	for _, tt := range closeTransitionClassStores() {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := tt.wrap(NewMemStore())

			suppressor, ok := CloseObserverSuppressorFor(wrapped)
			if ok {
				t.Fatalf("CloseObserverSuppressorFor(%T) reported supported", wrapped)
			}
			if suppressor != nil {
				t.Fatalf("CloseObserverSuppressorFor(%T) = %T, want nil", wrapped, suppressor)
			}
		})
	}
}

func TestClassStoresForwardObserverBarrierHandle(t *testing.T) {
	for _, tt := range closeTransitionClassStores() {
		t.Run(tt.name, func(t *testing.T) {
			base := NewCachingStoreForTest(NewMemStore(), nil)
			wrapped := tt.wrap(base)

			barrier, ok := ObserverBarrierFor(wrapped)
			if !ok {
				t.Fatalf("ObserverBarrierFor(%T) reported unsupported", wrapped)
			}
			if barrier != ObserverBarrier(base) {
				t.Fatalf("ObserverBarrierFor(%T) returned %T, want exact underlying %T", wrapped, barrier, base)
			}
		})
	}
}

func TestClassStoresReportMissingObserverBarrierCapability(t *testing.T) {
	for _, tt := range closeTransitionClassStores() {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := tt.wrap(NewMemStore())

			barrier, ok := ObserverBarrierFor(wrapped)
			if ok {
				t.Fatalf("ObserverBarrierFor(%T) reported supported", wrapped)
			}
			if barrier != nil {
				t.Fatalf("ObserverBarrierFor(%T) = %T, want nil", wrapped, barrier)
			}
		})
	}
}
