package sourceworkflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

const workflowInputOwnerPrefix = "workflow:"

// InputOwnership coordinates direct dispatch and graph-workflow execution of
// work beads. Work and Graph name the resident stores, which may be distinct.
// Target is the normalized route allowed to transfer unclaimed work to a graph.
// LockScope must match the resident work store's source-workflow lock scope.
type InputOwnership struct {
	Work, Graph                 beads.Store
	CityPath, LockScope, Target string
}

// WithWorkflow reserves the live members of inputConvoyID before launch and
// rolls back newly acquired ownership on failure. Empty input means a standalone
// graph with no work to own. The callback must settle failed graph artifacts
// before returning: a remaining live root keeps its input reserved.
//
// Workflows and direct claims contend on the SAME assignee field. A graph
// step's assignment does not own the input bead: reserve every live input
// before publishing any executable steps. Metadata-only locks cannot exclude
// bd --claim. Revision CAS also fences a direct route arriving during launch.
// The workflow lane owns the input until all graphs using it are terminal.
// Distinct configured formulas may still share an input (e.g. build/review);
// neither may compete with a direct executor for that input's assignment.
func (o InputOwnership) WithWorkflow(ctx context.Context, inputConvoyID string, launch func() error) error {
	if inputConvoyID == "" {
		return launch()
	}
	members, err := convoycore.Members(liveInputStore{Store: o.Work, reader: beads.HandlesFor(o.Work).Live}, inputConvoyID, false)
	if err != nil {
		return fmt.Errorf("loading workflow input ownership: %w", err)
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return o.withLocks(ctx, ids, func() error { return o.acquireWorkflowInputs(members, launch) })
}

// All input locks use a separate namespace from source/root launch locks.
// Sorted acquisition avoids deadlock between overlapping multi-item convoys.
func (o InputOwnership) withLocks(ctx context.Context, ids []string, fn func() error) error {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	var lock func(int) error
	lock = func(index int) error {
		if index == len(ids) {
			return fn()
		}
		return WithLock(ctx, o.CityPath, o.LockScope, "execution-input:"+ids[index], func() error { return lock(index + 1) })
	}
	return lock(0)
}

func (o InputOwnership) acquireWorkflowInputs(members []beads.Bead, launch func() error) error {
	var acquired []string
	rollback := func(cause error) error {
		for _, id := range acquired {
			current, err := beads.HandlesFor(o.Work).Live.Get(id)
			if err == nil && current.Assignee == workflowInputOwnerPrefix+id {
				err = o.checkWorkflowInputOwner(current)
			}
			if err != nil {
				cause = errors.Join(cause, err)
			}
		}
		return cause
	}
	target := o.Target
	for _, member := range members {
		owner := workflowInputOwnerPrefix + member.ID
		current, err := beads.HandlesFor(o.Work).Live.Get(member.ID)
		if err != nil {
			return rollback(fmt.Errorf("loading workflow input %s: %w", member.ID, err))
		}
		if convoycore.IsTerminalStatus(current.Status) {
			continue
		}
		if current.Assignee == owner {
			continue // another configured graph shares the workflow lane
		}
		if strings.HasPrefix(current.Assignee, workflowInputOwnerPrefix) {
			if err := o.checkWorkflowInputOwner(current); err != nil {
				return rollback(err)
			}
			current, err = beads.HandlesFor(o.Work).Live.Get(member.ID)
			if err != nil {
				return rollback(err)
			}
		}
		route := strings.TrimSpace(current.Metadata[beadmeta.RoutedToMetadataKey])
		// A routed but unclaimed input may transfer to a workflow on that same
		// target. A different route or any direct claimant is never stolen,
		// including by --force (which replaces workflows, not live workers).
		if current.Assignee != "" || (route != "" && route != target) || current.Status != "open" {
			return rollback(fmt.Errorf("input bead %s already has execution owner (assignee=%q route=%q status=%q)", current.ID, current.Assignee, route, current.Status))
		}
		writer, ok := beads.ConditionalWriterFor(o.Work)
		if !ok {
			return rollback(beads.ErrConditionalWriteUnsupported)
		}
		if err := writer.UpdateIfMatch(current.ID, current.Revision, beads.UpdateOpts{Assignee: &owner}); err != nil {
			// Ambiguous writes fail closed but roll back our own committed
			// assignment if the readback confirms it, never a competing owner.
			acquired = append(acquired, current.ID)
			return rollback(fmt.Errorf("reserving workflow input %s: %w", current.ID, err))
		}
		acquired = append(acquired, current.ID)
	}
	if err := launch(); err != nil {
		return rollback(err)
	}
	return nil
}

// WithDirect runs routing while excluding graph launches on the same input.
// Adapter failures restore only an unclaimed route owned by this invocation.
func (o InputOwnership) WithDirect(ctx context.Context, id string, route func() error) error {
	return o.withLocks(ctx, []string{id}, func() error {
		rollback, err := o.reserveDirectInputRoute(id)
		if err != nil {
			return err
		}
		if err := route(); err != nil {
			return errors.Join(err, rollback())
		}
		return nil
	})
}

func releaseWorkflowInputOwner(store beads.Store, id, owner string) error {
	current, err := beads.HandlesFor(store).Live.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Assignee != owner {
		return nil
	}
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	empty := ""
	if err := writer.UpdateIfMatch(id, current.Revision, beads.UpdateOpts{Assignee: &empty}); err != nil {
		return fmt.Errorf("releasing workflow input %s: %w", id, err)
	}
	return nil
}

// Called under the input lock: no launch can be between acquire and publish.
// Every live graph using any tracking convoy keeps the input reserved. A
// terminal or crashed-before-publish workflow relinquishes unfinished input
// on the next dispatch, without a status file or a stale-owner timeout.
func (o InputOwnership) checkWorkflowInputOwner(input beads.Bead) error {
	if !strings.HasPrefix(input.Assignee, workflowInputOwnerPrefix) {
		return nil
	}
	convoys, err := convoycore.TrackingConvoysForItem(liveInputStore{Store: o.Work, reader: beads.HandlesFor(o.Work).Live}, input.ID)
	if err != nil {
		return err
	}
	for _, convoy := range convoys {
		roots, err := beads.HandlesFor(o.Graph).Live.List(beads.ListQuery{Metadata: map[string]string{beadmeta.InputConvoyIDMetadataKey: convoy.ID}, IncludeClosed: true, TierMode: beads.TierBoth})
		if err != nil {
			return fmt.Errorf("checking workflow owner of %s: %w", input.ID, err)
		}
		for _, root := range roots {
			if IsWorkflowRoot(root) && !convoycore.IsTerminalStatus(root.Status) {
				return &ConflictError{SourceBeadID: input.ID, WorkflowIDs: []string{root.ID}}
			}
		}
	}
	return releaseWorkflowInputOwner(o.Work, input.ID, input.Assignee)
}

// ReleaseTerminal relinquishes unfinished inputs after a launch rollback or
// workflow termination. Inputs still used by another live graph stay owned.
func (o InputOwnership) ReleaseTerminal(ctx context.Context, inputConvoyID string) error {
	if inputConvoyID == "" {
		return nil
	}
	members, err := convoycore.Members(liveInputStore{Store: o.Work, reader: beads.HandlesFor(o.Work).Live}, inputConvoyID, false)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return o.withLocks(ctx, ids, func() error {
		for _, id := range ids {
			current, err := beads.HandlesFor(o.Work).Live.Get(id)
			if err != nil {
				return err
			}
			if err := o.checkWorkflowInputOwner(current); err != nil {
				var conflict *ConflictError
				if !errors.As(err, &conflict) {
					return err
				}
			}
		}
		return nil
	})
}

// Reuse convoy membership over the authoritative read handle. A stale cached
// tracks edge must never make a live workflow appear to have relinquished work.
type liveInputStore struct {
	beads.Store
	reader beads.LiveReader
}

func (s liveInputStore) Get(id string) (beads.Bead, error)            { return s.reader.Get(id) }
func (s liveInputStore) List(q beads.ListQuery) ([]beads.Bead, error) { return s.reader.List(q) }
func (s liveInputStore) DepList(id, direction string) ([]beads.Dep, error) {
	return s.reader.DepList(id, direction)
}

// Fence the route before invoking either routing adapter. Workflow acquire
// checks this same revision and route, so a racing launch cannot slip between
// our check and a shell/HTTP routing write. Restore only an unclaimed route on
// adapter failure; a delivered claim is never silently taken back.
func (o InputOwnership) reserveDirectInputRoute(id string) (func() error, error) {
	current, err := beads.HandlesFor(o.Work).Live.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return func() error { return nil }, nil
	} // --force compatibility
	if err != nil {
		return nil, err
	}
	if err := o.checkWorkflowInputOwner(current); err != nil {
		return nil, err
	}
	current, err = beads.HandlesFor(o.Work).Live.Get(id)
	if err != nil {
		return nil, err
	}
	writer, ok := beads.ConditionalWriterFor(o.Work)
	if !ok {
		// Direct routing on legacy stores remains supported. The shared input
		// lock fences GC graph launches; those still require assignment CAS.
		return func() error { return nil }, nil
	}
	target := o.Target
	previous := current.Metadata[beadmeta.RoutedToMetadataKey]
	if err := writer.UpdateIfMatch(id, current.Revision, beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target}}); err != nil {
		return nil, fmt.Errorf("reserving direct input route %s: %w", id, err)
	}
	return func() error {
		fresh, err := beads.HandlesFor(o.Work).Live.Get(id)
		if err != nil {
			return err
		}
		if fresh.Assignee != "" || fresh.Metadata[beadmeta.RoutedToMetadataKey] != target || fresh.Status != current.Status {
			return nil
		}
		return writer.UpdateIfMatch(id, fresh.Revision, beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: previous}})
	}, nil
}

// WithReassign excludes workflow launches while checking and changing the input
// assignment. Force-reassign never clears the assignment of a live workflow.
func (o InputOwnership) WithReassign(ctx context.Context, id string, reassign func() error) error {
	return o.withLocks(ctx, []string{id}, func() error {
		current, err := beads.HandlesFor(o.Work).Live.Get(id)
		if err != nil {
			return err
		}
		if err := o.checkWorkflowInputOwner(current); err != nil {
			return err
		}
		return reassign()
	})
}
