package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// The only store capabilities a v2 effect holds (CONTRACT v5 R3): fencedWriter
// for its own writes, and blindWriteRefusingStore under any legacy helper it
// reuses. Neither ever writes without a resolved conditional writer.

var (
	// errNoConditionalWriter: the row's store resolves no conditional writer
	// (or, for a close, no atomic conditional closer) at the time of the
	// call, and v2 never writes blind (C0.7).
	errNoConditionalWriter = errors.New("v2 effect: the row's store resolves no conditional writer, and v2 never writes blind")
	// errBlindWriteRefused: a method off blindWriteRefusingStore's read
	// allowlist.
	errBlindWriteRefused = errors.New("v2 effect: blind store write refused")
)

// fencedWriter is an effect's write capability over one census leg's store
// (session rows live in work stores too, #5187). Each
// method resolves the store's conditional writer first and refuses with
// errNoConditionalWriter when none resolves, since a store can stop
// resolving after boot's C0.7 check, and the session.Store helpers would
// then write unconditionally (UpdateMetadataFenced's fallback). The helpers
// run over blindWriteRefusingStore besides, so a fallback a helper takes at
// call time (a writer that answers ErrConditionalWriteUnsupported) is
// refused too. It never offers CommitStartedIfCurrent, whose premise admits
// state=asleep (v5 S2). The close verbs also require the atomic conditional
// closer on every call: without it the session.Store closes fall back to a
// blind metadata write and close.
type fencedWriter struct {
	store beads.Store
}

// front is the session front door over the refusing store, or
// errNoConditionalWriter, wrapping any resolution error (a require-mode store
// that cannot fence, a closed store).
func (w fencedWriter) front() (*session.Store, error) {
	switch cw, _, err := beads.ResolveConditionalWriter(w.store); {
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errNoConditionalWriter, err)
	case cw == nil:
		return nil, errNoConditionalWriter
	}
	return sessionFrontDoor(blindWriteRefusingStore{inner: w.store}), nil
}

// closeFront is front for a close verb: it also requires the atomic
// conditional closer.
func (w fencedWriter) closeFront() (*session.Store, error) {
	if _, ok := beads.AtomicConditionalCloserFor(w.store); !ok {
		return nil, errNoConditionalWriter
	}
	return w.front()
}

// updateMetadataFenced is session.Store.UpdateMetadataFenced, fenced: a
// lost CAS re-decides on the fresh row, up to attempts times.
func (w fencedWriter) updateMetadataFenced(id string, attempts int, decide func(session.Info, session.PersistedResponse) session.MetadataPatch) (bool, error) {
	front, err := w.front()
	if err != nil {
		return false, err
	}
	return front.UpdateMetadataFenced(id, attempts, decide)
}

// updateRowFenced is LL4's session.Store.UpdateRowFenced: metadata, title
// and labels in one UpdateIfMatch.
func (w fencedWriter) updateRowFenced(id string, attempts int, decide func(session.Info) (session.RowPatch, bool)) (bool, error) {
	front, err := w.front()
	if err != nil {
		return false, err
	}
	return front.UpdateRowFenced(id, attempts, decide)
}

// casRow is runTx's one CAS attempt: updateMetadataFenced once, reading the
// row from the backing through the leg's cache (CachingStore.RefreshRow), so
// decide never sees a cached copy. A newer write racing the read returns
// beads.ErrRowRefreshFenced, wrapped.
func (w fencedWriter) casRow(id string, decide func(session.Info, session.PersistedResponse) session.MetadataPatch) (bool, error) {
	if _, err := w.front(); err != nil {
		return false, err
	}
	return sessionFrontDoor(w.freshStore()).UpdateMetadataFenced(id, 1, decide)
}

// closeRow is runTx's one close attempt: CloseWithMetadataIfMatch at the
// revision of a row read as casRow reads it, the atomic conditional closer
// required, with no fallback.
func (w fencedWriter) closeRow(id string, decide func(session.Info, session.PersistedResponse) (session.MetadataPatch, bool)) (bool, error) {
	if _, err := w.closeFront(); err != nil {
		return false, err
	}
	return sessionFrontDoor(w.freshStore()).CloseWithMetadataIfMatch(id, decide)
}

// freshStore is the refusing store whose reads of a row go to the backing.
func (w fencedWriter) freshStore() beads.Store {
	if cache, ok := demandLabelKey(w.store).(*beads.CachingStore); ok {
		return freshRowStore{blindWriteRefusingStore: blindWriteRefusingStore{inner: w.store}, cache: cache}
	}
	return blindWriteRefusingStore{inner: w.store}
}

// wroteRow is row as a landed CAS of patch left it: session.Info's fold
// (ApplyPatch), not the blind store write the effect lint bans by that name.
func wroteRow(row session.Info, patch session.MetadataPatch) session.Info {
	return row.ApplyPatch(patch)
}

// freshRowStore is the refusing store whose Get reads the row from the
// backing, through the cache when it is live or partial.
type freshRowStore struct {
	blindWriteRefusingStore
	cache *beads.CachingStore
}

func (s freshRowStore) Get(id string) (beads.Bead, error) {
	b, err := s.cache.RefreshRow(id)
	if errors.Is(err, beads.ErrCacheUnavailable) {
		return s.cache.Backing().Get(id)
	}
	return b, err
}

// closePremise is session.Store.Close: the premise close.
func (w fencedWriter) closePremise(expected session.Info, stateCode string, now time.Time) (bool, error) {
	front, err := w.closeFront()
	if err != nil {
		return false, err
	}
	return front.Close(expected, stateCode, now)
}

// closeWithTerminalPatch is session.Store.CloseWithTerminalPatch.
func (w fencedWriter) closeWithTerminalPatch(expected session.Info, patch session.MetadataPatch, commitMsg string, now time.Time) (bool, error) {
	front, err := w.closeFront()
	if err != nil {
		return false, err
	}
	return front.CloseWithTerminalPatch(expected, patch, commitMsg, now)
}

// rollbackPendingCreate is session.Store.RollbackPendingCreateAtomically.
func (w fencedWriter) rollbackPendingCreate(expected session.Info, closePatch, postClosePatch session.MetadataPatch) (closed, postClosed bool, err error) {
	front, err := w.closeFront()
	if err != nil {
		return false, false, err
	}
	return front.RollbackPendingCreateAtomically(expected, closePatch, postClosePatch)
}

// blindWriteRefusingStore is a beads.Store that forwards a read allowlist
// (Get, List, ListOpen, Ready, Children, ListByLabel, ListByAssignee,
// ListByMetadata, GetLocalString, Ping, DepList) to inner and refuses every
// other method with errBlindWriteRefused (v5 R3). It declares inner as its
// conditional-writes resolution target, so ResolveConditionalWriter and
// AtomicConditionalCloserFor resolve through it exactly as on inner, mode
// source included: fenced writes reach inner's resolved writer, and only the
// unconditional fallbacks are refused.
type blindWriteRefusingStore struct {
	inner beads.Store
}

var (
	_ beads.Store                            = blindWriteRefusingStore{}
	_ beads.ConditionalWritesResolveTargeter = blindWriteRefusingStore{}
	_ beads.ConditionalWriterHandleProvider  = blindWriteRefusingStore{}
)

func (s blindWriteRefusingStore) ConditionalWritesResolveTarget() beads.Store { return s.inner }

// readOnlyStore is what Probes and Calls read through (capReadStores): a
// blind-write-refusing store that, holding only beads.Store, resolves no
// conditional writer either.
type readOnlyStore struct{ beads.Store }

func (s blindWriteRefusingStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterForTarget(s.inner)
}

// The read allowlist.

func (s blindWriteRefusingStore) Get(id string) (beads.Bead, error) { return s.inner.Get(id) }

func (s blindWriteRefusingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	return s.inner.List(q)
}

func (s blindWriteRefusingStore) ListOpen(status ...string) ([]beads.Bead, error) {
	return s.inner.ListOpen(status...)
}

func (s blindWriteRefusingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.inner.Ready(q...)
}

func (s blindWriteRefusingStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.inner.Children(parentID, opts...)
}

func (s blindWriteRefusingStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.inner.ListByLabel(label, limit, opts...)
}

func (s blindWriteRefusingStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	return s.inner.ListByAssignee(assignee, status, limit)
}

func (s blindWriteRefusingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.inner.ListByMetadata(filters, limit, opts...)
}

func (s blindWriteRefusingStore) GetLocalString(id, key string) (string, error) {
	return s.inner.GetLocalString(id, key)
}

func (s blindWriteRefusingStore) Ping() error { return s.inner.Ping() }

func (s blindWriteRefusingStore) DepList(id, direction string) ([]beads.Dep, error) {
	return s.inner.DepList(id, direction)
}

// Everything else is refused.

func (blindWriteRefusingStore) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errBlindWriteRefused
}
func (blindWriteRefusingStore) Update(string, beads.UpdateOpts) error { return errBlindWriteRefused }
func (blindWriteRefusingStore) Close(string) error                    { return errBlindWriteRefused }
func (blindWriteRefusingStore) Reopen(string) error                   { return errBlindWriteRefused }
func (blindWriteRefusingStore) CloseAll([]string, map[string]string) (int, error) {
	return 0, errBlindWriteRefused
}
func (blindWriteRefusingStore) SetMetadata(string, string, string) error { return errBlindWriteRefused }
func (blindWriteRefusingStore) SetMetadataBatch(string, map[string]string) error {
	return errBlindWriteRefused
}

func (blindWriteRefusingStore) SetLocalString(string, string, string) error {
	return errBlindWriteRefused
}
func (blindWriteRefusingStore) Tx(string, func(beads.Tx) error) error { return errBlindWriteRefused }
func (blindWriteRefusingStore) Delete(string) error                   { return errBlindWriteRefused }
func (blindWriteRefusingStore) DepAdd(string, string, string) error   { return errBlindWriteRefused }
func (blindWriteRefusingStore) DepRemove(string, string) error        { return errBlindWriteRefused }
