package beads

import (
	"fmt"
	"log"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// RouteNormalizerFunc normalizes a gc.routed_to target string for
// genuine-reroute comparison, so that two spellings of the same destination
// (e.g. a pool-slot-suffixed alias and its base name) do not look like a
// different target that would otherwise be (wrongly) treated as a genuine
// reroute.
type RouteNormalizerFunc func(string) string

// routeChangeClearingStore is a Store decorator that clears the three
// executor-identity stamps (gc.session_name, gc.work_dir, and the legacy
// work_dir key) on a bead whenever a write gives it a genuine (normalized-
// different) new gc.routed_to value. When the rerouted bead is a molecule
// step -- it carries gc.root_bead_id -- the same stamps are also cleared on
// its molecule root, since stampRunRootFromStep mirrors them there under a
// different bead ID that a per-bead clear would not otherwise reach (design
// ga-cm2o5t.1 sec 6 / Risk R6).
//
// The routing write is always delegated to the backing store first; stamps
// are cleared only once that write has actually succeeded (design sec 2).
// The clear itself is a single SetMetadataBatch call straight to the backing
// store, bypassing this decorator, so it can never recursively re-trigger
// the gate.
type routeChangeClearingStore struct {
	Store
	normalizer RouteNormalizerFunc
}

// WithRouteChangeClearing wraps store so that a genuine change to
// beadmeta.RoutedToMetadataKey clears the rerouted bead's executor-identity
// stamps -- and its molecule root's mirrored copies, if any -- once the
// routing write itself has succeeded. normalizer decides whether an old/new
// target pair is a genuine change (e.g. collapsing pool-slot suffixes so
// "worker" -> "worker-2" is a no-op, per FR-2 / ga-79uuwq).
func WithRouteChangeClearing(store Store, normalizer RouteNormalizerFunc) Store {
	return &routeChangeClearingStore{Store: store, normalizer: normalizer}
}

var (
	_ ConditionalWritesResolveTargeter = (*routeChangeClearingStore)(nil)
	_ DepMetadataReader                = (*routeChangeClearingStore)(nil)
	_ ConditionalAssignmentReleaser    = (*routeChangeClearingStore)(nil)
)

// ConditionalWritesResolveTarget declares the immediate backing store as the
// resolution target, per the ConditionalWritesResolveTargeter contract
// (internal/beads/conditional_writes_resolve.go): without this, any resolve
// through this decorator would silently collapse to unset/legacy.
func (w *routeChangeClearingStore) ConditionalWritesResolveTarget() Store {
	return w.Store
}

// enrichReadyProjectionForCache forwards the backing store's ready-projection
// enrichment (readyProjectionEnrichmentStore, caching_store.go) through this
// decorator. Wrapping a Store in an interface-embedding struct only promotes
// methods declared on the Store interface itself; an unexported optional
// capability like this one -- satisfied by *BdStore but not part of Store --
// becomes invisible to a type assertion against the wrapper unless
// re-declared here, exactly as beadPolicyStore forwards its own optional
// capabilities in cmd/gc/bead_policy_store.go. Without this forward, a
// CachingStore built over this decorator silently skips enrichment (no
// error, no degrade latch), which lets CachedReady answer a control-scoped
// query from the plain IsReadyCandidate filter instead of correctly
// declining to a live fallback for control/infrastructure beads.
func (w *routeChangeClearingStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	if backing, ok := w.Store.(readyProjectionEnrichmentStore); ok {
		return backing.enrichReadyProjectionForCache(items)
	}
	return items, nil
}

// DepMetadata forwards the backing store's edge-payload read
// (DepMetadataReader). This decorator does not touch dependency edges, so
// the answer passes through untouched -- but the interface is optional and
// the embedded Store field does not promote it, exactly like
// enrichReadyProjectionForCache above and beadPolicyStore.DepMetadata
// (cmd/gc/bead_policy_store.go) beside it. Returning an error rather than
// ("", false, nil) when the backing store lacks the read matters: the
// infra-class migration (infraSourceEdgePayloadRefusal) treats an
// unanswerable source as unsafe to copy from, and collapsing "cannot be
// asked" into "carries nothing" is exactly the conflation that let it drop
// edge payloads silently before that check existed.
func (w *routeChangeClearingStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	reader, ok := w.Store.(DepMetadataReader)
	if !ok {
		return "", false, fmt.Errorf("reading dependency metadata %s -> %s: route-clearing-wrapped store %T exposes no edge-payload read", issueID, dependsOnID, w.Store)
	}
	return reader.DepMetadata(issueID, dependsOnID)
}

// ReleaseIfCurrent forwards the backing store's conditional assignment
// release (ConditionalAssignmentReleaser). Forwarded explicitly for the same
// reason as DepMetadata above: the embedded Store field does not promote
// optional capabilities. A backing store without the release returns
// ErrConditionalReleaseUnsupported, matching every other wrapper in the tree
// (CachingStore.ReleaseIfCurrent, beadPolicyStore.ReleaseIfCurrent).
func (w *routeChangeClearingStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := w.Store.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
}

// SetMetadata clears the rerouted bead's (and its molecule root's) executor-
// identity stamps when key is beadmeta.RoutedToMetadataKey and the write is
// a genuine reroute, after delegating the write itself to the backing store.
func (w *routeChangeClearingStore) SetMetadata(id, key, value string) error {
	if key != beadmeta.RoutedToMetadataKey {
		return w.Store.SetMetadata(id, key, value)
	}

	before, beforeErr := w.Get(id)

	if err := w.Store.SetMetadata(id, key, value); err != nil {
		return err
	}

	if beforeErr == nil {
		w.clearIfGenuine(id, before, before.Metadata[beadmeta.RoutedToMetadataKey], value)
	}
	return nil
}

// SetMetadataBatch clears the rerouted bead's (and its molecule root's)
// executor-identity stamps when kv contains a genuine reroute of
// beadmeta.RoutedToMetadataKey, after delegating the batch write itself to
// the backing store.
func (w *routeChangeClearingStore) SetMetadataBatch(id string, kv map[string]string) error {
	newTarget, changesRoute := kv[beadmeta.RoutedToMetadataKey]
	if !changesRoute {
		return w.Store.SetMetadataBatch(id, kv)
	}

	before, beforeErr := w.Get(id)

	if err := w.Store.SetMetadataBatch(id, kv); err != nil {
		return err
	}

	if beforeErr == nil {
		w.clearIfGenuine(id, before, before.Metadata[beadmeta.RoutedToMetadataKey], newTarget)
	}
	return nil
}

// Update clears the rerouted bead's (and its molecule root's) executor-
// identity stamps when opts.Metadata contains a genuine reroute of
// beadmeta.RoutedToMetadataKey, after delegating the update itself to the
// backing store.
func (w *routeChangeClearingStore) Update(id string, opts UpdateOpts) error {
	newTarget, changesRoute := opts.Metadata[beadmeta.RoutedToMetadataKey]
	if !changesRoute {
		return w.Store.Update(id, opts)
	}

	before, beforeErr := w.Get(id)

	if err := w.Store.Update(id, opts); err != nil {
		return err
	}

	if beforeErr == nil {
		w.clearIfGenuine(id, before, before.Metadata[beadmeta.RoutedToMetadataKey], newTarget)
	}
	return nil
}

// clearIfGenuine clears the executor-identity stamps on bead id -- and, when
// before carries a molecule root distinct from id, on that root too -- if
// oldTarget and newTarget normalize to different values. before is the
// bead's pre-write state, read to recover its prior gc.routed_to and
// gc.root_bead_id before the routing write overwrote them.
//
// A clear failure is logged and swallowed, never returned: the routing write
// this decorator delegates to has already succeeded by the time
// clearIfGenuine runs, and that write must never be reported as failed
// because a best-effort follow-up cleanup was rejected (design sec 1 NFR-5).
func (w *routeChangeClearingStore) clearIfGenuine(id string, before Bead, oldTarget, newTarget string) {
	if w.normalizer(oldTarget) == w.normalizer(newTarget) {
		return
	}

	clearStamps := map[string]string{
		beadmeta.SessionNameMetadataKey:   "",
		beadmeta.WorkDirMetadataKey:       "",
		beadmeta.LegacyWorkDirMetadataKey: "",
	}
	if err := w.Store.SetMetadataBatch(id, clearStamps); err != nil {
		log.Printf("beads route-clear: clearing executor-identity stamps on %s after reroute to %q: %v", id, newTarget, err)
	}

	if rootID := before.Metadata[beadmeta.RootBeadIDMetadataKey]; rootID != "" && rootID != id {
		if err := w.Store.SetMetadataBatch(rootID, clearStamps); err != nil {
			log.Printf("beads route-clear: clearing executor-identity stamps on molecule root %s (step %s rerouted to %q): %v", rootID, id, newTarget, err)
		}
	}
}
