package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestStoreResolveAddressAgreesWithResolveSessionID is the equivalence oracle
// for the address seam: every fixture below is resolved twice, once through the
// package function raw-store callers use and once through the front door, and
// the two must name the same session. A directory that grew its own resolution
// order would fail here rather than in whichever caller moved onto it first.
func TestStoreResolveAddressAgreesWithResolveSessionID(t *testing.T) {
	store := beads.NewMemStore()
	create := func(metadata map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: metadata})
		if err != nil {
			t.Fatalf("Create session: %v", err)
		}
		return b
	}

	named := create(map[string]string{"session_name": "shared-address"})
	create(map[string]string{"alias": "shared-address"})
	dual := create(map[string]string{"alias": "dual", "session_name": "dual"})
	create(map[string]string{"session_name": "dual"})
	aliasOnly := create(map[string]string{"alias": "alias-only"})
	retired := create(map[string]string{"alias": "retired-address"})
	if err := store.Close(retired.ID); err != nil {
		t.Fatalf("Close session: %v", err)
	}

	for _, selector := range []string{
		"shared-address",
		"dual",
		"alias-only",
		"retired-address",
		named.ID,
		dual.ID,
		aliasOnly.ID,
		retired.ID,
		"absent",
	} {
		for _, includeClosed := range []bool{false, true} {
			resolve := ResolveSessionID
			if includeClosed {
				resolve = ResolveSessionIDAllowClosed
			}
			wantID, wantErr := resolve(store, selector)

			got, err := NewStore(beads.SessionStore{Store: store}).ResolveAddress(selector, includeClosed)

			switch {
			case wantErr != nil && err == nil:
				t.Fatalf("ResolveAddress(%q, %t) = %q, want the package function's error %v", selector, includeClosed, got.ID, wantErr)
			case wantErr != nil:
				for _, sentinel := range []error{ErrSessionNotFound, ErrAmbiguous} {
					if errors.Is(wantErr, sentinel) != errors.Is(err, sentinel) {
						t.Fatalf("ResolveAddress(%q, %t) error = %v, want the same %v classification as %v", selector, includeClosed, err, sentinel, wantErr)
					}
				}
			case err != nil:
				t.Fatalf("ResolveAddress(%q, %t) = %v, want %q", selector, includeClosed, err, wantID)
			case got.ID != wantID:
				t.Fatalf("ResolveAddress(%q, %t) = %q, want %q", selector, includeClosed, got.ID, wantID)
			}
		}
	}
}

// TestStoreResolveAddressResolvesAClosedSessionByID pins the exact-id arm's
// closed-inclusiveness: an identity stamped from a session that has since
// retired still resolves, which is what keeps a reply addressable after the
// sender is gone.
func TestStoreResolveAddressResolvesAClosedSessionByID(t *testing.T) {
	store := beads.NewMemStore()
	closed, err := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"session_name": "gone"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
	}

	got, err := NewStore(beads.SessionStore{Store: store}).ResolveAddress(closed.ID, false)
	if err != nil {
		t.Fatalf("ResolveAddress(closed id) = %v, want the closed session", err)
	}
	if got.ID != closed.ID {
		t.Fatalf("ResolveAddress(closed id) = %q, want %q", got.ID, closed.ID)
	}
}

// TestStoreResolveMailboxAddressIsAmbiguousAcrossAddressKeys pins the mailbox
// seam's defining difference from ResolveAddress: the bead id, the alias and
// the canonical name are all candidates at once, so two sessions claiming one
// address is ambiguous rather than won by the key that was probed first.
func TestStoreResolveMailboxAddressIsAmbiguousAcrossAddressKeys(t *testing.T) {
	t.Run("alias against session_name", func(t *testing.T) {
		store := beads.NewMemStore()
		mustCreateAddressed(t, store, map[string]string{"session_name": "shared"})
		mustCreateAddressed(t, store, map[string]string{"alias": "shared"})

		_, err := NewStore(beads.SessionStore{Store: store}).ResolveMailboxAddress("shared", false)
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("ResolveMailboxAddress = %v, want ErrAmbiguous", err)
		}
	})

	t.Run("bead id against alias", func(t *testing.T) {
		store := beads.NewMemStore()
		target := mustCreateAddressed(t, store, map[string]string{"session_name": "target"})
		mustCreateAddressed(t, store, map[string]string{"alias": target.ID})

		_, err := NewStore(beads.SessionStore{Store: store}).ResolveMailboxAddress(target.ID, false)
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("ResolveMailboxAddress = %v, want ErrAmbiguous", err)
		}
	})

	t.Run("one session claiming an address twice is not ambiguous", func(t *testing.T) {
		store := beads.NewMemStore()
		dual := mustCreateAddressed(t, store, map[string]string{"alias": "both", "session_name": "both"})

		got, err := NewStore(beads.SessionStore{Store: store}).ResolveMailboxAddress("both", false)
		if err != nil {
			t.Fatalf("ResolveMailboxAddress = %v, want the single owner", err)
		}
		if got.ID != dual.ID {
			t.Fatalf("ResolveMailboxAddress = %q, want %q", got.ID, dual.ID)
		}
	})
}

// TestStoreResolveMailboxAddressSeparatesLivenessPasses pins that the closed
// pass is closed-ONLY: a live session never answers it, so a caller running
// live-then-closed cannot see the same session twice.
func TestStoreResolveMailboxAddressSeparatesLivenessPasses(t *testing.T) {
	store := beads.NewMemStore()
	live := mustCreateAddressed(t, store, map[string]string{"alias": "live-one"})
	retired := mustCreateAddressed(t, store, map[string]string{"alias": "retired-one"})
	if err := store.Close(retired.ID); err != nil {
		t.Fatal(err)
	}
	directory := NewStore(beads.SessionStore{Store: store})

	if got, err := directory.ResolveMailboxAddress("live-one", false); err != nil || got.ID != live.ID {
		t.Fatalf("ResolveMailboxAddress(live, live pass) = (%q, %v), want %q", got.ID, err, live.ID)
	}
	if _, err := directory.ResolveMailboxAddress("live-one", true); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ResolveMailboxAddress(live, closed pass) = %v, want ErrSessionNotFound", err)
	}
	if _, err := directory.ResolveMailboxAddress("retired-one", false); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ResolveMailboxAddress(retired, live pass) = %v, want ErrSessionNotFound", err)
	}
	if got, err := directory.ResolveMailboxAddress("retired-one", true); err != nil || got.ID != retired.ID {
		t.Fatalf("ResolveMailboxAddress(retired, closed pass) = (%q, %v), want %q", got.ID, err, retired.ID)
	}
}

// TestStoreResolveMailboxAddressRejectsALooseMetadataAnswer pins the re-check
// on the rows the metadata probe returns. A store that answers the filter
// loosely would otherwise hand a message to a session that never claimed the
// address.
func TestStoreResolveMailboxAddressRejectsALooseMetadataAnswer(t *testing.T) {
	store := beads.NewMemStore()
	mustCreateAddressed(t, store, map[string]string{"alias": "someone-else"})

	_, err := NewStore(beads.SessionStore{Store: looseMetadataStore{Store: store}}).ResolveMailboxAddress("wanted", false)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ResolveMailboxAddress over a loose metadata filter = %v, want ErrSessionNotFound", err)
	}
}

// TestStoreResolveMailboxAddressSkipsTheIDLookupForSlashAddresses pins that a
// slash address never reaches the bead-id lookup: legacy backends render an id
// probe into a query clause, and a slash in that clause is a malformed query.
func TestStoreResolveMailboxAddressSkipsTheIDLookupForSlashAddresses(t *testing.T) {
	store := beads.NewMemStore()
	mustCreateAddressed(t, store, map[string]string{"alias": "rig/agent.name"})
	guard := &noIDLookupStore{Store: store, t: t}

	got, err := NewStore(beads.SessionStore{Store: guard}).ResolveMailboxAddress("rig/agent.name", false)
	if err != nil {
		t.Fatalf("ResolveMailboxAddress(slash address) = %v", err)
	}
	if got.Alias != "rig/agent.name" {
		t.Fatalf("ResolveMailboxAddress(slash address) = %q, want the aliased session", got.Alias)
	}
}

func TestStoreAddressErrorsKeepTheSelectorAndTheBackendMessageOut(t *testing.T) {
	t.Run("resolution not found", func(t *testing.T) {
		const sensitiveSelector = "sensitive-missing-session-address"
		directory := NewStore(beads.SessionStore{Store: beads.NewMemStore()})

		_, err := directory.ResolveAddress(sensitiveSelector, false)

		if !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("ResolveAddress(missing) error = %v, want ErrSessionNotFound", err)
		}
		requireRedactedAddressError(t, err, sensitiveSelector)
	})

	t.Run("resolution backend failure", func(t *testing.T) {
		const sensitiveSelector = "sensitive-direct-session-address"
		directory := NewStore(beads.SessionStore{Store: &failingAddressGetStore{
			Store:     beads.NewMemStore(),
			sensitive: sensitiveSelector,
		}})

		_, err := directory.ResolveAddress(sensitiveSelector, false)

		if !errors.Is(err, errAddressDirectoryBackend) {
			t.Fatalf("ResolveAddress(backend failure) error = %v, want backend classification", err)
		}
		requireRedactedAddressError(t, err, sensitiveSelector)
		if !strings.Contains(err.Error(), "address resolution") {
			t.Fatalf("ResolveAddress(backend failure) error = %q, want useful operation context", err)
		}
	})

	t.Run("mailbox direct lookup failure", func(t *testing.T) {
		const sensitiveSelector = "sensitive-mailbox-session-address"
		directory := NewStore(beads.SessionStore{Store: &failingAddressGetStore{
			Store:     beads.NewMemStore(),
			sensitive: sensitiveSelector,
		}})

		_, err := directory.ResolveMailboxAddress(sensitiveSelector, false)

		if !errors.Is(err, errAddressDirectoryBackend) {
			t.Fatalf("ResolveMailboxAddress(direct failure) error = %v, want backend classification", err)
		}
		requireRedactedAddressError(t, err, sensitiveSelector)
		if !strings.Contains(err.Error(), "direct lookup") {
			t.Fatalf("ResolveMailboxAddress(direct failure) error = %q, want useful operation context", err)
		}
	})

	t.Run("mailbox metadata failure", func(t *testing.T) {
		const sensitiveSelector = "sensitive-metadata-session-address"
		directory := NewStore(beads.SessionStore{Store: &failingAddressListStore{
			Store:     beads.NewMemStore(),
			sensitive: sensitiveSelector,
		}})

		_, err := directory.ResolveMailboxAddress(sensitiveSelector, false)

		if !errors.Is(err, errAddressDirectoryBackend) {
			t.Fatalf("ResolveMailboxAddress(metadata failure) error = %v, want backend classification", err)
		}
		requireRedactedAddressError(t, err, sensitiveSelector)
		if !strings.Contains(err.Error(), "alias lookup") {
			t.Fatalf("ResolveMailboxAddress(metadata failure) error = %q, want useful operation context", err)
		}
	})

	t.Run("mailbox ambiguity", func(t *testing.T) {
		const sensitiveSelector = "sensitive-ambiguous-address"
		store := beads.NewMemStore()
		var sensitiveIDs []string
		for range 2 {
			sensitiveIDs = append(sensitiveIDs, mustCreateAddressed(t, store, map[string]string{"alias": sensitiveSelector}).ID)
		}

		_, err := NewStore(beads.SessionStore{Store: store}).ResolveMailboxAddress(sensitiveSelector, false)

		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("ResolveMailboxAddress(ambiguous) error = %v, want ErrAmbiguous", err)
		}
		requireRedactedAddressError(t, err, append([]string{sensitiveSelector}, sensitiveIDs...)...)
		if !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("ResolveMailboxAddress(ambiguous) error = %q, want useful ambiguity context", err)
		}
	})
}

// TestStoreResolveMailboxAddressHealsAnEmptyTypeSessionBead pins the repair
// the mailbox path performs as a side effect. A session bead that lost its type
// to a crash or a partial write still answers to its address, and the row is
// healed on the way past so the next reader does not have to know about the
// damage.
func TestStoreResolveMailboxAddressHealsAnEmptyTypeSessionBead(t *testing.T) {
	store := beads.NewMemStore()
	damaged, err := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"alias": "damaged"}})
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	if err := store.Update(damaged.ID, beads.UpdateOpts{Type: &empty}); err != nil {
		t.Fatalf("clearing the fixture type: %v", err)
	}

	got, err := NewStore(beads.SessionStore{Store: store}).ResolveMailboxAddress("damaged", false)
	if err != nil {
		t.Fatalf("ResolveMailboxAddress = %v, want the repairable session", err)
	}
	if got.ID != damaged.ID {
		t.Fatalf("ResolveMailboxAddress = %q, want %q", got.ID, damaged.ID)
	}
	healed, err := store.Get(damaged.ID)
	if err != nil {
		t.Fatal(err)
	}
	if healed.Type != BeadType {
		t.Fatalf("persisted Type = %q after resolution, want %q", healed.Type, BeadType)
	}
}

func mustCreateAddressed(t *testing.T, store beads.Store, metadata map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: metadata})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	return b
}

var errAddressDirectoryBackend = errors.New("address directory backend unavailable")

type failingAddressGetStore struct {
	beads.Store
	sensitive string
}

func (s *failingAddressGetStore) Get(id string) (beads.Bead, error) {
	return beads.Bead{}, fmt.Errorf("%w: direct lookup exposed %s via %s", errAddressDirectoryBackend, s.sensitive, id)
}

type failingAddressListStore struct {
	beads.Store
	sensitive string
}

func (s *failingAddressListStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, beads.ErrNotFound
}

func (s *failingAddressListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("%w: metadata filter exposed %s", errAddressDirectoryBackend, s.sensitive)
}

// looseMetadataStore answers every metadata filter with every session it holds,
// which is what a backend that indexes loosely looks like from up here.
type looseMetadataStore struct {
	beads.Store
}

func (s looseMetadataStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if len(query.Metadata) > 0 {
		query.Metadata = nil
		query.Label = LabelSession
	}
	return s.Store.List(query)
}

type noIDLookupStore struct {
	beads.Store
	t *testing.T
}

func (s *noIDLookupStore) Get(id string) (beads.Bead, error) {
	if strings.Contains(id, "/") {
		s.t.Fatalf("mailbox resolution performed a bead-id lookup for the slash address %q", id)
	}
	return s.Store.Get(id)
}

func requireRedactedAddressError(t *testing.T, err error, sensitiveValues ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("address error = nil")
	}
	for _, sensitive := range sensitiveValues {
		if sensitive != "" && strings.Contains(err.Error(), sensitive) {
			t.Fatalf("address error %q leaks %q", err, sensitive)
		}
	}
}
