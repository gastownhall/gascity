package sqlite

// The serving half of the SQLite Beads provider: how a planned binding becomes
// the beads.Store the class front doors read and write through.
//
// The database and the id prefix are not chosen here. GraphPath resolves the
// one file this provider's whole lifecycle already inspects, fences and opens,
// and the reserved graph prefix comes from the shared prefix registry. Both are
// taken from the same source the storage-class migration takes them from, so a
// binding cannot be migrated into one database and served from another — a
// drift that would report a healthy cutover into a file nothing ever reads.

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storebinding"
)

var (
	_ storebinding.EngineOpener   = (*beadsProvider)(nil)
	_ storebinding.BindingLocator = (*beadsProvider)(nil)
)

// BindingLocation reports the database this binding serves from — the same
// file GraphPath resolved for the whole lifecycle, so what a city records as
// its served location is the file it actually opens.
func (p *beadsProvider) BindingLocation(spec storebinding.BindingSpec) (string, error) {
	if err := p.boundTo(spec); err != nil {
		return "", err
	}
	return p.path, nil
}

// storeCloser adapts a bead store's own close to io.Closer.
//
// beads.Store already has a Close method with a different meaning — closing one
// bead, not the store — so an engine handle cannot satisfy io.Closer directly.
// Adapting here rather than widening the seam keeps EngineOpener's contract the
// ordinary one every caller already knows how to spell.
type storeCloser struct {
	store interface{ CloseStore() error }
}

// Close releases the engine's durable resources.
func (c storeCloser) Close() error { return c.store.CloseStore() }

// OpenEngine opens this binding's Beads engine for the classes it serves.
//
// The classes are checked against what one Beads ledger can serve rather than
// trusted: an assignment this provider cannot honor must fail at the open, not
// at the first read of a class nobody projected.
//
// The store comes back fenced to the namespaces those classes hold — the
// pinned-id contract in engdocs/architecture/beads.md, invariant 16.
func (p *beadsProvider) OpenEngine(spec storebinding.BindingSpec, classes storebinding.ClassSet) (beads.Store, io.Closer, error) {
	if err := p.boundTo(spec); err != nil {
		return nil, nil, err
	}
	served, err := beadsClasses()
	if err != nil {
		return nil, nil, err
	}
	if classes.Empty() {
		return nil, nil, fmt.Errorf("%w: binding %q opens for no class", ErrInvalidBeadsBinding, p.spec.Name)
	}
	if !served.Contains(classes) {
		return nil, nil, fmt.Errorf("%w: binding %q is assigned classes this provider does not serve", ErrInvalidBeadsBinding, p.spec.Name)
	}
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || prefix == "" {
		return nil, nil, fmt.Errorf("%w: no reserved id prefix is registered for the %q class", ErrInvalidBeadsBinding, config.BeadClassGraph)
	}
	store, err := beads.OpenSQLiteStore(filepath.Dir(p.path),
		beads.WithSQLiteStoreIDPrefix(prefix),
		beads.WithSQLiteStoreReservedIDPrefixes(storebinding.EngineReservedPrefixes(classes)...),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the SQLite Beads engine of binding %q at %s: %w", p.spec.Name, p.path, err)
	}
	closer, ok := store.(interface{ CloseStore() error })
	if !ok {
		return nil, nil, fmt.Errorf("%w: the SQLite Beads engine of binding %q cannot be closed", ErrInvalidBeadsBinding, p.spec.Name)
	}
	return store, storeCloser{closer}, nil
}
