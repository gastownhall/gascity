package main

import "github.com/gastownhall/gascity/internal/beads"

type unavailableStore struct {
	err error
}

func (s unavailableStore) Create(beads.Bead) (beads.Bead, error)             { return beads.Bead{}, s.err }
func (s unavailableStore) Get(string) (beads.Bead, error)                    { return beads.Bead{}, s.err }
func (s unavailableStore) Update(string, beads.UpdateOpts) error             { return s.err }
func (s unavailableStore) Close(string) error                                { return s.err }
func (s unavailableStore) Reopen(string) error                               { return s.err }
func (s unavailableStore) CloseAll([]string, map[string]string) (int, error) { return 0, s.err }
func (s unavailableStore) List(beads.ListQuery) ([]beads.Bead, error)        { return nil, s.err }
func (s unavailableStore) ListOpen(...string) ([]beads.Bead, error)          { return nil, s.err }
func (s unavailableStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error)   { return nil, s.err }
func (s unavailableStore) Children(string, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func (s unavailableStore) ListByLabel(string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func (s unavailableStore) ListByAssignee(string, string, int) ([]beads.Bead, error) {
	return nil, s.err
}

func (s unavailableStore) ListByMetadata(map[string]string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}
func (s unavailableStore) SetMetadata(string, string, string) error         { return s.err }
func (s unavailableStore) SetMetadataBatch(string, map[string]string) error { return s.err }
func (s unavailableStore) Delete(string) error                              { return s.err }
func (s unavailableStore) Ping() error                                      { return s.err }
func (s unavailableStore) DepAdd(string, string, string) error              { return s.err }
func (s unavailableStore) DepRemove(string, string) error                   { return s.err }
func (s unavailableStore) DepList(string, string) ([]beads.Dep, error)      { return nil, s.err }

var _ beads.Store = unavailableStore{}
