package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Writes through the guarded rig store. See rig_namespace_store.go for the
// rules; this file applies them.

// ---------------------------------------------------------------------------
// Refusals and row fencing.

func (s *rigNamespaceStore) refuse(op, id, reason string, err error) error {
	return &rigNamespaceError{
		Rig:       s.ns.rigName(),
		Namespace: s.ns.namespace(),
		Op:        op,
		ID:        id,
		Reason:    reason,
		Err:       err,
	}
}

// outsideReason is the refusal reason for an identity outside the
// namespace.
func (s *rigNamespaceStore) outsideReason(what, value string) string {
	ns := s.ns.namespace()
	if ns == "" {
		return fmt.Sprintf("%s %q: the rig's namespace admits nothing (its store is ambiguous or the rig has no usable name)", what, value)
	}
	return fmt.Sprintf("%s %q is outside %s and is not one of this city's pool sessions of the rig", what, value, ns)
}

// refuseForeignRow is the refusal of a write to a row classified Foreign.
func (s *rigNamespaceStore) refuseForeignRow(op, id string, c rowClassification) error {
	var reason string
	switch {
	case c.Key != "" && c.Rule == rowRuleOwnerKeys:
		reason = s.outsideReason("its owner ("+c.Key+")", c.Value)
	case c.Key != "" && c.Rule == rowRuleClaimStamps:
		reason = fmt.Sprintf("no claim stamp on it (%s %q) names this city's own session of the rig", c.Key, c.Value)
	case c.Rule == rowRuleCreationStamp:
		reason = fmt.Sprintf("another namespace created it (%s %q)", c.Key, c.Value)
	default:
		reason = "the row belongs to another city"
	}
	return s.refuse(op, id, reason, c.Err)
}

// liveRow reads id from the inner store's live handle: a cache answers from
// its backing, so a write is decided on the stored row.
func (s *rigNamespaceStore) liveRow(id string) (beads.Bead, error) {
	return beads.HandlesFor(s.inner).Live.Get(id)
}

// fenceRow reads id live and refuses a write to it when the row is Foreign.
// A missing row is returned as the read's error (it wraps beads.ErrNotFound),
// which the write would have returned too.
func (s *rigNamespaceStore) fenceRow(op, id string) (beads.Bead, rowClassification, error) {
	b, err := s.liveRow(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, rowClassification{}, err
		}
		return beads.Bead{}, rowClassification{}, s.refuse(op, id, "its owner could not be read", err)
	}
	c := s.ns.classifyRow(b)
	if c.Class == rowForeign {
		return b, c, s.refuseForeignRow(op, id, c)
	}
	return b, c, nil
}

// fenceRowAt is fenceRow for a write pinned to revision: a row no longer at
// that revision is a *beads.PreconditionFailedError before anything is
// judged or written, so the decision is always about the row the inner write
// would land on.
func (s *rigNamespaceStore) fenceRowAt(op, id string, revision int64) (beads.Bead, rowClassification, error) {
	b, err := s.liveRow(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, rowClassification{}, err
		}
		return beads.Bead{}, rowClassification{}, s.refuse(op, id, "its owner could not be read", err)
	}
	if b.Revision != revision {
		return b, rowClassification{}, &beads.PreconditionFailedError{ID: id, Expected: revision, Current: b.Revision}
	}
	c := s.ns.classifyRow(b)
	if c.Class == rowForeign {
		return b, c, s.refuseForeignRow(op, id, c)
	}
	return b, c, nil
}

// fenceRows fences every id of a batch write, refusing the whole batch when
// any row is Foreign. Missing rows are left to the inner store.
func (s *rigNamespaceStore) fenceRows(op string, ids []string) (map[string]fencedRow, error) {
	rows := make(map[string]fencedRow, len(ids))
	for _, id := range ids {
		b, c, err := s.fenceRow(op, id)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rows[id] = fencedRow{bead: b, class: c}
	}
	return rows, nil
}

type fencedRow struct {
	bead  beads.Bead
	class rowClassification
}

// ---------------------------------------------------------------------------
// Value rules.

// isOwnerStampKey reports whether key names the creation stamp once the
// backend trims it.
func isOwnerStampKey(key string) bool {
	return strings.TrimSpace(key) == rigNamespaceOwnerStampKey
}

func isRigNamespaceAssigneeMetadataKey(key string) bool {
	key = strings.TrimSpace(key)
	for _, k := range rigNamespaceAssigneeMetadataKeys {
		if key == k {
			return true
		}
	}
	return false
}

// checkAssignee refuses setting an unadmitted, non-empty assignee.
func (s *rigNamespaceStore) checkAssignee(op, id, assignee string) error {
	if strings.TrimSpace(assignee) == "" {
		return nil
	}
	a := s.ns.admission(assignee)
	if a.Admitted() {
		return nil
	}
	return s.refuse(op, id, s.outsideReason("assignee", assignee), a.Err)
}

// checkMetadataWrite applies the value rules to a metadata write on a row
// already known not to be Foreign (class is its classification; row the
// stored row, for the creation stamp's byte comparison).
func (s *rigNamespaceStore) checkMetadataWrite(op, id string, row beads.Bead, class rowClass, set map[string]string) error {
	return s.checkMetadataWriteAfter(op, id, row, class, row.Assignee, set, nil)
}

// rowHasOwnersAfter reports whether row, once the write sets set, unsets
// unset and leaves assignee as its assignee, carries an owner key: its class
// is then decided by its owner keys, and a claim stamp on it decides nothing.
func rowHasOwnersAfter(row beads.Bead, assignee string, set map[string]string, unset []string) bool {
	post := row
	post.Assignee = assignee
	post.Metadata = make(map[string]string, len(row.Metadata)+len(set))
	for k, v := range row.Metadata {
		post.Metadata[k] = v
	}
	for k, v := range set {
		post.Metadata[strings.TrimSpace(k)] = v
	}
	for _, k := range unset {
		delete(post.Metadata, strings.TrimSpace(k))
	}
	return len(rowOwnerValues(post)) > 0
}

// checkMetadataWriteAfter is checkMetadataWrite for a write that leaves the
// row's assignee as assigneeAfter.
func (s *rigNamespaceStore) checkMetadataWriteAfter(op, id string, row beads.Bead, class rowClass, assigneeAfter string, set map[string]string, unset []string) error {
	ownersAfter := rowHasOwnersAfter(row, assigneeAfter, set, unset)
	for k, v := range set {
		switch {
		case isOwnerStampKey(k):
			stored, has := row.Metadata[rigNamespaceOwnerStampKey]
			if !has || v != stored {
				return s.refuse(op, id, fmt.Sprintf("only the guard writes %s, when it creates a row", rigNamespaceOwnerStampKey), nil)
			}
		case isRigNamespaceRouteKey(k):
			if err := s.checkRouteValue(op, id, k, v, class); err != nil {
				return err
			}
		case isRigNamespaceAssigneeMetadataKey(k):
			// A deferred or stranded assignee is a holder, not a route: it
			// must be admitted on every row, own rows included.
			if err := s.checkAssignee(op, id, v); err != nil {
				return err
			}
		case isRigNamespaceClaimStampKey(k):
			// A claim stamp decides the row's class only when it carries no
			// owner key; on any other row it is bookkeeping, unless it names a
			// pool session: another city's could later hand the row to it.
			if ownersAfter && !config.EmbedsPoolSessionID(v) {
				continue
			}
			if err := s.checkClaimStamp(op, id, k, v); err != nil {
				return err
			}
		case sessionpkg.IsSessionBeadOrRepairable(row):
			if err := s.checkSessionRowOwner(op, id, k, v); err != nil {
				return err
			}
		}
	}
	for _, k := range unset {
		if !isOwnerStampKey(k) {
			continue
		}
		if _, has := row.Metadata[rigNamespaceOwnerStampKey]; has {
			return s.refuse(op, id, fmt.Sprintf("only the guard writes %s, when it creates a row", rigNamespaceOwnerStampKey), nil)
		}
	}
	return nil
}

// isRigNamespaceClaimStampKey reports whether key is a claim stamp (K_S).
func isRigNamespaceClaimStampKey(key string) bool {
	key = strings.TrimSpace(key)
	for _, k := range rigNamespaceClaimStampKeys {
		if key == k {
			return true
		}
	}
	return false
}

// checkClaimStamp refuses a non-empty claim stamp that names no session of
// this city's pools of the rig (the claim-stamp admission the classifier
// applies): stamping another city's session would hand the row to it.
func (s *rigNamespaceStore) checkClaimStamp(op, id, key, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	key = strings.TrimSpace(key)
	a := s.ns.stampAdmission(keyValue{Key: key, Value: value})
	if a.Admitted() {
		return nil
	}
	if s.ns.admitsLexically(value) {
		return s.refuse(op, id, fmt.Sprintf("claim stamp %s %q names no session of this city's agents of the rig", key, value), a.Err)
	}
	return s.refuse(op, id, s.outsideReason("claim stamp "+key, value), a.Err)
}

// checkSessionRowOwner applies the owner rules to a session row's template
// and alias, which name its owner.
func (s *rigNamespaceStore) checkSessionRowOwner(op, id, key, value string) error {
	key = strings.TrimSpace(key)
	if strings.TrimSpace(value) == "" {
		return nil
	}
	switch key {
	case rowSessionTemplateKey:
		if !s.ns.templateInRig(value) {
			return s.refuse(op, id, s.outsideReason("session template", value), nil)
		}
	case "alias":
		return s.checkAssignee(op, id, value)
	}
	return nil
}

// checkRouteValue applies the routing rules: a non-empty route must be
// rig-qualified (a bare route in a shared store is served by every city that
// defines it), and one outside the namespace is a hand-off allowed only on an
// Owned row.
func (s *rigNamespaceStore) checkRouteValue(op, id, key, value string, class rowClass) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if !rigRouteQualified(value) {
		return s.refuse(op, id, fmt.Sprintf("%s %q is not rig-qualified; a bare route in a shared store is served by every city that defines it", key, value), nil)
	}
	return s.checkHandOff(op, id, key, value, class)
}

// checkHandOff allows an owner value outside the namespace only on an Owned
// row: this city may hand its own work off, but may not stamp a foreign
// owner onto a row nobody can be attributed to.
func (s *rigNamespaceStore) checkHandOff(op, id, key, value string, class rowClass) error {
	if strings.TrimSpace(value) == "" || class == rowOwned {
		return nil
	}
	a := s.ns.admission(value)
	if a.Admitted() {
		return nil
	}
	return s.refuse(op, id, s.outsideReason(key, value), a.Err)
}

// checkUpdate applies every rule to an update of a fenced row.
func (s *rigNamespaceStore) checkUpdate(id string, row beads.Bead, class rowClass, opts beads.UpdateOpts) error {
	const op = "update"
	assigneeAfter := row.Assignee
	if opts.Assignee != nil {
		if err := s.checkAssignee(op, id, *opts.Assignee); err != nil {
			return err
		}
		assigneeAfter = *opts.Assignee
	}
	return s.checkMetadataWriteAfter(op, id, row, class, assigneeAfter, opts.Metadata, opts.UnsetMetadata)
}

// stampCreate returns b as the guard creates it: the creation stamp set to
// exactly the namespace (replacing any caller value, under any spelling of
// the key the backend trims to it), after refusing an unadmitted assignee or
// deferred assignee and a bare route.
func (s *rigNamespaceStore) stampCreate(b beads.Bead) (beads.Bead, error) {
	const op = "create"
	stamp, ok := s.ns.ownerStamp()
	if !ok {
		return beads.Bead{}, s.refuse(op, b.ID, "the rig's namespace admits nothing, so no row can be stamped as this city's", nil)
	}
	if err := s.checkAssignee(op, b.ID, b.Assignee); err != nil {
		return beads.Bead{}, err
	}
	meta, err := s.stampMetadata(op, b.ID, b.Metadata, stamp, len(rowOwnerValues(b)) > 0)
	if err != nil {
		return beads.Bead{}, err
	}
	b.Metadata = meta
	if sessionpkg.IsSessionBeadOrRepairable(b) {
		for _, k := range rigNamespaceSessionRowOwnerKeys {
			if err := s.checkSessionRowOwner(op, b.ID, k, b.Metadata[k]); err != nil {
				return beads.Bead{}, err
			}
		}
	}
	return b, nil
}

// stampMetadata checks a create's metadata and returns a copy carrying the
// creation stamp.
func (s *rigNamespaceStore) stampMetadata(op, id string, in map[string]string, stamp string, hasOwners bool) (map[string]string, error) {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		if isOwnerStampKey(k) {
			continue
		}
		switch {
		case isRigNamespaceRouteKey(k):
			// Create-time route rule (one rule, two layers; see "Routes on a
			// new bead" in docs/guides/sharing-a-rig-store.md): the store
			// refuses only a route that is not rig-qualified. A qualified
			// route outside the namespace is accepted, because the new row is
			// stamped as this city's and routing it elsewhere hands this
			// city's own work off. gc bd create and gc sling are stricter by
			// design and refuse such a route before any bd process starts
			// (bdRigNamespaceCheck's create check, sling's
			// rigNamespaceRouteRefusal); do not tighten one layer without
			// the other and the guide.
			if strings.TrimSpace(v) != "" && !rigRouteQualified(v) {
				return nil, s.refuse(op, id, fmt.Sprintf("%s %q is not rig-qualified; a bare route in a shared store is served by every city that defines it", k, v), nil)
			}
		case isRigNamespaceAssigneeMetadataKey(k):
			if err := s.checkAssignee(op, id, v); err != nil {
				return nil, err
			}
		case isRigNamespaceClaimStampKey(k):
			if !hasOwners || config.EmbedsPoolSessionID(v) {
				if err := s.checkClaimStamp(op, id, k, v); err != nil {
					return nil, err
				}
			}
		}
		out[k] = v
	}
	out[rigNamespaceOwnerStampKey] = stamp
	return out, nil
}

// ---------------------------------------------------------------------------
// beads.Store writes.

// Create stamps and creates the row.
func (s *rigNamespaceStore) Create(b beads.Bead) (beads.Bead, error) {
	stamped, err := s.stampCreate(b)
	if err != nil {
		return beads.Bead{}, err
	}
	return s.inner.Create(stamped)
}

// CreateWithStorage stamps and creates the row in the given storage tier. An
// inner store without tiered creates gets the tier on the bead's own flags,
// as the policy layer does.
func (s *rigNamespaceStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	stamped, err := s.stampCreate(b)
	if err != nil {
		return beads.Bead{}, err
	}
	if storer, ok := beads.As[beads.StorageCreateStore](s.inner); ok {
		return storer.CreateWithStorage(stamped, storage)
	}
	return s.inner.Create(applyStorageClass(stamped, storage))
}

// CreateWithForeignID stamps and creates the row under its caller-chosen id.
func (s *rigNamespaceStore) CreateWithForeignID(b beads.Bead) (beads.Bead, error) {
	creator, ok := beads.As[beads.ForeignIDCreator](s.inner)
	if !ok {
		return beads.Bead{}, fmt.Errorf("creating with a foreign id: %T does not support it", s.inner)
	}
	stamped, err := s.stampCreate(b)
	if err != nil {
		return beads.Bead{}, err
	}
	return creator.CreateWithForeignID(stamped)
}

// Update fences the row and the values, then updates it.
func (s *rigNamespaceStore) Update(id string, opts beads.UpdateOpts) error {
	row, c, err := s.fenceRow("update", id)
	if err != nil {
		return err
	}
	if err := s.checkUpdate(id, row, c.Class, opts); err != nil {
		return err
	}
	return s.inner.Update(id, opts)
}

// UpdateReporting is Update reporting the written row (beads.UpdateReporter),
// fenced like Update.
func (s *rigNamespaceStore) UpdateReporting(id string, opts beads.UpdateOpts) (beads.UpdatedRow, error) {
	row, c, err := s.fenceRow("update", id)
	if err != nil {
		return beads.UpdatedRow{}, err
	}
	if err := s.checkUpdate(id, row, c.Class, opts); err != nil {
		return beads.UpdatedRow{}, err
	}
	return beads.UpdateAndReadBack(s.inner, id, opts)
}

// Close fences and closes the row.
func (s *rigNamespaceStore) Close(id string) error {
	if _, _, err := s.fenceRow("close", id); err != nil {
		return err
	}
	return s.inner.Close(id)
}

// Reopen fences and reopens the row.
func (s *rigNamespaceStore) Reopen(id string) error {
	if _, _, err := s.fenceRow("reopen", id); err != nil {
		return err
	}
	return s.inner.Reopen(id)
}

// CloseAll refuses the whole batch when any row is Foreign or the metadata
// breaks a value rule on any row.
func (s *rigNamespaceStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	rows, err := s.fenceRows("close", ids)
	if err != nil {
		return 0, err
	}
	for id, r := range rows {
		if err := s.checkMetadataWrite("close", id, r.bead, r.class.Class, metadata); err != nil {
			return 0, err
		}
	}
	return s.inner.CloseAll(ids, metadata)
}

// Delete fences and deletes the row.
func (s *rigNamespaceStore) Delete(id string) error {
	if _, _, err := s.fenceRow("delete", id); err != nil {
		return err
	}
	return s.inner.Delete(id)
}

// DeleteBatch refuses the whole batch when any row is Foreign.
func (s *rigNamespaceStore) DeleteBatch(ids []string) error {
	deleter, ok := beads.As[beads.BatchDeleter](s.inner)
	if !ok {
		return beads.ErrBatchDeleteUnsupported
	}
	if _, err := s.fenceRows("delete", ids); err != nil {
		return err
	}
	return deleter.DeleteBatch(ids)
}

// SetMetadata fences the row and the value.
func (s *rigNamespaceStore) SetMetadata(id, key, value string) error {
	if err := s.fenceMetadataWrite(id, map[string]string{key: value}); err != nil {
		return err
	}
	return s.inner.SetMetadata(id, key, value)
}

// SetMetadataBatch fences the row and the values.
func (s *rigNamespaceStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.fenceMetadataWrite(id, kvs); err != nil {
		return err
	}
	return s.inner.SetMetadataBatch(id, kvs)
}

func (s *rigNamespaceStore) fenceMetadataWrite(id string, kvs map[string]string) error {
	row, c, err := s.fenceRow("set metadata", id)
	if err != nil {
		return err
	}
	return s.checkMetadataWrite("set metadata", id, row, c.Class, kvs)
}

// DepAdd fences the issue side: the dependency is recorded on it.
func (s *rigNamespaceStore) DepAdd(issueID, dependsOnID, depType string) error {
	if _, _, err := s.fenceRow("add dependency", issueID); err != nil {
		return err
	}
	return s.inner.DepAdd(issueID, dependsOnID, depType)
}

// DepRemove fences the issue side.
func (s *rigNamespaceStore) DepRemove(issueID, dependsOnID string) error {
	if _, _, err := s.fenceRow("remove dependency", issueID); err != nil {
		return err
	}
	return s.inner.DepRemove(issueID, dependsOnID)
}

// DepAddWithMetadata fences the issue side and records the edge with its
// payload. An inner store that cannot carry a payload is an error; a plain
// DepAdd could clear an existing payload.
func (s *rigNamespaceStore) DepAddWithMetadata(issueID, dependsOnID, depType, metadata string) error {
	writer, ok := beads.As[beads.DepMetadataWriter](s.inner)
	if !ok {
		return fmt.Errorf("writing dependency metadata %s -> %s: guarded store over %T cannot carry an edge payload", issueID, dependsOnID, s.inner)
	}
	if _, _, err := s.fenceRow("add dependency", issueID); err != nil {
		return err
	}
	return writer.DepAddWithMetadata(issueID, dependsOnID, depType, metadata)
}

// Tx runs fn against a guarded view of the inner transaction: every create
// is stamped and every write fenced. Rows are read outside the transaction,
// so a row created earlier in the same transaction is judged as this city's
// own creation.
func (s *rigNamespaceStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	return s.inner.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&rigNamespaceTx{s: s, tx: tx, created: map[string]beads.Bead{}})
	})
}

type rigNamespaceTx struct {
	s       *rigNamespaceStore
	tx      beads.Tx
	created map[string]beads.Bead
}

func (t *rigNamespaceTx) Create(b beads.Bead) (beads.Bead, error) {
	stamped, err := t.s.stampCreate(b)
	if err != nil {
		return beads.Bead{}, err
	}
	out, err := t.tx.Create(stamped)
	if err == nil && out.ID != "" {
		row := out
		if row.Metadata == nil {
			row.Metadata = stamped.Metadata
		}
		t.created[out.ID] = row
	}
	return out, err
}

// fence decides a write to id inside the transaction: a row this
// transaction created is this city's own; any other row is fenced from its
// stored state, and one not stored yet is judged as an unattributed row.
func (t *rigNamespaceTx) fence(op, id string) (beads.Bead, rowClass, error) {
	if row, ok := t.created[id]; ok {
		return row, rowOwned, nil
	}
	row, c, err := t.s.fenceRow(op, id)
	if errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, rowUnattributed, nil
	}
	if err != nil {
		return beads.Bead{}, rowForeign, err
	}
	return row, c.Class, nil
}

func (t *rigNamespaceTx) Update(id string, opts beads.UpdateOpts) error {
	row, class, err := t.fence("update", id)
	if err != nil {
		return err
	}
	if err := t.s.checkUpdate(id, row, class, opts); err != nil {
		return err
	}
	return t.tx.Update(id, opts)
}

func (t *rigNamespaceTx) SetMetadataBatch(id string, kvs map[string]string) error {
	row, class, err := t.fence("set metadata", id)
	if err != nil {
		return err
	}
	if err := t.s.checkMetadataWrite("set metadata", id, row, class, kvs); err != nil {
		return err
	}
	return t.tx.SetMetadataBatch(id, kvs)
}

func (t *rigNamespaceTx) Close(id string) error {
	if _, _, err := t.fence("close", id); err != nil {
		return err
	}
	return t.tx.Close(id)
}

var (
	_ beads.ConditionalAssignmentReleaser = (*rigNamespaceStore)(nil)
	_ beads.ConditionalAssigneeTransferer = (*rigNamespaceStore)(nil)
	_ beads.AssignmentGuardedUpdater      = (*rigNamespaceStore)(nil)
	_ interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	} = (*rigNamespaceStore)(nil)
)

// ---------------------------------------------------------------------------
// Claim-shaped conditional writes. Each is decided on the holder it names
// before the row is read: a holder outside the namespace means the row is
// Foreign whatever else it carries.

// ReleaseIfCurrent releases the row only from an admitted holder on a row
// that is not Foreign.
func (s *rigNamespaceStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := beads.As[beads.ConditionalAssignmentReleaser](s.inner)
	if !ok {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	if err := s.checkAssignee("release", id, expectedAssignee); err != nil {
		return false, err
	}
	if _, _, err := s.fenceRow("release", id); err != nil {
		return false, err
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
}

// TransferIfCurrent moves the row between two admitted holders on a row that
// is not Foreign.
func (s *rigNamespaceStore) TransferIfCurrent(id, fromAssignee, toAssignee string) (bool, error) {
	mover, ok := beads.As[beads.ConditionalAssigneeTransferer](s.inner)
	if !ok {
		return false, beads.ErrConditionalTransferUnsupported
	}
	if err := s.checkAssignee("transfer", id, fromAssignee); err != nil {
		return false, err
	}
	if err := s.checkAssignee("transfer", id, toAssignee); err != nil {
		return false, err
	}
	if _, _, err := s.fenceRow("transfer", id); err != nil {
		return false, err
	}
	return mover.TransferIfCurrent(id, fromAssignee, toAssignee)
}

// UpdateIfAssignment applies opts only while the row still has the expected
// status and an admitted expected holder. An inner store without the guarded
// update answers ErrConditionalWriteUnsupported with nothing written.
func (s *rigNamespaceStore) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	updater, ok := beads.As[beads.AssignmentGuardedUpdater](s.inner)
	if !ok {
		return false, beads.ErrConditionalWriteUnsupported
	}
	if err := s.checkAssignee("update", id, expectedAssignee); err != nil {
		return false, err
	}
	row, c, err := s.fenceRow("update", id)
	if err != nil {
		return false, err
	}
	if err := s.checkUpdate(id, row, c.Class, opts); err != nil {
		return false, err
	}
	return updater.UpdateIfAssignment(id, expectedStatus, expectedAssignee, opts)
}

// Claim claims the row for an admitted assignee when it is not Foreign.
func (s *rigNamespaceStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	claimer, ok := beads.As[interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	}](s.inner)
	if !ok {
		return beads.Bead{}, false, beads.ErrClaimUnsupported
	}
	if strings.TrimSpace(assignee) == "" {
		return beads.Bead{}, false, s.refuse("claim", id, "a claim needs an assignee", nil)
	}
	if err := s.checkAssignee("claim", id, assignee); err != nil {
		return beads.Bead{}, false, err
	}
	if _, _, err := s.fenceRow("claim", id); err != nil {
		return beads.Bead{}, false, err
	}
	return claimer.Claim(id, assignee)
}

// ---------------------------------------------------------------------------
// Handle-first capabilities. Each answers false when the inner store lacks
// the capability, and hands out a guarded writer, never the inner one.

// ConditionalWriterHandle exposes the inner store's revision-fenced writes,
// guarded.
func (s *rigNamespaceStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	w, ok := beads.ConditionalWriterFor(s.inner)
	if !ok {
		return nil, false
	}
	return rigNamespaceConditionalWriter{s: s, inner: w}, true
}

// MetadataCASWriterHandle exposes the inner store's metadata value-CAS,
// guarded.
func (s *rigNamespaceStore) MetadataCASWriterHandle() (beads.MetadataCASWriter, bool) {
	w, ok := beads.MetadataCASWriterFor(s.inner)
	if !ok {
		return nil, false
	}
	return rigNamespaceMetadataCAS{s: s, inner: w}, true
}

// AtomicConditionalCloserHandle exposes the inner store's atomic fenced
// close, guarded.
func (s *rigNamespaceStore) AtomicConditionalCloserHandle() (beads.AtomicConditionalCloser, bool) {
	c, ok := beads.AtomicConditionalCloserFor(s.inner)
	if !ok {
		return nil, false
	}
	return rigNamespaceAtomicCloser{s: s, inner: c}, true
}

// GuardedBatcherHandle exposes the inner store's guarded batches, guarded.
func (s *rigNamespaceStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	b, ok := beads.GuardedBatcherFor(s.inner)
	if !ok {
		return nil, false
	}
	return rigNamespaceBatcher{s: s, inner: b}, true
}

// GraphApplyHandle exposes the inner store's graph apply, guarded.
func (s *rigNamespaceStore) GraphApplyHandle() (beads.GraphApplyStore, bool) {
	a, ok := beads.GraphApplyFor(s.inner)
	if !ok {
		return nil, false
	}
	return rigNamespaceGraphApplier{s: s, inner: a}, true
}

type rigNamespaceConditionalWriter struct {
	s     *rigNamespaceStore
	inner beads.ConditionalWriter
}

// UpdateIfMatch is decided on the row at the expected revision (fenceRowAt):
// the inner write lands only at that revision, so the decision is exact for
// the row it writes.
func (w rigNamespaceConditionalWriter) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	row, c, err := w.s.fenceRowAt("update", id, revision)
	if err != nil {
		return err
	}
	if err := w.s.checkUpdate(id, row, c.Class, opts); err != nil {
		return err
	}
	return w.inner.UpdateIfMatch(id, revision, opts)
}

func (w rigNamespaceConditionalWriter) CloseIfMatch(id string, revision int64) error {
	if _, _, err := w.s.fenceRowAt("close", id, revision); err != nil {
		return err
	}
	return w.inner.CloseIfMatch(id, revision)
}

func (w rigNamespaceConditionalWriter) DeleteIfMatch(id string, revision int64) error {
	if _, _, err := w.s.fenceRowAt("delete", id, revision); err != nil {
		return err
	}
	return w.inner.DeleteIfMatch(id, revision)
}

func (w rigNamespaceConditionalWriter) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return rigNamespaceMetadataCAS{s: w.s, inner: w.inner}.CompareAndSetMetadataKey(id, key, expected, next)
}

type rigNamespaceMetadataCAS struct {
	s     *rigNamespaceStore
	inner beads.MetadataCASWriter
}

func (w rigNamespaceMetadataCAS) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	row, c, err := w.s.fenceRow("set metadata", id)
	if err != nil {
		return false, err
	}
	if err := w.s.checkMetadataWrite("set metadata", id, row, c.Class, map[string]string{key: next}); err != nil {
		return false, err
	}
	return w.inner.CompareAndSetMetadataKey(id, key, expected, next)
}

type rigNamespaceAtomicCloser struct {
	s     *rigNamespaceStore
	inner beads.AtomicConditionalCloser
}

func (w rigNamespaceAtomicCloser) CloseWithMetadataIfMatch(id string, revision int64, metadata map[string]string) (beads.Bead, error) {
	row, c, err := w.s.fenceRowAt("close", id, revision)
	if err != nil {
		return beads.Bead{}, err
	}
	if err := w.s.checkMetadataWrite("close", id, row, c.Class, metadata); err != nil {
		return beads.Bead{}, err
	}
	return w.inner.CloseWithMetadataIfMatch(id, revision, metadata)
}

type rigNamespaceBatcher struct {
	s     *rigNamespaceStore
	inner beads.GuardedBatcher
}

// ApplyGuarded stamps every create and fences every update, refusing the
// whole batch on the first item that breaks a rule. An unguarded update is
// pinned to the revision its row was judged at, so the decision holds for
// the row the batch writes.
func (w rigNamespaceBatcher) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	items := make([]beads.GuardedItem, len(b.Items))
	for i, it := range b.Items {
		switch {
		case it.Create != nil:
			stamped, err := w.s.stampCreate(*it.Create)
			if err != nil {
				return beads.GuardedResult{}, err
			}
			items[i] = beads.GuardedItem{Create: &stamped}
		case it.Update != nil:
			u := *it.Update
			row, c, err := w.s.fenceRow("update", u.ID)
			if errors.Is(err, beads.ErrNotFound) {
				// Nothing to judge, and nothing an unguarded update could be
				// pinned to: refuse the batch as the store would.
				return beads.GuardedResult{}, &beads.BatchItemError{Index: i, ID: u.ID, Err: err}
			}
			if err != nil {
				return beads.GuardedResult{}, err
			}
			if cerr := w.s.checkUpdate(u.ID, row, c.Class, u.Opts); cerr != nil {
				return beads.GuardedResult{}, cerr
			}
			if u.ExpectedRevision == nil {
				rev := row.Revision
				u.ExpectedRevision = &rev
			}
			items[i] = beads.GuardedItem{Update: &u}
		default:
			items[i] = it
		}
	}
	b.Items = items
	return w.inner.ApplyGuarded(ctx, b)
}

type rigNamespaceGraphApplier struct {
	s     *rigNamespaceStore
	inner beads.GraphApplyStore
}

func (a rigNamespaceGraphApplier) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	guarded, err := a.s.guardGraphPlan(plan)
	if err != nil {
		return nil, err
	}
	return a.inner.ApplyGraphPlan(ctx, guarded)
}

// ApplyGraphPlanWithStorage applies the plan in a storage tier; an inner
// applier without tiers applies it plainly, as the policy layer would.
func (a rigNamespaceGraphApplier) ApplyGraphPlanWithStorage(ctx context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	guarded, err := a.s.guardGraphPlan(plan)
	if err != nil {
		return nil, err
	}
	if storer, ok := a.inner.(beads.StorageGraphApplyStore); ok {
		return storer.ApplyGraphPlanWithStorage(ctx, guarded, storage)
	}
	return a.inner.ApplyGraphPlan(ctx, guarded)
}

// guardGraphPlan returns a copy of plan whose every node carries the
// creation stamp, after refusing an unadmitted assignee, a bare route, a
// metadata reference that would fill an identity key or the stamp with a
// bead id, and an edge recorded on an existing Foreign row.
func (s *rigNamespaceStore) guardGraphPlan(plan *beads.GraphApplyPlan) (*beads.GraphApplyPlan, error) {
	if plan == nil {
		return nil, nil
	}
	stamp, ok := s.ns.ownerStamp()
	if !ok {
		return nil, s.refuse("graph apply", "", "the rig's namespace admits nothing, so no row can be stamped as this city's", nil)
	}
	out := *plan
	out.Nodes = make([]beads.GraphApplyNode, len(plan.Nodes))
	for i, node := range plan.Nodes {
		if err := s.checkAssignee("graph apply", node.Key, node.Assignee); err != nil {
			return nil, err
		}
		hasOwners := len(rowOwnerValues(beads.Bead{Type: node.Type, Labels: node.Labels, Assignee: node.Assignee, Metadata: node.Metadata})) > 0
		meta, err := s.stampMetadata("graph apply", node.Key, node.Metadata, stamp, hasOwners)
		if err != nil {
			return nil, err
		}
		node.Metadata = meta
		for k := range node.MetadataRefs {
			if isOwnerStampKey(k) || isRigNamespaceIdentityPredicateKey(k) || isRigNamespaceAssigneeMetadataKey(k) {
				return nil, s.refuse("graph apply", node.Key, fmt.Sprintf("metadata reference %s would fill an owner key with a bead id", k), nil)
			}
		}
		out.Nodes[i] = node
	}
	for _, edge := range plan.Edges {
		if strings.TrimSpace(edge.FromID) == "" {
			continue
		}
		if _, _, err := s.fenceRow("add dependency", edge.FromID); err != nil && !errors.Is(err, beads.ErrNotFound) {
			return nil, err
		}
	}
	return &out, nil
}
