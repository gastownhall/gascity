package beads

// MintSpec names the store each class handle of one scope wraps. The residency
// layer fills it from a topology; nothing else decides it.
type MintSpec struct {
	Work     Store
	Graph    Store
	Sessions Store
	Mail     Store
	Orders   Store
	Nudges   Store
}

// ClassStores is every class's handle for one scope.
type ClassStores struct {
	Work     WorkStore
	Graph    GraphStore
	Sessions SessionStore
	Mail     MailStore
	Orders   OrdersStore
	Nudges   NudgesStore
}

// MintClassStores builds the class handles spec names (typed-stores design
// §3.1). It is the production mint: storeref.NewStores is its only caller, and
// the storeclass lint reports any other (beadstest is the test mint). Each
// handle wraps the store spec names for its class, the same value, so a
// single-store city's six handles share one store.
func MintClassStores(spec MintSpec) ClassStores {
	return ClassStores{
		Work:     WorkStore{Store: spec.Work},
		Graph:    GraphStore{Store: spec.Graph},
		Sessions: SessionStore{Store: spec.Sessions},
		Mail:     MailStore{Store: spec.Mail},
		Orders:   OrdersStore{Store: spec.Orders},
		Nudges:   NudgesStore{Store: spec.Nudges},
	}
}
