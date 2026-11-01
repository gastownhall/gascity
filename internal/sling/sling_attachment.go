package sling

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// BeadFromGetters tries multiple BeadQuerier implementations and returns
// the first successful result.
func BeadFromGetters(id string, getters ...BeadQuerier) (beads.Bead, bool) {
	for _, getter := range getters {
		if getter == nil {
			continue
		}
		b, err := getter.Get(id)
		if err == nil {
			return b, true
		}
	}
	return beads.Bead{}, false
}

// CollectAttachedBeads finds all molecule/workflow attachments for a parent bead.
// Every attachment is read from store; a caller whose attachments may live in
// a different store from the parent (a city that relocates the graph class)
// uses CollectAttachments instead.
func CollectAttachedBeads(parent beads.Bead, store beads.Store, childQuerier BeadChildQuerier) ([]beads.Bead, error) {
	located, err := CollectAttachments(parent, childQuerier, store)
	attachments := make([]beads.Bead, 0, len(located))
	for _, a := range located {
		attachments = append(attachments, a.Bead)
	}
	return attachments, err
}

// AttachedBead is an attachment together with the store that holds it, so a
// caller that mutates the attachment (burn, close) writes to its own store.
type AttachedBead struct {
	Bead  beads.Bead
	Store beads.Store
}

// CollectAttachments finds all molecule/workflow attachments for a parent bead
// across the stores an attachment may live in.
//
// A molecule_id / workflow_id names a bead that was poured through the graph
// store, which on a city that relocates the graph class is a different store
// from the parent's work store; a work-class molecule (a v1 poured recipe, or a
// pre-split one) stays on the work ledger. stores lists the candidates in
// lookup order, graph store first; the first store that resolves an id owns
// it. An id that no store holds is reported as the probe error (ErrNotFound),
// exactly as a single store reports it. A read that failed is reported in its
// place: a not-found is a dangling stamp a guard may pass, a failed read is
// not, so it is never hidden behind one.
//
// DB-child attachments (beads whose parent is this bead) are listed through
// childQuerier and are attributed to the LAST store in stores — the parent's
// own store, since a child shares its parent's ledger.
//
// Ordering risk: on a migrated city the binding holds relics whose ids keep
// their work prefix, and bd can mint a work id that collides with one. A
// molecule_id naming such a reused id resolves to the binding's relic, not to
// the work-store molecule, because the graph store is asked first. The guard
// then decides on the relic's status; it never mutates the work molecule.
// Collisions are rare (bd reuses an id only after its row is gone) and the
// relic census bounds the population; the by-id residency plan is the place to
// settle it if it ever matters.
func CollectAttachments(parent beads.Bead, childQuerier BeadChildQuerier, stores ...beads.Store) ([]AttachedBead, error) {
	stores = nonNilStores(stores)
	var (
		attachments          []AttachedBead
		hardErr, notFoundErr error
	)
	noteErr := func(err error) {
		switch {
		case errors.Is(err, beads.ErrNotFound):
			if notFoundErr == nil {
				notFoundErr = err
			}
		case hardErr == nil:
			hardErr = err
		}
	}
	seen := make(map[string]struct{})

	addByID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || len(stores) == 0 {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		attached, owner, err := getFromStores(id, stores)
		if err != nil {
			noteErr(err)
			return
		}
		seen[id] = struct{}{}
		// A controller-owned row is never an attachment, whatever a
		// molecule_id or workflow_id names: no autoclose, force or GC tears
		// it down (mc-dkblm, architecture B).
		if coordclass.ControllerOwned(attached) {
			return
		}
		attachments = append(attachments, AttachedBead{Bead: attached, Store: owner})
	}

	addByID(parent.Metadata[beadmeta.MoleculeIDMetadataKey])
	addByID(parent.Metadata["workflow_id"])

	if childQuerier != nil {
		children, err := childQuerier.List(beads.ListQuery{
			ParentID: parent.ID,
			Sort:     beads.SortCreatedAsc,
		})
		if err != nil {
			noteErr(err)
		} else {
			var childStore beads.Store
			if len(stores) > 0 {
				childStore = stores[len(stores)-1]
			}
			for _, child := range children {
				if !IsAttachedRoot(child) || coordclass.ControllerOwned(child) {
					continue
				}
				if _, ok := seen[child.ID]; ok {
					continue
				}
				seen[child.ID] = struct{}{}
				attachments = append(attachments, AttachedBead{Bead: child, Store: childStore})
			}
		}
	}

	if hardErr != nil {
		return attachments, hardErr
	}
	return attachments, notFoundErr
}

// getFromStores reads id from the first store that holds it. A store that
// answers ErrNotFound passes the lookup on; any other error is remembered and
// returned only when no later store resolves the id.
func getFromStores(id string, stores []beads.Store) (beads.Bead, beads.Store, error) {
	var hardErr, notFoundErr error
	for _, s := range stores {
		// A first-owner probe: the attachment lives in the graph store or,
		// for a v1 poured molecule, in the work store, so a miss passes on.
		b, err := beads.FederatedGet(s, id)
		if err == nil {
			return b, s, nil
		}
		if errors.Is(err, beads.ErrNotFound) {
			if notFoundErr == nil {
				notFoundErr = err
			}
			continue
		}
		if hardErr == nil {
			hardErr = err
		}
	}
	if hardErr != nil {
		return beads.Bead{}, nil, hardErr
	}
	return beads.Bead{}, nil, notFoundErr
}

// residency:allow — filters the caller's own handle list; resolves no owner.
func nonNilStores(stores []beads.Store) []beads.Store {
	out := stores[:0:0]
	for _, s := range stores {
		if s != nil {
			out = append(out, s)
		}
	}
	return out
}

// attachmentScope names where a source bead and its attachments live: the
// source bead (and the molecule_id / workflow_id it carries) in work, and the
// attachments themselves in graph first, then work. On a single-store city the
// two are the same store and every lookup is the pre-split one.
type attachmentScope struct {
	work  beads.Store
	graph beads.Store
}

// singleStoreAttachmentScope is the scope of a caller that hands one store for
// both the source bead and its attachments.
func singleStoreAttachmentScope(store beads.Store) attachmentScope {
	return attachmentScope{work: store, graph: store}
}

// attachmentScope is the scope of this sling: the source bead in Store, its
// attachments wherever graphStore() poured them.
func (deps SlingDeps) attachmentScope() attachmentScope {
	return attachmentScope{work: deps.Store, graph: deps.graphStore()}
}

// stores returns the stores an attachment may live in, in lookup order: the
// graph store the sling pours through, then the work store that holds the
// source bead. On a single-store city both are the same store, listed once.
//
// residency:allow — the sling's own two handles (the work store it was given
// and the graph store it pours through), in lookup order; no topology, no
// binding census and no namespace is consulted.
func (s attachmentScope) stores() []beads.Store {
	switch {
	case s.graph == nil && s.work == nil:
		return nil
	case s.graph == nil:
		return []beads.Store{s.work} // residency:allow — the caller's own two handles (work, graph), not a store enumeration
	case s.work == nil || sameStoreValue(s.graph, s.work):
		return []beads.Store{s.graph} // residency:allow — the caller's own two handles (work, graph), not a store enumeration
	default:
		return []beads.Store{s.graph, s.work} // residency:allow — the caller's own two handles (work, graph), not a store enumeration
	}
}

// sameStoreValue reports whether a and b are the same store value; a store
// type that is not comparable reads as different.
func sameStoreValue(a, b beads.Store) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// liveConvoyTrackedWorkflowRoots returns the live (non-terminal, per
// convoycore.IsTerminalStatus) graph.v2 workflow roots launched by
// formulaName that are reachable from beadID through the launch's own
// synthetic input convoy: every synthetic convoy that tracks beadID
// (convoycore.TrackingConvoysForItem), filtered to the workflow roots that
// were launched from that convoy -- identified by gc.input_convoy_id, which
// stampGraphV2RootMetadata (sling.go) stamps on every graph.v2 root at
// instantiation time -- and further filtered to formulaName via
// gc.formula_name (stamped on every formula's root step unconditionally,
// formula/compile.go).
//
// This is the only durable link a convoy-first `--on` launch leaves behind:
// attachFormulaToBead deliberately calls doStartGraphWorkflow with an empty
// sourceBeadID on that path ("the source is tracked through the input
// convoy, not gc.source_bead_id" -- sling_core.go), and the root is never a
// DB child of beadID, nor referenced by molecule_id/workflow_id metadata
// either, so it is invisible to CollectAttachedBeads' three routes (#5420).
//
// The formulaName filter matters: distinct formulas targeting the same bead
// are legitimate concurrent work (e.g. a "review" workflow and a "build"
// workflow both attached to one source bead), not a duplicate. Only
// relaunching the SAME formula against the SAME bead while its prior root is
// still live is the bug this guards -- "dedup on (formula, target bead)",
// per the issue.
//
// convoyStore is queried for the tracking edges -- the convoy always lives
// co-resident with the target bead it tracks (TrackItemIn enforces this at
// write time). rootStore is queried for the roots themselves, which may live
// in a different store when graph beads are relocated (deps.graphStore()).
// The common case passes the same store for both.
func liveConvoyTrackedWorkflowRoots(convoyStore, rootStore beads.Store, beadID, formulaName string) ([]beads.Bead, error) {
	beadID = strings.TrimSpace(beadID)
	formulaName = strings.TrimSpace(formulaName)
	if convoyStore == nil || rootStore == nil || beadID == "" || formulaName == "" {
		return nil, nil
	}
	convoys, err := convoycore.TrackingConvoysForItem(convoyStore, beadID)
	if err != nil {
		return nil, fmt.Errorf("listing tracking convoys for %s: %w", beadID, err)
	}
	var roots []beads.Bead
	seen := make(map[string]struct{}, len(convoys))
	for _, convoy := range convoys {
		// These are convoys by construction, so the convoy type's Ready
		// exclusion (#3591) does not apply here -- only skip convoys excluded
		// by infrastructure label (session/order-tracking bookkeeping). Unlike
		// hasLiveTrackingConvoy above, this loop does NOT also skip terminal
		// convoys: liveness is decided per root below (IsTerminalStatus on the
		// root itself), so a closed convoy holding a live root still counts.
		if beads.HasReadyExcludedLabel(convoy) {
			continue
		}
		// Scope to the launch's OWN synthetic input convoy, which is exactly
		// the "(formula, this bead)" edge this guard is for:
		// CreateSingleItemInputConvoy (graphv2/invocation.go) stamps
		// gc.synthetic=true and tracks exactly one item, so a synthetic
		// tracking convoy means a launch was made against this bead alone.
		//
		// A real multi-item convoy that merely happens to track beadID
		// belongs to a DIFFERENT launch: a convoy-level `--on` stamps
		// gc.input_convoy_id with that convoy itself (NormalizeInputConvoy
		// returns a convoy target unchanged), so counting its root here would
		// block a later per-member-bead `--on` of the same formula and
		// misattribute the ConflictError to the member bead. Note that the
		// convoy-target route was never covered by this lookup in the first
		// place -- nothing tracks a convoy target -- so skipping it here
		// removes a false positive rather than dropping real coverage.
		//
		// This does not weaken the #5420 fix: a bare-bead `--on` always mints
		// a synthetic input convoy, so the duplicate relaunch still blocks.
		if strings.TrimSpace(convoy.Metadata[beadmeta.SyntheticMetadataKey]) != "true" {
			continue
		}
		matches, err := rootStore.ListByMetadata(map[string]string{
			beadmeta.InputConvoyIDMetadataKey: convoy.ID,
		}, 0, beads.WithBothTiers)
		if err != nil {
			return nil, fmt.Errorf("listing workflow roots for input convoy %s: %w", convoy.ID, err)
		}
		for _, root := range matches {
			if convoycore.IsTerminalStatus(root.Status) || !IsWorkflowAttachment(root) {
				continue
			}
			if strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != formulaName {
				continue
			}
			if _, ok := seen[root.ID]; ok {
				continue
			}
			seen[root.ID] = struct{}{}
			roots = append(roots, root)
		}
	}
	slices.SortFunc(roots, func(a, b beads.Bead) int {
		return strings.Compare(a.ID, b.ID)
	})
	return roots, nil
}

// LiveConvoyTrackedWorkflowRoots is the exported form of
// liveConvoyTrackedWorkflowRoots, for callers outside this package (the `gc
// sling --dry-run` preview in cmd/gc) that need the same convoy-first
// duplicate-detection checkLegacySourceWorkflowConflict performs at launch
// time, so the dry-run "Pre-check" line reflects real coverage instead of
// reporting a vacuous "no existing molecule/wisp children" pass (#5420).
func LiveConvoyTrackedWorkflowRoots(convoyStore, rootStore beads.Store, beadID, formulaName string) ([]beads.Bead, error) {
	return liveConvoyTrackedWorkflowRoots(convoyStore, rootStore, beadID, formulaName)
}

// AttachmentLabel returns "workflow" or "molecule" based on the bead type.
func AttachmentLabel(b beads.Bead) string {
	if IsWorkflowAttachment(b) {
		return "workflow"
	}
	return "molecule"
}

// IsAttachedRoot reports whether a bead is a workflow or molecule root.
func IsAttachedRoot(b beads.Bead) bool {
	return IsWorkflowAttachment(b) || IsMoleculeAttachment(b)
}

// IsWorkflowAttachment reports whether a bead is a graph.v2 workflow attachment.
func IsWorkflowAttachment(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]), beadmeta.KindWorkflow) ||
		strings.EqualFold(strings.TrimSpace(b.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2)
}

// IsMoleculeAttachment reports whether a bead is a molecule attachment.
func IsMoleculeAttachment(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Type), "molecule")
}

// findBlockingMolecule is the error-returning core behind FindBlockingMolecule
// and HasMoleculeChildren. It returns the first open attached molecule/wisp
// child, and surfaces the attachment-probe error only when no live attachment
// was found -- a discovered live attachment is definitive even if the probe was
// partial. Callers that must fail closed can inspect the error to tell "no
// attachment" apart from "probe failed". Read-only -- does not auto-burn.
func findBlockingMolecule(q BeadQuerier, beadID string, scope attachmentScope) (label, id string, err error) {
	att, err := findBlockingAttachment(q, beadID, scope)
	if att == nil {
		return "", "", err
	}
	return AttachmentLabel(att.Bead), att.Bead.ID, nil
}

// findBlockingAttachment is findBlockingMolecule returning the live attachment
// itself (nil when none was found).
func findBlockingAttachment(q BeadQuerier, beadID string, scope attachmentScope) (*AttachedBead, error) {
	parent, ok := BeadFromGetters(beadID, q, scope.work)
	if !ok {
		return nil, nil
	}
	var childQuerier BeadChildQuerier
	if cq, ok := q.(BeadChildQuerier); ok {
		childQuerier = cq
	} else if cq, ok := any(scope.work).(BeadChildQuerier); ok {
		childQuerier = cq
	}
	attachments, probeErr := CollectAttachments(parent, childQuerier, scope.stores()...)
	for i := range attachments {
		if attachments[i].Bead.Status != "closed" {
			return &attachments[i], nil
		}
	}
	// No live attachment found. A probe error here means we cannot conclude the
	// bead is unattached, so report it rather than a clean "none".
	return nil, probeErr
}

// FindBlockingMolecule checks if the bead has any open attached molecule
// or wisp children. Returns the blocking attachment's label and ID, or
// empty strings if none (or if the attachment probe could not complete).
// Read-only -- does not auto-burn. Attachments are read from store; a caller
// that holds SlingDeps uses SlingDeps.FindBlockingMolecule, which also reads
// the graph store the sling pours through.
func FindBlockingMolecule(q BeadQuerier, beadID string, store beads.Store) (label, id string) {
	label, id, _ = findBlockingMolecule(q, beadID, singleStoreAttachmentScope(store))
	return label, id
}

// FindBlockingMolecule is the package-level FindBlockingMolecule over this
// sling's stores: the source bead in Store, its attachments in the graph store
// first, then Store.
func (deps SlingDeps) FindBlockingMolecule(q BeadQuerier, beadID string) (label, id string) {
	label, id, _ = findBlockingMolecule(q, beadID, deps.attachmentScope())
	return label, id
}

// HasMoleculeChildren reports whether the bead has any open attached molecule
// or wisp children. The returned error is non-nil only when the attachment
// probe could not complete and no live attachment was found, so a caller that
// must fail closed -- such as the --on idempotency override -- can preserve its
// safe state instead of mistaking a probe failure for "no molecule". Read-only
// -- does not auto-burn.
func HasMoleculeChildren(q BeadQuerier, beadID string, store beads.Store) (bool, error) {
	return hasMoleculeChildrenIn(q, beadID, singleStoreAttachmentScope(store))
}

func hasMoleculeChildrenIn(q BeadQuerier, beadID string, scope attachmentScope) (bool, error) {
	label, _, err := findBlockingMolecule(q, beadID, scope)
	return label != "", err
}

// CloseAttachedSubtree closes an attached workflow or molecule root and any
// open descendants beneath it.
func CloseAttachedSubtree(store beads.Store, attached beads.Bead) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("store unavailable")
	}
	if IsWorkflowAttachment(attached) {
		return sourceworkflow.CloseWorkflowSubtree(store, attached.ID)
	}
	return molecule.CloseSubtree(store, attached.ID)
}

func clearAttachmentMetadata(store beads.Store, parent beads.Bead, attached beads.Bead) error {
	if store == nil || strings.TrimSpace(parent.ID) == "" || strings.TrimSpace(attached.ID) == "" {
		return nil
	}
	if strings.TrimSpace(parent.Metadata["workflow_id"]) == attached.ID {
		if err := store.SetMetadata(parent.ID, "workflow_id", ""); err != nil {
			return err
		}
	}
	if strings.TrimSpace(parent.Metadata[beadmeta.MoleculeIDMetadataKey]) == attached.ID {
		if err := store.SetMetadata(parent.ID, beadmeta.MoleculeIDMetadataKey, ""); err != nil {
			return err
		}
	}
	return nil
}

func checkNoMoleculeChildren(q BeadQuerier, beadID string, scope attachmentScope, result *SlingResult, allowLiveWorkflow bool) error {
	parent, ok := BeadFromGetters(beadID, q, scope.work)
	if !ok {
		return nil
	}
	parentUnassigned := strings.TrimSpace(parent.Assignee) == ""

	var childQuerier BeadChildQuerier
	if cq, ok := q.(BeadChildQuerier); ok {
		childQuerier = cq
	} else if cq, ok := any(scope.work).(BeadChildQuerier); ok {
		childQuerier = cq
	}
	attachments, err := CollectAttachments(parent, childQuerier, scope.stores()...)
	probeErr := attachmentProbeFailure(beadID, err)

	for _, located := range attachments {
		attached := located.Bead
		if attached.Status == "closed" {
			continue
		}
		if IsWorkflowAttachment(attached) {
			if allowLiveWorkflow {
				continue
			}
			return &sourceworkflow.ConflictError{
				SourceBeadID: beadID,
				WorkflowIDs:  []string{attached.ID},
			}
		}
		if parentUnassigned && probeErr != nil {
			// The reference the probe could not read may be a live molecule,
			// so burning this one would not make room: refused below, with
			// nothing burned.
			continue
		}
		var held []string
		var unreadable error
		if parentUnassigned && located.Store != nil {
			if held, unreadable = attachmentActiveWork(located); len(held) == 0 && unreadable == nil {
				if _, burnErr := CloseAttachedSubtree(located.Store, attached); burnErr == nil {
					if clearErr := clearAttachmentMetadata(scope.work, parent, attached); clearErr != nil {
						return clearErr
					}
					result.AutoBurned = append(result.AutoBurned, attached.ID)
					continue
				}
			}
		}
		return &MoleculeAttachedError{BeadID: beadID, Label: AttachmentLabel(attached), AttachmentID: attached.ID, ActiveSteps: held, Unreadable: unreadable}
	}
	return probeErr
}

// attachmentProbeFailure is the refusal the attach guards return for err,
// CollectAttachments' error, when no live attachment refuses first. A
// reference no store holds is a dangling stamp (the molecule was purged):
// there is nothing attached there to refuse or burn, so a pure not-found is no
// refusal. Any other failure means the guard cannot tell whether a live
// molecule is attached, even beside the attachments it did read, so it fails
// closed rather than pour a second one.
func attachmentProbeFailure(beadID string, err error) error {
	if err == nil || errors.Is(err, beads.ErrNotFound) {
		return nil
	}
	return &AttachmentProbeError{BeadID: beadID, Err: err}
}

// AttachmentProbeError reports that the attach guard could not read a source
// bead's attachment (a store error, not a not-found), so it cannot tell
// whether a live molecule is attached and refuses rather than risk pouring a
// second one, or could not read the subtree of a stale attachment it would
// replace (subtreeProbeError), so it cannot tell whether a worker holds a step.
// It is a store fault, not a conflict: the API answers it 5xx.
type AttachmentProbeError struct {
	BeadID string
	Err    error
}

func (e *AttachmentProbeError) Error() string {
	return fmt.Sprintf("could not verify molecule attachment for %s: %v", e.BeadID, e.Err)
}

func (e *AttachmentProbeError) Unwrap() error { return e.Err }

// subtreeProbeError is the AttachmentProbeError for an attachment whose
// subtree could not be read (attachmentActiveWork's unreadable): the guard
// read the attachment but could not tell whether a worker holds a step.
func subtreeProbeError(beadID, label, attachmentID string, unreadable error) *AttachmentProbeError {
	return &AttachmentProbeError{BeadID: beadID, Err: fmt.Errorf("reading the subtree of attached %s %s: %w", label, attachmentID, unreadable)}
}

// attachmentActiveWork reports why an unassigned source's stale attachment may
// not be burned to make room for a new one: the beads of its subtree a worker
// holds (in_progress, or hooked), or the error that kept the subtree from
// being read. Fails closed: an unreadable subtree is never burned.
func attachmentActiveWork(a AttachedBead) (held []string, unreadable error) {
	if a.Store == nil {
		return nil, errors.New("its store is unavailable")
	}
	subtree, err := molecule.ListSubtree(a.Store, a.Bead.ID)
	if err != nil {
		return nil, err
	}
	for _, b := range subtree {
		if b.Status == "in_progress" || b.Status == "hooked" {
			held = append(held, b.ID)
		}
	}
	return held, nil
}

// MoleculeAttachedError reports that a bead already has a live, non-workflow
// molecule/wisp attachment blocking a new formula attach. It is distinct from
// sourceworkflow.ConflictError (a live graph.v2 workflow attachment) so
// callers can use errors.As to tell the two conflict kinds apart: an implicit
// default-formula sling may choose to fall back to plain routing on this
// error, but must keep hard-failing on a workflow conflict or any other
// error, since neither is the "unrelated molecule already attached" case the
// fallback exists for.
type MoleculeAttachedError struct {
	BeadID       string
	Label        string // AttachmentLabel(attached), e.g. "molecule" or "wisp"
	AttachmentID string
	// ActiveSteps are the attachment's beads a worker holds (in_progress or
	// hooked), when that is why an unassigned source's attachment was not
	// burned to make room.
	ActiveSteps []string
	// Unreadable is why the attachment's subtree could not be read, when that
	// is why it was not burned (the guard fails closed).
	Unreadable error
}

func (e *MoleculeAttachedError) Error() string {
	msg := fmt.Sprintf("bead %s already has attached %s %s", e.BeadID, e.Label, e.AttachmentID)
	if e.Unreadable != nil {
		msg += fmt.Sprintf("; it was not replaced because its subtree could not be read (%v), so whether a worker holds a step is unknown", e.Unreadable)
	}
	if len(e.ActiveSteps) > 0 {
		msg += fmt.Sprintf("; it was not replaced because a worker holds %s. If that step is stale (its worker is gone), release it with `gc bd update <step> --status open --assignee \"\"` or close it with `gc bd close <step>`, then sling again", strings.Join(e.ActiveSteps, ", "))
	}
	return msg
}

// Unwrap reports an unreadable subtree as the AttachmentProbeError it also
// is: a store fault kept the guard from deciding, which a retry can clear, so
// a caller (the API) answers it as one rather than as a conflict. A refusal
// for any other reason wraps nothing.
func (e *MoleculeAttachedError) Unwrap() error {
	if e.Unreadable == nil {
		return nil
	}
	return subtreeProbeError(e.BeadID, e.Label, e.AttachmentID, e.Unreadable)
}

// CheckNoMoleculeChildren returns an error if the bead already has an attached
// molecule or wisp child that is still open. Auto-burn messages go to result.AutoBurned.
func CheckNoMoleculeChildren(q BeadQuerier, beadID string, store beads.Store, result *SlingResult) error {
	return checkNoMoleculeChildren(q, beadID, singleStoreAttachmentScope(store), result, false)
}

// CheckBatchNoMoleculeChildren checks all open children for existing molecule
// attachments before any wisps are created.
func CheckBatchNoMoleculeChildren(q BeadChildQuerier, open []beads.Bead, store beads.Store, result *SlingResult) error {
	return checkBatchNoMoleculeChildren(q, open, singleStoreAttachmentScope(store), result, false)
}

// CheckNoMoleculeChildrenAllowLiveWorkflow is like CheckNoMoleculeChildren
// but permits an existing live workflow attachment (used on --force graph
// launches that will supersede the existing root under the source-workflow
// lock).
func CheckNoMoleculeChildrenAllowLiveWorkflow(q BeadQuerier, beadID string, store beads.Store, result *SlingResult) error {
	return checkNoMoleculeChildren(q, beadID, singleStoreAttachmentScope(store), result, true)
}

// CheckBatchNoMoleculeChildrenAllowLiveWorkflow is the batch variant of
// CheckNoMoleculeChildrenAllowLiveWorkflow.
func CheckBatchNoMoleculeChildrenAllowLiveWorkflow(q BeadChildQuerier, open []beads.Bead, store beads.Store, result *SlingResult) error {
	return checkBatchNoMoleculeChildren(q, open, singleStoreAttachmentScope(store), result, true)
}

func checkBatchNoMoleculeChildren(q BeadChildQuerier, open []beads.Bead, scope attachmentScope, result *SlingResult, allowLiveWorkflow bool) error {
	var problems []string
	// workflowConflicts tracks children whose already-attached root is a
	// live workflow. We emit a typed *sourceworkflow.ConflictError for
	// those so the CLI/API boundary returns exit 3 + the cleanup hint;
	// without this, users see a generic "cannot use --on" string and
	// never learn about `gc workflow delete-source`. The first child's
	// conflict becomes the typed payload; a combined non-typed error
	// keeps the summary message so "%d/%d" diagnostics stay readable.
	type workflowConflict struct {
		childID    string
		workflowID string
	}
	var workflowConflicts []workflowConflict
	var probeErrs []error
	for _, child := range open {
		attachments, err := CollectAttachments(child, q, scope.stores()...)
		probeErr := attachmentProbeFailure(child.ID, err)
		if probeErr != nil {
			probeErrs = append(probeErrs, probeErr)
			problems = append(problems, fmt.Sprintf("%s (could not verify molecule attachment: %v)", child.ID, err))
		}
		childUnassigned := strings.TrimSpace(child.Assignee) == ""
		for _, located := range attachments {
			attached := located.Bead
			if attached.Status == "closed" {
				continue
			}
			if IsWorkflowAttachment(attached) {
				if allowLiveWorkflow {
					continue
				}
				problems = append(problems, fmt.Sprintf("%s (has %s %s)", child.ID, AttachmentLabel(attached), attached.ID))
				workflowConflicts = append(workflowConflicts, workflowConflict{childID: child.ID, workflowID: attached.ID})
				continue
			}
			if childUnassigned && probeErr != nil {
				// Its failed probe is already reported: nothing is burned past
				// it, as in checkNoMoleculeChildren.
				continue
			}
			var held []string
			var unreadable error
			if childUnassigned && located.Store != nil {
				if held, unreadable = attachmentActiveWork(located); len(held) == 0 && unreadable == nil {
					if _, burnErr := CloseAttachedSubtree(located.Store, attached); burnErr == nil {
						if clearErr := clearAttachmentMetadata(scope.work, child, attached); clearErr != nil {
							return clearErr
						}
						result.AutoBurned = append(result.AutoBurned, attached.ID)
						continue
					}
				}
			}
			if unreadable != nil {
				probeErrs = append(probeErrs, subtreeProbeError(child.ID, AttachmentLabel(attached), attached.ID, unreadable))
				problems = append(problems, fmt.Sprintf("%s (has %s %s, whose subtree could not be read: %v)", child.ID, AttachmentLabel(attached), attached.ID, unreadable))
				continue
			}
			if len(held) > 0 {
				problems = append(problems, fmt.Sprintf("%s (has %s %s, held by %s; release or close a stale step, then sling again)", child.ID, AttachmentLabel(attached), attached.ID, strings.Join(held, ", ")))
				continue
			}
			problems = append(problems, fmt.Sprintf("%s (has %s %s)", child.ID, AttachmentLabel(attached), attached.ID))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	summary := fmt.Errorf("cannot use --on: beads already have attached molecules: %s",
		strings.Join(problems, ", "))
	if len(probeErrs) > 0 {
		// A probe that could not complete is a store fault, not a conflict:
		// carry the typed errors so a caller (the API) answers it as one.
		summary = errors.Join(append([]error{summary}, probeErrs...)...)
	}
	if len(workflowConflicts) == 0 {
		return summary
	}
	// Emit one typed ConflictError per conflicted child so cleanup hints
	// stay correctly attributed. Collapsing into a single error keyed to
	// the first child misreports which source bead owns each blocking
	// workflow — users running the suggested `gc workflow delete-source
	// <first-child>` command would see unrelated workflow IDs and only
	// clean up part of the batch. Group blocking workflow IDs by child,
	// then join them alongside the summary; the CLI walks the
	// error chain to render one cleanup hint per affected child.
	conflictsByChild := make(map[string][]string, len(workflowConflicts))
	childOrder := make([]string, 0, len(workflowConflicts))
	for _, c := range workflowConflicts {
		if _, seen := conflictsByChild[c.childID]; !seen {
			childOrder = append(childOrder, c.childID)
		}
		conflictsByChild[c.childID] = append(conflictsByChild[c.childID], c.workflowID)
	}
	joined := make([]error, 0, len(childOrder)+1)
	for _, childID := range childOrder {
		joined = append(joined, &sourceworkflow.ConflictError{
			SourceBeadID: childID,
			WorkflowIDs:  conflictsByChild[childID],
		})
	}
	joined = append(joined, summary)
	return errors.Join(joined...)
}

// needsConvoyRecovery reports whether an already-routed bead should re-enter
// finalize to repair missing or closed auto-convoy membership.
//
// It fails CLOSED on a store error: rather than reporting "recovery needed"
// (which re-runs finalize and mints a duplicate auto-convoy under a transient
// store hiccup, #2987), it returns the error so callers can treat the convoy
// as already present. A read error is never evidence that recovery is needed.
func needsConvoyRecovery(q BeadQuerier, b beads.Bead, deps SlingDeps, opts BeadCheckOptions) (bool, error) {
	if opts.NoConvoy {
		return false, nil
	}
	live, err := hasLiveTrackingConvoy(deps.Store, b.ID)
	if err != nil {
		return false, err
	}
	if live {
		return false, nil
	}
	parentID := strings.TrimSpace(b.ParentID)
	if parentID == "" {
		return true, nil
	}
	if q == nil {
		return false, nil
	}
	parent, err := q.Get(parentID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			// A genuinely deleted parent is not a transient store hiccup: the
			// routed child is orphaned, so finalize must re-run to recreate its
			// missing auto-convoy. Return recovery-needed rather than failing
			// closed. Only ambiguous/transient store errors fail closed below
			// (assuming the convoy already exists, #2987).
			return true, nil
		}
		return false, fmt.Errorf("reading parent %s for convoy recovery of %s: %w", parentID, b.ID, err)
	}
	if parent.Type == "convoy" {
		return convoycore.IsTerminalStatus(parent.Status), nil
	}
	if sourceworkflow.IsWorkflowRoot(parent) {
		return false, nil
	}
	// Ordinary parent beads do not own the routing lifecycle. A routed child
	// without a live tracking convoy needs finalize to run again so the missing
	// auto-convoy can be recreated; finalize is idempotent for an already-routed
	// bead because CheckBeadState preserves the routed metadata and only repairs
	// the missing tracking attachment.
	return true, nil
}

func hasLiveTrackingConvoy(store beads.Store, itemID string) (bool, error) {
	live, err := liveTrackingConvoys(store, itemID)
	if err != nil {
		return false, err
	}
	return len(live) > 0, nil
}

// liveTrackingConvoys returns every non-terminal convoy tracking itemID that is
// eligible to serve as an auto-convoy root, oldest first
// (TrackingConvoysForItem sorts by creation time).
//
// It is the shared live-root lookup behind both the convoy-recovery check
// (which only needs existence, over every live convoy) and auto-convoy reuse
// at the mint site, which narrows this set to dispatch roots via
// liveAutoConvoyRoots before reusing the first and reaping the rest.
//
// These are convoys by construction, so the convoy type's Ready exclusion
// (#3591) does not apply here — only convoys excluded by infrastructure label
// (session/order-tracking bookkeeping) are skipped. Those track the item for
// their own bookkeeping and are neither dispatch roots to reuse nor duplicates
// to reap.
func liveTrackingConvoys(store beads.Store, itemID string) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	convoys, err := convoycore.TrackingConvoysForItem(store, itemID)
	if err != nil {
		return nil, fmt.Errorf("listing tracking convoys for %s: %w", itemID, err)
	}
	live := make([]beads.Bead, 0, len(convoys))
	for _, convoy := range convoys {
		if beads.HasReadyExcludedLabel(convoy) || convoycore.IsTerminalStatus(convoy.Status) {
			continue
		}
		live = append(live, convoy)
	}
	return live, nil
}

// AutoConvoyRootTitle is the title finalize mints auto-convoy roots under.
// The reuse/reap path keys on it so only roots this dispatch path created are
// eligible — user convoys (gc convoy create), drain unit convoys and graph.v2
// input convoys are all unowned, unlabeled convoys that can track the same
// bead, and must never be adopted as a dispatch root or reaped as a duplicate.
func AutoConvoyRootTitle(beadID string) string { return "sling-" + beadID }

// liveAutoConvoyRoots is liveTrackingConvoys narrowed to the auto-convoy roots
// finalize minted for itemID, oldest first. This is the reuse/reap set: a
// convoy that merely tracks the bead is somebody else's convoy.
func liveAutoConvoyRoots(store beads.Store, itemID string) ([]beads.Bead, error) {
	live, err := liveTrackingConvoys(store, itemID)
	if err != nil {
		return nil, err
	}
	want := AutoConvoyRootTitle(itemID)
	roots := make([]beads.Bead, 0, len(live))
	for _, c := range live {
		if strings.TrimSpace(c.Title) == want {
			roots = append(roots, c)
		}
	}
	return roots, nil
}

// convoyReapReason is the close_reason stamped on an auto-convoy root that a
// re-sling superseded. Long enough to satisfy bd's validation.on-close=error
// length requirement while naming why the root was closed.
const convoyReapReason = "convoy reap: superseded duplicate root"

// reapSupersededConvoyRoots closes auto-convoy roots that a re-sling has
// superseded, so a bead carrying several live roots converges to the single one
// being reused instead of staying stuck at N until the tracked bead closes
// (ga-5jnq).
//
// Reaping is best-effort and never blocks the dispatch: each failure is
// returned as a message for SlingResult.MetadataErrors. A root left open is the
// pre-existing over-count, which the drain still clears when the tracked bead
// goes terminal; failing the sling over it would be strictly worse.
//
// Callers must pass only unowned roots. The "owned" label is what suppresses
// convoy autoclose, so closing one here would silently convert a
// caller-managed lifecycle into an auto-managed one.
func reapSupersededConvoyRoots(store beads.Store, superseded []beads.Bead, keptID string) []string {
	var problems []string
	for _, root := range superseded {
		if err := convoycore.CloseWithReason(store, root.ID, convoyReapReason); err != nil {
			problems = append(problems,
				fmt.Sprintf("reaping convoy root %s superseded by %s: %v", root.ID, keptID, err))
		}
	}
	return problems
}

// resolveConvoyRecovery maps needsConvoyRecovery onto a BeadCheckResult for an
// already-routed bead: an empty result when finalize must re-run to recreate a
// missing auto-convoy, or Idempotent otherwise. On a store error it fails
// CLOSED — assuming the convoy already exists rather than minting a duplicate
// (#2987) — and surfaces the error as a warning instead of swallowing it.
func resolveConvoyRecovery(q BeadQuerier, b beads.Bead, deps SlingDeps, opts BeadCheckOptions, beadID string) BeadCheckResult {
	needRecovery, err := needsConvoyRecovery(q, b, deps, opts)
	if err != nil {
		return BeadCheckResult{
			Idempotent: true,
			Warnings:   []string{fmt.Sprintf("warning: bead %s convoy-recovery check failed, assuming convoy exists: %v", beadID, err)},
		}
	}
	if needRecovery {
		// Prior sling set gc.routed_to but left no convoy — let finalize
		// re-run to create it and poke the controller.
		return BeadCheckResult{}
	}
	return BeadCheckResult{Idempotent: true}
}

// CheckBeadState checks whether a bead is already routed and returns a
// structured result. Best-effort: nil querier or query failure → empty result.
func CheckBeadState(q BeadQuerier, beadID string, a config.Agent, deps SlingDeps) BeadCheckResult {
	return CheckBeadStateWithOptions(q, beadID, a, deps, BeadCheckOptions{})
}

// CheckBeadStateWithOptions checks whether a bead is already routed for the
// requested route options and returns a structured result, which also
// carries the status the check read.
func CheckBeadStateWithOptions(q BeadQuerier, beadID string, a config.Agent, deps SlingDeps, opts BeadCheckOptions) BeadCheckResult {
	if q == nil {
		return BeadCheckResult{}
	}
	b, err := q.Get(beadID)
	if err != nil {
		return BeadCheckResult{}
	}
	result := checkReadBeadState(q, b, beadID, a, deps, opts)
	result.Status = b.Status
	return result
}

// checkReadBeadState returns the routing verdict of CheckBeadStateWithOptions
// for b, the bead it just read.
func checkReadBeadState(q BeadQuerier, b beads.Bead, beadID string, a config.Agent, deps SlingDeps, opts BeadCheckOptions) BeadCheckResult {
	if IsCustomSlingQuery(a) {
		return BeadCheckResult{Warnings: routedStateWarnings(b, beadID)}
	}

	target := agentutil.RoutedToIdentity(&a)
	isMulti := agentutil.IsMultiSessionAgent(&a)
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == target {
		// A pool session claims routed work under its own session identity
		// (its session bead ID since #6324; "<target>-<suffix>" session_name
		// on older claims), not under the bare pool target, so bare equality
		// reads already-claimed pool work as un-slung and mints a second
		// attempt for it. The original keeps gc.routed_to once wrapped, so
		// both it and its do-work step satisfy the pool work_query: one unit
		// of work, two dispatchable rows, two sessions. Treat a claim by any of
		// this pool's own sessions as idempotent (assigneeIsOwnPoolSession); a
		// claim by a different pool still falls through to the warning below.
		claimedByOwnPoolSession := isMulti && assigneeIsOwnPoolSession(b.Assignee, target, q, deps.Store)
		if b.Assignee == "" || b.Assignee == target || claimedByOwnPoolSession {
			return resolveConvoyRecovery(q, b, deps, opts, beadID)
		}
		return BeadCheckResult{
			Warnings: []string{fmt.Sprintf("warning: bead %s routed to %q but assigned to %q", beadID, target, b.Assignee)},
		}
	}

	if !isMulti {
		if b.Assignee == target {
			return resolveConvoyRecovery(q, b, deps, opts, beadID)
		}
		return BeadCheckResult{Warnings: routedStateWarnings(b, beadID)}
	}

	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == "" {
		poolLabel := "pool:" + target
		for _, l := range b.Labels {
			if l == poolLabel {
				return resolveConvoyRecovery(q, b, deps, opts, beadID)
			}
		}
	}
	return BeadCheckResult{Warnings: routedStateWarnings(b, beadID)}
}

// assigneeIsOwnPoolSession reports whether assignee is one of pool target's own
// sessions. A pool session's claim used to be recorded under its session_name
// ("<target>-<suffix>"), which the prefix check still recognizes for beads
// claimed before the upgrade. Since #6324 an unaliased pool session claims
// under its session bead ID, which carries no pool prefix, so the assignee is
// also resolved as a bead ID: it is this pool's own when it names an open
// session bead created from target. getters are tried in order; the first
// that resolves the exact ID decides (bd resolves partial IDs, so a result
// whose ID differs from assignee is not a match).
func assigneeIsOwnPoolSession(assignee, target string, getters ...BeadQuerier) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" || target == "" {
		return false
	}
	if strings.HasPrefix(assignee, target+"-") {
		return true
	}
	sb, ok := BeadFromGetters(assignee, getters...)
	if !ok || sb.ID != assignee {
		return false
	}
	return session.IsOpenSessionOfTemplate(sb, target)
}

// routedStateWarnings reports human-readable warnings describing any existing
// routing state on b (assignee, gc.routed_to metadata, and pool: labels) that
// would collide with a fresh sling of beadID. It returns nil when b carries no
// such state.
func routedStateWarnings(b beads.Bead, beadID string) []string {
	var warnings []string
	if b.Assignee != "" {
		warnings = append(warnings, fmt.Sprintf("warning: bead %s already assigned to %q", beadID, b.Assignee))
	}
	if routedTo := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]); routedTo != "" {
		warnings = append(warnings, fmt.Sprintf("warning: bead %s already routed to %q", beadID, routedTo))
	}
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "pool:") {
			warnings = append(warnings, fmt.Sprintf("warning: bead %s already has pool label %q", beadID, l))
		}
	}
	return warnings
}
