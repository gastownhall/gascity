package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sessionWaitDependencyDurableGeneration is the provenance marker a target
// resolved from the DURABLE wait rows carries in place of a published index
// generation. It is deliberately non-zero: the lease provenance validation
// rejects a zero generation, and a durable-resolved target has provenance -- it
// is simply the rows themselves rather than a census of them (ga-zo9h3 option
// (b)).
const sessionWaitDependencyDurableGeneration uint64 = 1<<64 - 1

// sessionWaitDependencyTarget is the detached durable identity needed to
// reread one dependency wait. It contains no store or runtime capability.
type sessionWaitDependencyTarget struct {
	WaitID     string
	SessionID  string
	DepIDs     []string
	DepMode    string
	generation uint64
	// authoritative marks a target resolved from the DURABLE wait rows rather
	// than from the observed index. The index cannot certify such a target --
	// the index missing the wait is exactly why the live path exists -- so its
	// freshness comes from the live read plus the lease certification that
	// re-reads both durable rows (ga-zo9h3 option (b)).
	authoritative bool
}

// sessionWaitDependencyIndex maps each pending dependency wait to the session
// it should wake when an exact dependency becomes ready.
type sessionWaitDependencyIndex struct {
	mu           sync.Mutex
	byWaitID     map[string]waitDependencyRegistration
	byDependency map[string]map[string]string
}

type waitDependencyRegistration struct {
	sessionID string
	depIDs    []string
	depMode   string
}

// TargetsForDependency returns detached exact wait targets for one dependency.
// The returned order is stable by wait ID so a caller can schedule without
// inheriting map iteration nondeterminism.
func (i *sessionWaitDependencyIndex) TargetsForDependency(depID string) []sessionWaitDependencyTarget {
	i.mu.Lock()
	defer i.mu.Unlock()

	edges := i.byDependency[depID]
	if len(edges) == 0 {
		return nil
	}
	ids := make([]string, 0, len(edges))
	for waitID := range edges {
		ids = append(ids, waitID)
	}
	sort.Strings(ids)
	return i.targetsLocked(ids)
}

// TargetForWait returns a detached exact target when the wait is currently
// indexed.
func (i *sessionWaitDependencyIndex) TargetForWait(waitID string) (sessionWaitDependencyTarget, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	registration, ok := i.byWaitID[waitID]
	if !ok {
		return sessionWaitDependencyTarget{}, false
	}
	return sessionWaitDependencyTarget{
		WaitID:    waitID,
		SessionID: registration.sessionID,
		DepIDs:    append([]string(nil), registration.depIDs...),
		DepMode:   registration.depMode,
	}, true
}

// AllTargets returns a detached, canonically ordered snapshot of the index.
func (i *sessionWaitDependencyIndex) AllTargets() []sessionWaitDependencyTarget {
	i.mu.Lock()
	defer i.mu.Unlock()
	ids := make([]string, 0, len(i.byWaitID))
	for waitID := range i.byWaitID {
		ids = append(ids, waitID)
	}
	sort.Strings(ids)
	return i.targetsLocked(ids)
}

func (i *sessionWaitDependencyIndex) targetsLocked(ids []string) []sessionWaitDependencyTarget {
	targets := make([]sessionWaitDependencyTarget, 0, len(ids))
	for _, waitID := range ids {
		registration, ok := i.byWaitID[waitID]
		if !ok {
			continue
		}
		targets = append(targets, sessionWaitDependencyTarget{
			WaitID:    waitID,
			SessionID: registration.sessionID,
			DepIDs:    append([]string(nil), registration.depIDs...),
			DepMode:   registration.depMode,
		})
	}
	return targets
}

func newSessionWaitDependencyIndex() *sessionWaitDependencyIndex {
	return &sessionWaitDependencyIndex{
		byWaitID:     make(map[string]waitDependencyRegistration),
		byDependency: make(map[string]map[string]string),
	}
}

// Replace replaces a wait's registration. Known non-pending dependency waits
// remove any prior registration. Malformed dependency waits leave prior state
// unchanged.
func (i *sessionWaitDependencyIndex) Replace(wait sessionpkg.WaitInfo) error {
	registration, indexable, err := waitDependencyRegistrationFrom(wait)
	if err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.removeLocked(wait.ID)
	if !indexable {
		return nil
	}
	addWaitDependencyRegistration(i.byWaitID, i.byDependency, wait.ID, registration)
	return nil
}

// Rebuild atomically replaces the index from the supplied wait census. It does
// not validate census freshness; callers must fence stale snapshots.
func (i *sessionWaitDependencyIndex) Rebuild(waits []sessionpkg.WaitInfo) error {
	seen := make(map[string]struct{}, len(waits))
	for _, wait := range waits {
		if err := validateWaitDependencyIndexID("wait ID", wait.ID); err != nil {
			return fmt.Errorf("rebuilding wait dependency index: %w", err)
		}
		if _, exists := seen[wait.ID]; exists {
			return fmt.Errorf("rebuilding wait dependency index: duplicate wait ID %q", wait.ID)
		}
		seen[wait.ID] = struct{}{}
	}

	byWaitID := make(map[string]waitDependencyRegistration, len(waits))
	byDependency := make(map[string]map[string]string)
	for _, wait := range waits {
		registration, indexable, err := waitDependencyRegistrationFrom(wait)
		if err != nil {
			return fmt.Errorf("rebuilding wait dependency index for %q: %w", wait.ID, err)
		}
		if indexable {
			addWaitDependencyRegistration(byWaitID, byDependency, wait.ID, registration)
		}
	}

	i.mu.Lock()
	i.byWaitID = byWaitID
	i.byDependency = byDependency
	i.mu.Unlock()
	return nil
}

// Remove removes a wait's registration when the durable wait disappears.
func (i *sessionWaitDependencyIndex) Remove(waitID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.removeLocked(waitID)
}

// SessionsForDependency returns detached session IDs registered for one exact
// dependency, sorted for deterministic wake scheduling.
func (i *sessionWaitDependencyIndex) SessionsForDependency(depID string) []string {
	i.mu.Lock()
	defer i.mu.Unlock()

	edges := i.byDependency[depID]
	if len(edges) == 0 {
		return nil
	}
	sessions := make(map[string]struct{}, len(edges))
	for _, sessionID := range edges {
		sessions[sessionID] = struct{}{}
	}
	result := make([]string, 0, len(sessions))
	for sessionID := range sessions {
		result = append(result, sessionID)
	}
	sort.Strings(result)
	return result
}

func (i *sessionWaitDependencyIndex) containsWait(id string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.byWaitID[id]
	return ok
}

func waitDependencyRegistrationFrom(wait sessionpkg.WaitInfo) (waitDependencyRegistration, bool, error) {
	if wait.Kind != "deps" {
		return waitDependencyRegistration{}, false, nil
	}
	switch wait.Status {
	case "closed":
		return waitDependencyRegistration{}, false, nil
	case "open":
	default:
		return waitDependencyRegistration{}, false, fmt.Errorf("dependency wait %q has unsupported status %q", wait.ID, wait.Status)
	}
	switch wait.State {
	case waitStatePending:
	case waitStateReady:
		return waitDependencyRegistration{}, false, nil
	default:
		if sessionpkg.IsWaitTerminalState(wait.State) {
			return waitDependencyRegistration{}, false, nil
		}
		return waitDependencyRegistration{}, false, fmt.Errorf("open dependency wait %q has unsupported state %q", wait.ID, wait.State)
	}
	if err := validateWaitDependencyIndexID("wait ID", wait.ID); err != nil {
		return waitDependencyRegistration{}, false, err
	}
	if err := validateWaitDependencyIndexID("session ID", wait.SessionID); err != nil {
		return waitDependencyRegistration{}, false, err
	}
	if wait.DepMode != "all" && wait.DepMode != "any" {
		return waitDependencyRegistration{}, false, fmt.Errorf("pending dependency wait %q has invalid dependency mode %q", wait.ID, wait.DepMode)
	}
	if len(wait.DepIDs) == 0 {
		return waitDependencyRegistration{}, false, fmt.Errorf("pending dependency wait %q has no dependency IDs", wait.ID)
	}

	depIDs := make([]string, 0, len(wait.DepIDs))
	seen := make(map[string]struct{}, len(wait.DepIDs))
	for _, depID := range wait.DepIDs {
		if err := validateWaitDependencyIndexID("dependency ID", depID); err != nil {
			return waitDependencyRegistration{}, false, fmt.Errorf("pending dependency wait %q: %w", wait.ID, err)
		}
		if _, exists := seen[depID]; exists {
			continue
		}
		seen[depID] = struct{}{}
		depIDs = append(depIDs, depID)
	}
	sort.Strings(depIDs)
	return waitDependencyRegistration{sessionID: wait.SessionID, depIDs: depIDs, depMode: wait.DepMode}, true, nil
}

func validateWaitDependencyIndexID(field, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is empty or has surrounding whitespace", field)
	}
	return nil
}

func addWaitDependencyRegistration(byWaitID map[string]waitDependencyRegistration, byDependency map[string]map[string]string, waitID string, registration waitDependencyRegistration) {
	byWaitID[waitID] = registration
	for _, depID := range registration.depIDs {
		edges := byDependency[depID]
		if edges == nil {
			edges = make(map[string]string)
			byDependency[depID] = edges
		}
		edges[waitID] = registration.sessionID
	}
}

func (i *sessionWaitDependencyIndex) removeLocked(waitID string) {
	registration, exists := i.byWaitID[waitID]
	if !exists {
		return
	}
	for _, depID := range registration.depIDs {
		edges := i.byDependency[depID]
		delete(edges, waitID)
		if len(edges) == 0 {
			delete(i.byDependency, depID)
		}
	}
	delete(i.byWaitID, waitID)
}
