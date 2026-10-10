package api

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// readCountingStore counts the reads that reach a bead store.
type readCountingStore struct {
	beads.Store
	reads atomic.Int32
}

func (s *readCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.List(q)
}

func (s *readCountingStore) Get(id string) (beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.Get(id)
}

func (s *readCountingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.Ready(q...)
}

// /status on a suspended city reads no bead store: a read would restart the
// city's retired bd proxy, and an open dashboard polls this. The body says
// it left the stores out.
func TestStatusOnASuspendedCityReadsNoStore(t *testing.T) {
	state := newFakeState(t)
	city := &readCountingStore{Store: beads.NewMemStore()}
	rig := &readCountingStore{Store: beads.NewMemStore()}
	state.cityBeadStore = city
	state.stores = map[string]beads.Store{"myrig": rig}
	state.cityMailProv = beadmail.New(city)
	suspended := true
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, state.cityPath, &suspended); err != nil {
		t.Fatal(err)
	}
	s := &Server{state: state}

	body := s.buildStatusBody(context.Background(), false)
	if n := city.reads.Load() + rig.reads.Load(); n != 0 {
		t.Fatalf("/status on a suspended city read the stores %d time(s)", n)
	}
	if !body.Suspended || !body.StoresNotRead {
		t.Fatalf("body suspended=%v stores_not_read=%v, want both true", body.Suspended, body.StoresNotRead)
	}

	resumed := false
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, state.cityPath, &resumed); err != nil {
		t.Fatal(err)
	}
	body = s.buildStatusBody(context.Background(), false)
	if body.StoresNotRead || city.reads.Load()+rig.reads.Load() == 0 {
		t.Fatalf("a resumed city's /status did not read its stores (stores_not_read=%v)", body.StoresNotRead)
	}
}
