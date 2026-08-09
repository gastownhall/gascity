package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// doctorStoreReadTimeout bounds the first stalled BD read during a doctor
// run. Once tripped, the shared circuit makes later advisory store checks
// fail immediately rather than serially consuming the generic 60s check
// budget.
var doctorStoreReadTimeout = 2 * time.Second

type doctorStoreCircuit struct {
	mu  sync.Mutex
	err error
}

func (c *doctorStoreCircuit) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *doctorStoreCircuit) trip(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
	return c.err
}

type boundedDoctorStore struct {
	beads.Store
	circuit *doctorStoreCircuit
}

func (s boundedDoctorStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if err := s.circuit.failure(); err != nil {
		return nil, err
	}
	type result struct {
		items []beads.Bead
		err   error
	}
	done := make(chan result, 1)
	go func() { items, err := s.Store.List(q); done <- result{items, err} }()
	select {
	case r := <-done:
		return r.items, r.err
	case <-time.After(doctorStoreReadTimeout):
		return nil, s.circuit.trip(fmt.Errorf("doctor bead-store read timed out after %s", doctorStoreReadTimeout))
	}
}

func (s boundedDoctorStore) ListOpen(status ...string) ([]beads.Bead, error) {
	if err := s.circuit.failure(); err != nil {
		return nil, err
	}
	type result struct {
		items []beads.Bead
		err   error
	}
	done := make(chan result, 1)
	go func() { items, err := s.Store.ListOpen(status...); done <- result{items, err} }()
	select {
	case r := <-done:
		return r.items, r.err
	case <-time.After(doctorStoreReadTimeout):
		return nil, s.circuit.trip(fmt.Errorf("doctor bead-store read timed out after %s", doctorStoreReadTimeout))
	}
}

func (s boundedDoctorStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	if err := s.circuit.failure(); err != nil {
		return nil, err
	}
	type result struct {
		items []beads.Bead
		err   error
	}
	done := make(chan result, 1)
	go func() { items, err := s.Store.Ready(q...); done <- result{items, err} }()
	select {
	case r := <-done:
		return r.items, r.err
	case <-time.After(doctorStoreReadTimeout):
		return nil, s.circuit.trip(fmt.Errorf("doctor bead-store read timed out after %s", doctorStoreReadTimeout))
	}
}

func (s boundedDoctorStore) Get(id string) (beads.Bead, error) {
	if err := s.circuit.failure(); err != nil {
		return beads.Bead{}, err
	}
	type result struct {
		item beads.Bead
		err  error
	}
	done := make(chan result, 1)
	go func() { item, err := s.Store.Get(id); done <- result{item, err} }()
	select {
	case r := <-done:
		return r.item, r.err
	case <-time.After(doctorStoreReadTimeout):
		return beads.Bead{}, s.circuit.trip(fmt.Errorf("doctor bead-store read timed out after %s", doctorStoreReadTimeout))
	}
}

func openStoreForCityReadOnlyFastBounded(cityPath string) func(string) (beads.Store, error) {
	open := openStoreForCityReadOnlyFast(cityPath)
	circuit := &doctorStoreCircuit{}
	return func(dirPath string) (beads.Store, error) {
		store, err := open(dirPath)
		if err != nil {
			return nil, err
		}
		return boundedDoctorStore{Store: store, circuit: circuit}, nil
	}
}
