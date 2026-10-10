package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Decided is a row as a decision read it, and the registered site (factsSites)
// that names the facts the decision rests on. A destructive act or a write
// made on it is checked against those facts on a fresh read (Match), so a
// decision taken on a stale snapshot never executes. Decide mints it; a zero
// Decided matches no row.
type Decided struct {
	info Info
	site FactsSite
}

// Decide is the decision on info, resting on site's facts. info is the row
// as the deciding read projected it (a tick's snapshot, folded with the
// tick's own writes).
func Decide(info Info, site FactsSite) Decided { return Decided{info: info, site: site} }

// Info is the row as d was decided on.
func (d Decided) Info() Info { return d.info }

// Match reports whether fresh, read after d, is d's open row and carries the
// facts d was decided on, as d's site compares them: each compared key's
// projected Info fields are equal. A site of a Decided compares only keys
// Info projects (TestDecidedSitesCompareProjectedKeys).
func (d Decided) Match(fresh Info) bool {
	e := factsSites[d.site]
	switch {
	case d.info.ID == "" || fresh.ID != d.info.ID || fresh.Closed:
		return false
	case len(e.States) > 0 && !slices.Contains(e.States, strings.TrimSpace(fresh.MetadataState)):
		return false
	case e.NoWake && liveMetadataState(fresh.MetadataState) && !liveMetadataState(d.info.MetadataState):
		return false
	}
	have, want := reflect.ValueOf(fresh), reflect.ValueOf(d.info)
	for _, k := range factKeys[:min(len(factKeys), numFactKeys)] {
		if slices.Contains(e.Drop, k) || len(e.Only) > 0 && !slices.Contains(e.Only, k) {
			continue
		}
		for _, f := range infoKeyFields[k] {
			if !reflect.DeepEqual(have.Field(f).Interface(), want.Field(f).Interface()) {
				return false
			}
		}
	}
	return true
}

// infoKeyFields are, per metadata key, the indexes of the Info fields its
// codec setter writes.
var infoKeyFields = func() map[string][]int {
	out := make(map[string][]int, len(infoKeyCodec))
	for _, spec := range infoKeyCodec {
		for _, v := range []string{"true", "2006-01-02T15:04:05Z"} {
			var info Info
			spec.set(&info, v)
			rv := reflect.ValueOf(info)
			for f := 0; f < rv.NumField(); f++ {
				if !rv.Field(f).IsZero() && !slices.Contains(out[spec.key], f) {
					out[spec.key] = append(out[spec.key], f)
				}
			}
		}
	}
	return out
}()

// liveMetadataState reports whether a row's raw state claims a live or
// starting runtime.
func liveMetadataState(state string) bool {
	switch State(strings.TrimSpace(state)) {
	case StateActive, StateAwake, StateCreating, StateStartPending:
		return true
	}
	return false
}

// CommitResult is how a Commit ended.
type CommitResult uint8

// The Commit results.
const (
	// CommitLanded wrote the patch.
	CommitLanded CommitResult = iota + 1
	// CommitMoved refused: the row no longer carries the decision's facts.
	// The decision is made again on a later read.
	CommitMoved
	// CommitClosed refused: the row is closed.
	CommitClosed
	// CommitContended refused: the conditional write lost every attempt.
	CommitContended
)

func (r CommitResult) String() string {
	if names := []string{"", "landed", "moved", "closed", "contended"}; int(r) < len(names) {
		return names[r]
	}
	return fmt.Sprintf("CommitResult(%d)", r)
}

// Commit writes patch to d's row while the row still carries the facts d was
// decided on: each attempt reads the row live (the backing store, never a
// cache: a move another process wrote is seen whatever refreshed the cache),
// matches it, and writes fenced at that read's revision, up to three
// attempts. A store without conditional writes is read, matched and written,
// with the window between the read and the write left open.
func (s *Store) Commit(d Decided, patch MetadataPatch) (CommitResult, error) {
	if d.info.ID == "" {
		return 0, errors.New("session: commit of a zero decision")
	}
	return s.commitIf(d.info.ID, patch, nil, startCommitMaxAttempts, s.freshBead, func(b beads.Bead) bool {
		return d.Match(infoFromPersistedBead(b))
	})
}

// Holds reads d's row fresh and reports whether it still carries the facts d
// was decided on.
func (s *Store) Holds(d Decided) (bool, error) {
	if d.info.ID == "" {
		return false, nil
	}
	b, err := s.freshBead(d.info.ID)
	if err != nil {
		return false, err
	}
	return b.Status != "closed" && d.Match(infoFromPersistedBead(b)), nil
}

type killDecidedCtxKey struct{}

// ErrKillPremiseMoved refuses a kill whose row, read fresh under the runtime
// lease, no longer carries the facts the kill was decided on.
var ErrKillPremiseMoved = errors.New("runtime lease: the row moved since the kill was decided")

// WithKillDecided makes a Manager kill under ctx decide again under the
// runtime lease: once it holds the lease it reads the row fresh, and stops
// only while the row still carries d's facts. It cannot see a move that writes
// nothing d's site compares: a live runtime restarted in place at the same
// generation and token is told apart only by the kill's exact-object fence.
func WithKillDecided(ctx context.Context, d Decided) context.Context {
	return context.WithValue(ctx, killDecidedCtxKey{}, d)
}

// killPremiseHolds checks ctx's kill decision, if any, on a fresh read of id.
func (m *Manager) killPremiseHolds(ctx context.Context, id string) error {
	d, ok := ctx.Value(killDecidedCtxKey{}).(Decided)
	if !ok {
		return nil
	}
	if d.info.ID != id {
		return fmt.Errorf("%w: session %q: the kill was decided on session %q", ErrKillPremiseMoved, id, d.info.ID)
	}
	holds, err := NewStore(beads.SessionStore{Store: m.store}).Holds(d)
	if err != nil {
		return err
	}
	if !holds {
		return fmt.Errorf("%w: session %q", ErrKillPremiseMoved, id)
	}
	return nil
}
