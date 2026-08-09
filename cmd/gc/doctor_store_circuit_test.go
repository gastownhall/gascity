package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type blockingDoctorStore struct {
	beads.Store
	release <-chan struct{}
}

func (s blockingDoctorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	<-s.release
	return nil, nil
}

func TestBoundedDoctorStoreTripsSharedCircuit(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	oldTimeout := doctorStoreReadTimeout
	doctorStoreReadTimeout = 20 * time.Millisecond
	t.Cleanup(func() { doctorStoreReadTimeout = oldTimeout })

	circuit := &doctorStoreCircuit{}
	store := boundedDoctorStore{Store: blockingDoctorStore{release: release}, circuit: circuit}
	if _, err := store.List(beads.ListQuery{AllowScan: true}); err == nil {
		t.Fatal("first List error = nil, want timeout")
	}
	start := time.Now()
	_, err := store.List(beads.ListQuery{AllowScan: true})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("second List error = %v, want tripped circuit error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("second List took %s, want fast circuit failure", elapsed)
	}
}
