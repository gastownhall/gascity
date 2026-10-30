//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const claimedWorkRefreshTimeout = 2 * time.Second

var errClaimedWorkRefreshTimeout = errors.New("claimed-work refresh timed out")

var claimedWorkRefreshFlights sync.Map

var claimedWorkRefreshObservations = struct {
	sync.Mutex
	seen map[string]struct{}
}{
	seen: make(map[string]struct{}),
}

type claimedWorkRefreshKey struct {
	store beads.Store
	ref   string
}

type claimedWorkRefreshIndexKey struct {
	refresh claimedWorkRefreshKey
	id      string
}

type claimedWorkRefreshCandidate struct {
	sessionName string
	trigger     string
	indexes     []int
}

type claimedWorkRefreshRunningSnapshot struct {
	loaded  bool
	running map[string]bool
	err     error
}

type claimedWorkRefreshFlight struct {
	done chan struct{}
	rows []beads.Bead
	err  error
}

// refreshDrainAckedOrExitedClaimedWorkForDemand re-reads the in-progress work
// claimed by sessions that drain-acked or exited, so demand is computed from
// current rows, and drops work the refresh finds closed, unassigned or missing.
//
// residency:allow — a caller's own snapshot, projected. Each bead is re-read
// from the store it was collected from and kept or dropped together with that
// store and ref; it opens no store, consults no binding, namespace or leg
// order, and can only ever return a subsequence of the stores it was handed.
func refreshDrainAckedOrExitedClaimedWorkForDemand(
	cfg *config.City,
	sp runtime.Provider,
	assignedWorkBeads []beads.Bead,
	assignedWorkStores []beads.Store,
	assignedWorkStoreRefs []string,
	readyAssigned map[storeScopedBeadKey]bool,
	sessionBeads *sessionBeadSnapshot,
	stderr io.Writer,
) ([]beads.Bead, []beads.Store, []string, map[storeScopedBeadKey]bool) {
	if sp == nil || sessionBeads == nil || len(assignedWorkBeads) == 0 {
		return assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, readyAssigned
	}
	if stderr == nil {
		stderr = io.Discard
	}
	workByAssignee := make(map[string][]int)
	for i, work := range assignedWorkBeads {
		if strings.TrimSpace(work.Status) != "in_progress" {
			continue
		}
		if assignee := strings.TrimSpace(work.Assignee); assignee != "" {
			workByAssignee[assignee] = append(workByAssignee[assignee], i)
		}
	}
	if len(workByAssignee) == 0 {
		return assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, readyAssigned
	}

	var runningSnapshot claimedWorkRefreshRunningSnapshot
	var candidates []claimedWorkRefreshCandidate
	for _, info := range sessionBeads.OpenInfos() {
		sessionName := strings.TrimSpace(info.SessionNameMetadata)
		if sessionName == "" {
			continue
		}
		indexSet := make(map[int]struct{})
		for _, identity := range compactSessionAssignmentIdentifiers(append(sessionBeadAssigneeIdentitiesInfo(info), sessionAssignmentIdentifiersForConfigInfo(info, cfg)...)) {
			for _, idx := range workByAssignee[identity] {
				indexSet[idx] = struct{}{}
			}
		}
		if len(indexSet) == 0 {
			continue
		}
		indexes := make([]int, 0, len(indexSet))
		for idx := range indexSet {
			indexes = append(indexes, idx)
		}
		sort.Ints(indexes)

		trigger := ""
		if claimedWorkRefreshSessionMayBeRunning(info) {
			if ack, err := sp.GetMeta(sessionName, "GC_DRAIN_ACK"); err == nil {
				if strings.TrimSpace(ack) == "1" {
					trigger = "drain-ack"
				}
			} else {
				fmt.Fprintf(stderr, "claimed-work refresh: checking drain-ack for %s: %v\n", sessionName, err) //nolint:errcheck
			}
		}
		if trigger == "" && claimedWorkRefreshSessionExited(info, sp, &runningSnapshot, stderr) {
			trigger = "exit"
		}
		if trigger == "" {
			continue
		}
		if !claimRefreshObservationFirstSeen(sessionName, trigger, indexes, assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs) {
			continue
		}
		candidates = append(candidates, claimedWorkRefreshCandidate{sessionName: sessionName, trigger: trigger, indexes: indexes})
	}
	if len(candidates) == 0 {
		return assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, readyAssigned
	}

	groups := make(map[claimedWorkRefreshKey][]string)
	indexByKey := make(map[claimedWorkRefreshIndexKey][]int)
	for _, candidate := range candidates {
		for _, idx := range candidate.indexes {
			if idx < 0 || idx >= len(assignedWorkBeads) {
				continue
			}
			if idx >= len(assignedWorkStores) || assignedWorkStores[idx] == nil {
				continue
			}
			ref := ""
			if idx < len(assignedWorkStoreRefs) {
				ref = assignedWorkStoreRefs[idx]
			}
			id := strings.TrimSpace(assignedWorkBeads[idx].ID)
			if id == "" {
				continue
			}
			key := claimedWorkRefreshKey{store: assignedWorkStores[idx], ref: ref}
			groups[key] = append(groups[key], id)
			indexKey := claimedWorkRefreshIndexKey{refresh: key, id: id}
			indexByKey[indexKey] = append(indexByKey[indexKey], idx)
		}
	}
	if len(groups) == 0 {
		return assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, readyAssigned
	}

	freshByIndex := make(map[int]beads.Bead)
	refreshedByIndex := make(map[int]struct{})
	deadline := time.Now().Add(claimedWorkRefreshTimeout)
	for key, ids := range groups {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			fmt.Fprintf(stderr, "claimed-work refresh: refreshing %s claimed bead(s) in %s: %v\n", strings.Join(ids, ","), claimedWorkRefreshStoreLabel(key.ref), errClaimedWorkRefreshTimeout) //nolint:errcheck
			continue
		}
		rows, err := refreshClaimedWorkIDs(key.store, ids, remaining)
		if err != nil {
			fmt.Fprintf(stderr, "claimed-work refresh: refreshing %s claimed bead(s) in %s: %v\n", strings.Join(ids, ","), claimedWorkRefreshStoreLabel(key.ref), err) //nolint:errcheck
			continue
		}
		for _, id := range compactClaimedWorkRefreshIDs(ids) {
			indexKey := claimedWorkRefreshIndexKey{refresh: key, id: id}
			for _, idx := range indexByKey[indexKey] {
				refreshedByIndex[idx] = struct{}{}
			}
		}
		for _, row := range rows {
			indexKey := claimedWorkRefreshIndexKey{refresh: key, id: row.ID}
			for _, idx := range indexByKey[indexKey] {
				freshByIndex[idx] = row
			}
		}
	}
	if len(refreshedByIndex) == 0 {
		return assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, readyAssigned
	}

	filteredWork := make([]beads.Bead, 0, len(assignedWorkBeads))
	filteredStores := make([]beads.Store, 0, len(assignedWorkStores))
	filteredRefs := make([]string, 0, len(assignedWorkStoreRefs))
	filteredReady := make(map[storeScopedBeadKey]bool, len(readyAssigned))
	for key, ready := range readyAssigned {
		filteredReady[key] = ready
	}
	for i, work := range assignedWorkBeads {
		original := work
		_, refreshed := refreshedByIndex[i]
		fresh, freshFound := freshByIndex[i]
		if freshFound {
			work = fresh
		}
		ref := ""
		if i < len(assignedWorkStoreRefs) {
			ref = assignedWorkStoreRefs[i]
		}
		originalKey := storeScopedBeadKey{StoreRef: ref, ID: original.ID}
		if refreshed {
			delete(filteredReady, originalKey)
			if !freshFound || strings.TrimSpace(work.Status) == "closed" || strings.TrimSpace(work.Assignee) == "" {
				continue
			}
		}
		filteredWork = append(filteredWork, work)
		if i < len(assignedWorkStores) {
			filteredStores = append(filteredStores, assignedWorkStores[i])
		} else {
			filteredStores = append(filteredStores, nil)
		}
		filteredRefs = append(filteredRefs, ref)
		if refreshed {
			key := storeScopedBeadKey{StoreRef: ref, ID: work.ID}
			if readyAssigned[originalKey] || strings.TrimSpace(work.Status) == "in_progress" {
				filteredReady[key] = true
			}
		}
	}
	return filteredWork, filteredStores, filteredRefs, filteredReady
}

func claimedWorkRefreshSessionMayBeRunning(info sessionpkg.Info) bool {
	switch strings.TrimSpace(string(info.State)) {
	case "", "active", "awake", "creating", "start-pending", "draining":
		return true
	default:
		return false
	}
}

func claimedWorkRefreshSessionExited(info sessionpkg.Info, sp runtime.Provider, snapshot *claimedWorkRefreshRunningSnapshot, stderr io.Writer) bool {
	sessionName := strings.TrimSpace(info.SessionNameMetadata)
	if sessionName == "" {
		return false
	}
	if !claimedWorkRefreshSessionMayBeRunning(info) {
		return true
	}
	if snapshot == nil {
		return false
	}
	if !snapshot.loaded {
		snapshot.loaded = true
		running, err := sp.ListRunning("")
		if err != nil {
			snapshot.err = err
			if stderr != nil {
				fmt.Fprintf(stderr, "claimed-work refresh: listing running sessions for exit check: %v\n", err) //nolint:errcheck
			}
			return false
		}
		snapshot.running = make(map[string]bool, len(running))
		for _, name := range running {
			snapshot.running[name] = true
		}
	}
	if snapshot.err != nil {
		return false
	}
	return !snapshot.running[sessionName]
}

func claimRefreshObservationFirstSeen(
	sessionName string,
	trigger string,
	indexes []int,
	assignedWorkBeads []beads.Bead,
	assignedWorkStores []beads.Store,
	assignedWorkStoreRefs []string,
) bool {
	key := claimRefreshObservationKey(sessionName, trigger, indexes, assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs)
	if key == "" {
		return false
	}
	claimedWorkRefreshObservations.Lock()
	defer claimedWorkRefreshObservations.Unlock()
	if claimedWorkRefreshObservations.seen == nil {
		claimedWorkRefreshObservations.seen = make(map[string]struct{})
	}
	if _, ok := claimedWorkRefreshObservations.seen[key]; ok {
		return false
	}
	claimedWorkRefreshObservations.seen[key] = struct{}{}
	return true
}

func claimRefreshObservationKey(
	sessionName string,
	trigger string,
	indexes []int,
	assignedWorkBeads []beads.Bead,
	assignedWorkStores []beads.Store,
	assignedWorkStoreRefs []string,
) string {
	sessionName = strings.TrimSpace(sessionName)
	trigger = strings.TrimSpace(trigger)
	if sessionName == "" || trigger == "" || len(indexes) == 0 {
		return ""
	}
	claims := make([]string, 0, len(indexes))
	seen := make(map[string]struct{}, len(indexes))
	for _, idx := range indexes {
		if idx < 0 || idx >= len(assignedWorkBeads) {
			continue
		}
		id := strings.TrimSpace(assignedWorkBeads[idx].ID)
		if id == "" {
			continue
		}
		ref := ""
		if idx < len(assignedWorkStoreRefs) {
			ref = assignedWorkStoreRefs[idx]
		}
		storeID := ""
		if idx < len(assignedWorkStores) {
			storeID = claimedWorkRefreshStoreIdentity(assignedWorkStores[idx])
		}
		claim := storeID + "\x00" + ref + "\x00" + id
		if _, ok := seen[claim]; ok {
			continue
		}
		seen[claim] = struct{}{}
		claims = append(claims, claim)
	}
	if len(claims) == 0 {
		return ""
	}
	sort.Strings(claims)
	return sessionName + "\x00" + trigger + "\x00" + strings.Join(claims, "\x01")
}

func refreshClaimedWorkIDs(store beads.Store, ids []string, timeout time.Duration) ([]beads.Bead, error) {
	if store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	ids = compactClaimedWorkRefreshIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	if timeout <= 0 {
		return nil, errClaimedWorkRefreshTimeout
	}
	storeKey := claimedWorkRefreshStoreIdentity(store)
	flight := &claimedWorkRefreshFlight{done: make(chan struct{})}
	actual, loaded := claimedWorkRefreshFlights.LoadOrStore(storeKey, flight)
	if loaded {
		if existing, ok := actual.(*claimedWorkRefreshFlight); ok {
			select {
			case <-existing.done:
			default:
				return nil, errClaimedWorkRefreshTimeout
			}
		}
		return nil, errClaimedWorkRefreshTimeout
	}
	go func() {
		defer close(flight.done)
		defer claimedWorkRefreshFlights.Delete(storeKey)
		flight.rows, flight.err = readClaimedWorkIDsTargeted(store, ids)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		if flight.err != nil {
			return nil, flight.err
		}
		allowed := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			allowed[id] = struct{}{}
		}
		filtered := flight.rows[:0]
		for _, row := range flight.rows {
			if _, ok := allowed[row.ID]; ok {
				filtered = append(filtered, row)
			}
		}
		return filtered, nil
	case <-timer.C:
		return nil, errClaimedWorkRefreshTimeout
	}
}

type claimedWorkIDRefresher interface {
	RefreshIDs(ids []string) ([]beads.Bead, error)
}

func readClaimedWorkIDsTargeted(store beads.Store, ids []string) ([]beads.Bead, error) {
	if refresher, ok := store.(claimedWorkIDRefresher); ok {
		return refresher.RefreshIDs(ids)
	}
	rows := make([]beads.Bead, 0, len(ids))
	for _, id := range ids {
		row, err := store.Get(id)
		switch {
		case err == nil:
			rows = append(rows, row)
		case errors.Is(err, beads.ErrNotFound):
			continue
		default:
			return nil, err
		}
	}
	return rows, nil
}

func compactClaimedWorkRefreshIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func claimedWorkRefreshStoreIdentity(store beads.Store) string {
	if store == nil {
		return "<nil>"
	}
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Ptr, reflect.Slice, reflect.UnsafePointer:
		if value.IsNil() {
			return fmt.Sprintf("%T:nil", store)
		}
		return fmt.Sprintf("%T:%x", store, value.Pointer())
	default:
		return fmt.Sprintf("%T:%v", store, store)
	}
}

func claimedWorkRefreshStoreLabel(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "city"
	}
	return ref
}
