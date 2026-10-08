package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The fallback Store.Tx route for a backing store that has no transaction.
//
// Store.Tx's contract is "every write in this callback lands together", and the
// native route delivers it with one Dolt transaction. A store reached over the
// v0 wire has none to open: RunInTransaction is off the served surface and
// refuses with a typed *beadslib.ErrUnsupported before the callback is ever
// invoked. Without a second route every reconciler close site — the
// metadata-then-close pair that stamps a session's terminal state and then
// retires it — would fail outright on a served city.
//
// The second route is issueops.BatchApplier: ONE request whose items are
// applied inside one transaction the SERVER opens, all or nothing. So the
// callback is not executed against a live transaction at all; it is RECORDED
// into items, and the recording is dialed once when the callback returns
// cleanly. That inversion is what buys three properties the native route does
// not have on this backend:
//
//   - atomic, because the server's transaction spans the whole item list;
//   - reason-preserving, because a close item CARRIES its reason, where the
//     native tx.CloseIssue had to read it back out of the row;
//   - nothing-on-failure, because a callback that errors has dialed no request.
//
// WHAT IT CANNOT RECORD, it refuses by name rather than approximating:
//
//   - CREATE. A create inside the callback must hand back a minted id
//     immediately — extmsg's bind reads it to key the transcript membership it
//     creates in the same callback — and an undialed batch has no id to give.
//     The id arrives in ApplyBatchResult.Keys, which is a caller-shaped answer,
//     so the sites that create inside a Tx are the ones that have to be
//     rewritten against BatchApplier directly. (ga-8tiw9)
//   - REPARENT. issueops.UpdateItem carries Patch.ParentID and the in-process
//     appliers honor it, but the http client's apply encoder refuses that
//     member (W-ApplyPatch.ParentID): apply publishes no parent_id even though
//     updateIssue does. Refusing here names the member; letting it through
//     would surface the encoder's refusal as an opaque transport error at the
//     one call site — an Update carrying ParentID — that can explain it.
//
// READS ARE NOT IN THE TRANSACTION, and that is the one property lost relative
// to the native route. The close reason and the already-closed check are read
// before the batch is dialed, so a row that changes in the gap is a row this
// route describes with a slightly stale reason. Every caller of this shape
// already re-reads and retries on the next reconciler tick, and the alternative
// — dialing a read per item inside a request that has not been built yet — is
// not available on a batch API.

// errBatchTxUnrecordable reports a Store.Tx callback that asked for something
// the batch route cannot express. It is its own sentinel so a caller can tell
// "this backend cannot do transactions at all" from "this backend cannot do
// THIS write in a transaction".
var errBatchTxUnrecordable = errors.New("beads tx: unrecordable in a batch-applied transaction")

// runTxAsBatch replays fn's writes as one issueops.BatchApplier request. It is
// called only after the native transaction refused as unsupported.
func (s *NativeDoltStore) runTxAsBatch(ctx context.Context, storage beadslib.Storage, commitMsg string, fn func(Tx) error) error {
	applier, err := storage.BatchApplier()
	if err != nil {
		return fmt.Errorf("beads tx %q: this store has neither transactions nor a batch applier: %w", commitMsg, err)
	}
	recorder := &nativeBatchTx{store: s, ctx: ctx, storage: storage}
	if err := fn(recorder); err != nil {
		return err
	}
	if recorder.err != nil {
		return recorder.err
	}
	if len(recorder.items) == 0 {
		return nil
	}
	if _, err := applier.ApplyBatch(ctx, issueops.ApplyBatchRequest{
		Actor: s.actor,
		Items: recorder.items,
		// Provenance labels the history entry the way the native route's
		// commit message did. The batch opens its own transaction and takes no
		// commit message, so this is where a gc-authored label survives.
		Provenance: commitMsg,
	}); err != nil {
		return fmt.Errorf("beads tx %q: %w", commitMsg, err)
	}
	return nil
}

// nativeBatchTx records a Store.Tx callback's writes as issueops apply items.
type nativeBatchTx struct {
	store   *NativeDoltStore
	ctx     context.Context
	storage beadslib.Storage
	items   []issueops.ApplyItem
	// staged is the metadata this batch has already written per id. A close
	// item reads its reason from here first, because the update that stamped
	// it has not committed and cannot be read back.
	staged map[string]map[string]string
	err    error
}

var _ Tx = (*nativeBatchTx)(nil)

// Create refuses: see the file comment. The callback needs an id this route
// cannot mint.
func (t *nativeBatchTx) Create(Bead) (Bead, error) {
	t.err = fmt.Errorf("%w: create needs a minted id before the batch is dialed; rewrite this site against issueops.BatchApplier and read the id from ApplyBatchResult.Keys", errBatchTxUnrecordable)
	return Bead{}, t.err
}

// Update records one patch. A parent change is refused by name.
//
// The item waives exactly the guard the native route's tx.UpdateIssue never
// applied. That raw update has no anti-steal fence, so an assignee edit forces
// the transfer here too; the role refuses that force without an assignee edit,
// so it stays conditioned on one. The same raw update DOES enforce the close
// policy, so ForceClosePolicy stays unset and a done-crossing status write is
// refused on both routes alike (see the Update doc comment). This is not
// updateThroughBatch, which waives both guards because the storage-layer write
// the standalone Store.Update replaced applied neither.
func (t *nativeBatchTx) Update(id string, opts UpdateOpts) error {
	if opts.ParentID != nil {
		t.err = fmt.Errorf("%w: the apply patch publishes no parent_id (W-ApplyPatch.ParentID), so a reparent cannot ride a batch; reparent through Store.Update, which routes to updateIssue", errBatchTxUnrecordable)
		return t.err
	}
	patch, err := nativeIssuePatchFromUpdateOpts(opts)
	if err != nil {
		t.err = err
		return err
	}
	if nativeIssuePatchIsEmpty(patch) {
		return nil
	}
	t.stage(id, opts.Metadata)
	t.items = append(t.items, issueops.ApplyItem{
		Kind: issueops.ItemUpdate,
		Update: &issueops.UpdateItem{
			Target:                issueops.Ref{ID: id},
			Patch:                 patch,
			ForceAssigneeTransfer: opts.Assignee != nil,
		},
	})
	return nil
}

// SetMetadataBatch records a metadata merge. The role's metadata patch merges
// rather than replaces, so unlike the native route this needs no read-back.
func (t *nativeBatchTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	return t.Update(id, UpdateOpts{Metadata: kvs})
}

// Close records one close, carrying the reason this batch staged or the row
// already holds. An already-closed row records nothing, matching
// applyCloseInTx; a missing one is ErrNotFound, also matching it.
func (t *nativeBatchTx) Close(id string) error {
	current, err := t.storage.GetIssue(t.ctx, id)
	if err != nil {
		t.err = nativeStoreError(id, err)
		return t.err
	}
	if current == nil {
		t.err = fmt.Errorf("bead %q: %w", id, ErrNotFound)
		return t.err
	}
	if current.Status == beadslib.StatusClosed {
		return nil
	}
	t.items = append(t.items, issueops.ApplyItem{
		Kind: issueops.ItemClose,
		Close: &issueops.CloseItem{
			Target: issueops.Ref{ID: id},
			Reason: t.closeReason(id, current),
			// Force mirrors the native route: applyCloseInTx wrote through
			// tx.CloseIssue, which applies no close policy, and a molecule root
			// routinely closes over open children.
			Force: true,
		},
	})
	return nil
}

// closeReason answers with the reason the batch itself staged, falling back to
// the one the row already carries. The staged value wins because it is the one
// the committed row will hold.
func (t *nativeBatchTx) closeReason(id string, current *beadslib.Issue) string {
	if staged, ok := t.staged[id]; ok {
		if reason, ok := staged["close_reason"]; ok {
			return strings.TrimSpace(reason)
		}
	}
	return nativeCloseReasonFromIssue(current)
}

func (t *nativeBatchTx) stage(id string, metadata map[string]string) {
	if len(metadata) == 0 {
		return
	}
	if t.staged == nil {
		t.staged = make(map[string]map[string]string, 1)
	}
	if t.staged[id] == nil {
		t.staged[id] = make(map[string]string, len(metadata))
	}
	for key, value := range metadata {
		t.staged[id][key] = value
	}
}

// nativeIssuePatchIsEmpty reports a patch that asks for nothing. The served
// wire refuses an empty patch on both doors (beads internal/httpapi update.go
// and batch_apply.go), where the in-process role answers one with a row read
// and no write. Callers reach one legitimately: rollbackPendingCreateClears
// passes Store.Tx an empty post-close clear map when the session name was not
// explicit, and a pure foreign reparent leaves Update's facade patch empty.
func nativeIssuePatchIsEmpty(patch issueops.IssuePatch) bool {
	return !patch.Title.Set &&
		!patch.Status.Set &&
		!patch.IssueType.Set &&
		!patch.Priority.Set &&
		!patch.Description.Set &&
		!patch.Assignee.Set &&
		!patch.ParentID.Set &&
		len(patch.Labels.Add) == 0 &&
		len(patch.Labels.Remove) == 0 &&
		len(patch.Metadata.Set) == 0
}
