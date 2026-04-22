package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

const (
	waitBeadType  = sessionpkg.WaitBeadType
	waitBeadLabel = sessionpkg.WaitBeadLabel

	waitStatePending  = "pending"
	waitStateReady    = "ready"
	waitStateClosed   = "closed"
	waitStateCanceled = "canceled"
	waitStateExpired  = "expired"
	waitStateFailed   = "failed"
)

func newWaitCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Inspect and manage durable session waits",
	}
	cmd.AddCommand(
		newWaitListCmd(stdout, stderr),
		newWaitInspectCmd(stdout, stderr),
		newWaitCancelCmd(stdout, stderr),
		newWaitReadyCmd(stdout, stderr),
	)
	return cmd
}

func newSessionWaitCmd(stdout, stderr io.Writer) *cobra.Command {
	var depIDs []string
	var matchAny bool
	var note string
	var sleep bool
	cmd := &cobra.Command{
		Use:   "wait [session-id-or-alias]",
		Short: "Register a dependency wait for a session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionWait(args, depIDs, matchAny, note, sleep, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().StringSliceVar(&depIDs, "on-beads", nil, "bead IDs to watch")
	cmd.Flags().BoolVar(&matchAny, "any", false, "wake when any watched bead closes (default: all)")
	cmd.Flags().StringVar(&note, "note", "", "reminder text delivered when the wait is satisfied")
	cmd.Flags().BoolVar(&sleep, "sleep", false, "set wait hold so the session can drain to sleep")
	return cmd
}

func newWaitListCmd(stdout, stderr io.Writer) *cobra.Command {
	var stateFilter string
	var sessionFilter string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List durable waits",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdWaitList(stateFilter, sessionFilter, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stateFilter, "state", "", "filter by wait state")
	cmd.Flags().StringVar(&sessionFilter, "session", "", "filter by session ID")
	return cmd
}

func newWaitInspectCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <wait-id>",
		Short: "Show details for a wait",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdWaitInspect(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newWaitCancelCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <wait-id>",
		Short: "Cancel a wait",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdWaitSetState(args[0], waitStateCanceled, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newWaitReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "ready <wait-id>",
		Short: "Manually mark a wait ready",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdWaitSetState(args[0], waitStateReady, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdSessionWait(args, depIDs []string, matchAny bool, note string, sleep bool, stdout, stderr io.Writer) int {
	if len(depIDs) == 0 {
		fmt.Fprintln(stderr, "gc session wait: at least one --on-beads value is required") //nolint:errcheck
		return 1
	}
	if strings.TrimSpace(note) == "" {
		fmt.Fprintln(stderr, "gc session wait: --note is required") //nolint:errcheck
		return 1
	}
	store, code := openCityStore(stderr, "gc session wait")
	if store == nil {
		return code
	}
	target := ""
	if len(args) > 0 {
		target = args[0]
	} else {
		target = os.Getenv("GC_SESSION_ID")
	}
	if target == "" {
		fmt.Fprintln(stderr, "gc session wait: session not specified (pass an ID/name or set $GC_SESSION_ID)") //nolint:errcheck
		return 1
	}
	if err := waitLifecycleEnabled(); err != nil {
		fmt.Fprintf(stderr, "gc session wait: %v\n", err) //nolint:errcheck
		return 1
	}
	if sleep {
		cityPath, err := resolveCity()
		if err != nil || !cityUsesManagedReconciler(cityPath) {
			fmt.Fprintln(stderr, "gc session wait: a managed controller must be running when --sleep is used") //nolint:errcheck
			return 1
		}
	}
	cityPath, cityErr := resolveCity()
	var cfg *config.City
	if cityErr == nil {
		cfg, _ = loadCityConfig(cityPath, stderr)
	}
	sessionID, err := resolveSessionIDWithConfig(cityPath, cfg, store, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc session wait: %v\n", err) //nolint:errcheck
		return 1
	}
	sb, err := store.Get(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session wait: %v\n", err) //nolint:errcheck
		return 1
	}
	for _, depID := range depIDs {
		if _, err := loadWaitDependencyBead(cityPath, store, depID); err != nil {
			fmt.Fprintf(stderr, "gc session wait: dependency %s: %v\n", depID, err) //nolint:errcheck
			return 1
		}
	}
	state := waitStatePending
	now := time.Now().UTC()
	meta := map[string]string{
		"session_id":         sessionID,
		"session_name":       sb.Metadata["session_name"],
		"kind":               "deps",
		"state":              state,
		"dep_ids":            strings.Join(depIDs, ","),
		"dep_mode":           "all",
		"registered_epoch":   sb.Metadata["continuation_epoch"],
		"delivery_attempt":   "1",
		"created_by_session": os.Getenv("GC_SESSION_ID"),
		"created_at":         now.Format(time.RFC3339),
	}
	if matchAny {
		meta["dep_mode"] = "any"
	}
	waitBead, err := store.Create(beads.Bead{
		Title:       "wait:" + sb.Title,
		Type:        waitBeadType,
		Description: note,
		Labels: []string{
			waitBeadLabel,
			"session:" + sessionID,
		},
		Metadata: meta,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc session wait: creating wait: %v\n", err) //nolint:errcheck
		return 1
	}
	ready, depErr := depsWaitReadyDetailedForCity(cityPath, store, waitBead)
	if depErr != nil {
		if err := setWaitTerminalState(store, waitBead.ID, map[string]string{
			"state":      waitStateFailed,
			"failed_at":  now.Format(time.RFC3339),
			"last_error": depErr.Error(),
		}); err != nil {
			fmt.Fprintf(stderr, "gc session wait: setting failed state: %v\n", err) //nolint:errcheck
		}
		fmt.Fprintf(stderr, "gc session wait: dependency state check: %v\n", depErr) //nolint:errcheck
		return 1
	}
	if ready {
		if err := store.SetMetadataBatch(waitBead.ID, map[string]string{
			"state":    waitStateReady,
			"ready_at": now.Format(time.RFC3339),
		}); err != nil {
			fmt.Fprintf(stderr, "gc session wait: setting ready state: %v\n", err) //nolint:errcheck
			return 1
		}
		fmt.Fprintf(stdout, "Registered wait %s for session %s (already ready).\n", waitBead.ID, sessionID) //nolint:errcheck
		return 0
	}
	if sleep {
		if err := store.SetMetadataBatch(sessionID, map[string]string{
			"wait_hold":    "true",
			"sleep_intent": "wait-hold",
		}); err != nil {
			fmt.Fprintf(stderr, "gc session wait: setting wait hold: %v\n", err) //nolint:errcheck
			return 1
		}
		if cityPath, err := resolveCity(); err == nil {
			if err := pokeController(cityPath); err != nil {
				fmt.Fprintf(stderr, "gc session wait: poking controller: %v\n", err) //nolint:errcheck
				return 1
			}
		}
		fmt.Fprintf(stdout, "Registered wait %s for session %s.\nSession %s draining to sleep.\n", waitBead.ID, sessionID, sessionID) //nolint:errcheck
		return 0
	}
	fmt.Fprintf(stdout, "Registered wait %s for session %s.\n", waitBead.ID, sessionID) //nolint:errcheck
	return 0
}

func cmdWaitList(stateFilter, sessionFilter string, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc wait list")
	if store == nil {
		return code
	}
	items, err := loadWaitBeads(store)
	if err != nil {
		fmt.Fprintf(stderr, "gc wait list: %v\n", err) //nolint:errcheck
		return 1
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WAIT\tSESSION\tSTATE\tKIND\tNOTE") //nolint:errcheck
	for _, item := range items {
		if stateFilter != "" && item.Metadata["state"] != stateFilter {
			continue
		}
		if sessionFilter != "" && item.Metadata["session_id"] != sessionFilter {
			continue
		}
		note := item.Description
		if note == "" {
			note = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", item.ID, item.Metadata["session_id"], item.Metadata["state"], item.Metadata["kind"], note) //nolint:errcheck
	}
	_ = tw.Flush()
	return 0
}

func cmdWaitInspect(waitID string, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc wait inspect")
	if store == nil {
		return code
	}
	b, err := store.Get(waitID)
	if err != nil {
		fmt.Fprintf(stderr, "gc wait inspect: %v\n", err) //nolint:errcheck
		return 1
	}
	if !sessionpkg.IsWaitBead(b) {
		fmt.Fprintf(stderr, "gc wait inspect: %s is not a wait\n", waitID) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "Wait:       %s\n", b.ID)                                               //nolint:errcheck
	fmt.Fprintf(stdout, "Session:    %s\n", b.Metadata["session_id"])                           //nolint:errcheck
	fmt.Fprintf(stdout, "State:      %s\n", b.Metadata["state"])                                //nolint:errcheck
	fmt.Fprintf(stdout, "Kind:       %s\n", b.Metadata["kind"])                                 //nolint:errcheck
	fmt.Fprintf(stdout, "Deps:       %s (%s)\n", b.Metadata["dep_ids"], b.Metadata["dep_mode"]) //nolint:errcheck
	fmt.Fprintf(stdout, "Epoch:      %s\n", b.Metadata["registered_epoch"])                     //nolint:errcheck
	fmt.Fprintf(stdout, "Attempt:    %s\n", b.Metadata["delivery_attempt"])                     //nolint:errcheck
	fmt.Fprintf(stdout, "Nudge:      %s\n", b.Metadata["nudge_id"])                             //nolint:errcheck
	fmt.Fprintf(stdout, "Note:       %s\n", b.Description)                                      //nolint:errcheck
	return 0
}

func cmdWaitSetState(waitID, state string, stdout, stderr io.Writer) int {
	store, code := openCityStore(stderr, "gc wait")
	if store == nil {
		return code
	}
	b, err := store.Get(waitID)
	if err != nil {
		fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
		return 1
	}
	if !sessionpkg.IsWaitBead(b) {
		fmt.Fprintf(stderr, "gc wait: %s is not a wait\n", waitID) //nolint:errcheck
		return 1
	}
	if state == waitStateReady {
		if err := waitLifecycleEnabled(); err != nil {
			fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if state == waitStateReady && b.Status == "closed" {
		retried, err := retryClosedWait(store, b, now)
		if err != nil {
			fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
			return 1
		}
		fmt.Fprintf(stdout, "Retried wait %s as %s.\n", waitID, retried.ID) //nolint:errcheck
		return 0
	}
	batch := map[string]string{"state": state}
	switch state {
	case waitStateReady:
		batch["ready_at"] = now
		nextAttempt, err := nextWaitDeliveryAttempt(store, b)
		if err != nil {
			fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
			return 1
		}
		if nextAttempt != "" {
			batch["delivery_attempt"] = nextAttempt
			batch["nudge_id"] = ""
			batch["commit_boundary"] = ""
			batch["last_error"] = ""
			batch["closed_at"] = ""
			batch["failed_at"] = ""
			batch["expired_at"] = ""
			batch["canceled_at"] = ""
		}
	case waitStateCanceled:
		batch["canceled_at"] = now
	}
	apply := store.SetMetadataBatch
	if state == waitStateCanceled {
		apply = func(id string, kv map[string]string) error {
			return setWaitTerminalState(store, id, kv)
		}
	}
	if err := apply(waitID, batch); err != nil {
		fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
		return 1
	}
	if state == waitStateCanceled {
		if cityPath, err := resolveCity(); err == nil {
			if err := withdrawQueuedWaitNudges(cityPath, []string{b.Metadata["nudge_id"]}); err != nil {
				fmt.Fprintf(stderr, "gc wait: withdrawing queued nudge: %v\n", err) //nolint:errcheck
				return 1
			}
		}
		if err := clearSessionWaitHoldIfIdle(store, b.Metadata["session_id"]); err != nil {
			fmt.Fprintf(stderr, "gc wait: clearing session wait hold: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	fmt.Fprintf(stdout, "Updated wait %s to %s.\n", waitID, state) //nolint:errcheck
	return 0
}

func loadWaitBeads(store beads.Store) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	return loadWaitBeadsByLabel(store, waitBeadLabel)
}

func loadSessionWaitBeads(store beads.Store, sessionID string) ([]beads.Bead, error) {
	if store == nil || sessionID == "" {
		return nil, nil
	}
	return loadWaitBeadsByLabel(store, "session:"+sessionID)
}

func waitNudgeIDsForSession(store beads.Store, sessionID string) ([]string, error) {
	return sessionpkg.WaitNudgeIDs(store, sessionID)
}

func loadWaitBeadsByLabel(store beads.Store, label string) ([]beads.Bead, error) {
	all, err := store.List(beads.ListQuery{Label: label})
	if err != nil {
		return nil, err
	}
	result := make([]beads.Bead, 0, len(all))
	for _, item := range all {
		if item.Status == "closed" {
			continue
		}
		if !sessionpkg.IsWaitBead(item) {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}

func depsWaitReady(store beads.Store, wait beads.Bead) bool {
	ready, err := depsWaitReadyDetailed(store, wait)
	return err == nil && ready
}

func depsWaitReadyDetailed(store beads.Store, wait beads.Bead) (bool, error) {
	return depsWaitReadyDetailedForCity("", store, wait)
}

func depsWaitReadyDetailedForCity(cityPath string, store beads.Store, wait beads.Bead) (bool, error) {
	rawDepIDs := strings.Split(wait.Metadata["dep_ids"], ",")
	depIDs := make([]string, 0, len(rawDepIDs))
	for _, depID := range rawDepIDs {
		depID = strings.TrimSpace(depID)
		if depID != "" {
			depIDs = append(depIDs, depID)
		}
	}
	if len(depIDs) == 0 {
		return false, nil
	}
	mode := wait.Metadata["dep_mode"]
	closedCount := 0
	foundAny := false
	var missingErr error
	for _, depID := range depIDs {
		dep, err := loadWaitDependencyBead(cityPath, store, depID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				if mode != "any" {
					return false, fmt.Errorf("dependency %s: %w", depID, err)
				}
				if missingErr == nil {
					missingErr = fmt.Errorf("dependency %s: %w", depID, err)
				}
				continue
			}
			return false, fmt.Errorf("dependency %s: %w", depID, err)
		}
		foundAny = true
		if dep.Status == "closed" {
			closedCount++
			if mode == "any" {
				return true, nil
			}
		}
	}
	if mode == "any" {
		if !foundAny && missingErr != nil {
			return false, missingErr
		}
		return false, nil
	}
	return closedCount == len(depIDs), nil
}

func loadWaitDependencyBead(cityPath string, cityStore beads.Store, depID string) (beads.Bead, error) {
	if strings.TrimSpace(cityPath) == "" {
		if cityStore == nil {
			return beads.Bead{}, beads.ErrNotFound
		}
		return cityStore.Get(depID)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return beads.Bead{}, err
	}
	cityRoot := filepath.Clean(cityPath)
	for _, scopeRoot := range convoyStoreCandidates(cfg, cityPath, depID) {
		scopeRoot = resolveStoreScopeRoot(cityPath, scopeRoot)
		if scopeRoot == cityRoot && cityStore != nil {
			dep, err := cityStore.Get(depID)
			if err == nil {
				return dep, nil
			}
			if !errors.Is(err, beads.ErrNotFound) {
				return beads.Bead{}, err
			}
			continue
		}
		scopeStore, err := openStoreAtForCity(scopeRoot, cityPath)
		if err != nil {
			continue
		}
		dep, err := scopeStore.Get(depID)
		if err == nil {
			return dep, nil
		}
		if !errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, err
		}
	}
	return beads.Bead{}, beads.ErrNotFound
}

func retryableWaitMetadata(src map[string]string) map[string]string {
	if src["kind"] != "deps" {
		meta := make(map[string]string, len(src))
		for key, value := range src {
			if value == "" {
				continue
			}
			meta[key] = value
		}
		return meta
	}
	keys := []string{
		"session_id",
		"session_name",
		"kind",
		"dep_ids",
		"dep_mode",
		"registered_epoch",
		"created_by_session",
		"expires_at",
	}
	meta := make(map[string]string, len(keys)+8)
	for _, key := range keys {
		if value := src[key]; value != "" {
			meta[key] = value
		}
	}
	return meta
}

func prepareWaitWakeState(store beads.Store, now time.Time) (map[string]bool, error) {
	return prepareWaitWakeStateForCity("", store, now)
}

func prepareWaitWakeStateForCity(cityPath string, store beads.Store, now time.Time) (map[string]bool, error) {
	return prepareWaitWakeStateForCityWithSnapshot(cityPath, store, now, nil)
}

func prepareWaitWakeStateForCityWithSnapshot(cityPath string, store beads.Store, now time.Time, sessionBeads *sessionBeadSnapshot) (map[string]bool, error) {
	waits, err := loadWaitBeads(store)
	if err != nil {
		return nil, err
	}
	if sessionBeads == nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(store)
		if err != nil {
			return nil, err
		}
	}
	readyWaitSet := make(map[string]bool)
	for _, wait := range waits {
		state := wait.Metadata["state"]
		sessionID := wait.Metadata["session_id"]
		if sessionID == "" {
			continue
		}
		if isWaitTerminal(state) {
			continue
		}
		sessionBead, ok := sessionBeads.FindByID(sessionID)
		if !ok {
			if wait.Metadata["registered_epoch"] != "" {
				var found bool
				sessionBead, found, err = lookupSessionBeadByID(store, sessionID)
				if err != nil {
					return nil, err
				}
				if !found {
					continue
				}
			} else {
				continue
			}
		}
		if epoch := wait.Metadata["registered_epoch"]; epoch != "" && sessionBead.Metadata["continuation_epoch"] != "" && epoch != sessionBead.Metadata["continuation_epoch"] {
			if err := setWaitTerminalState(store, wait.ID, map[string]string{
				"state":       waitStateCanceled,
				"canceled_at": now.UTC().Format(time.RFC3339),
				"last_error":  "continuation-stale",
			}); err != nil {
				return nil, err
			}
			if err := clearSessionWaitHoldIfIdle(store, sessionID); err != nil {
				return nil, err
			}
			continue
		}
		if !ok {
			continue
		}
		if expiresAt := wait.Metadata["expires_at"]; expiresAt != "" {
			if ts, err := time.Parse(time.RFC3339, expiresAt); err == nil && !ts.After(now) {
				if err := setWaitTerminalState(store, wait.ID, map[string]string{
					"state":      waitStateExpired,
					"expired_at": now.UTC().Format(time.RFC3339),
				}); err != nil {
					return nil, err
				}
				if err := clearSessionWaitHoldIfIdle(store, sessionID); err != nil {
					return nil, err
				}
				continue
			}
		}
		if state == waitStateReady {
			done, err := finalizeReadyWaitFromNudge(store, wait, now)
			if err != nil {
				return nil, err
			}
			if done {
				if err := clearSessionWaitHoldIfIdle(store, sessionID); err != nil {
					return nil, err
				}
				continue
			}
			readyWaitSet[sessionID] = true
			continue
		}
		if wait.Metadata["kind"] != "deps" {
			continue
		}
		ready, depErr := depsWaitReadyDetailedForCity(cityPath, store, wait)
		if depErr != nil {
			if errors.Is(depErr, beads.ErrNotFound) {
				if err := setWaitTerminalState(store, wait.ID, map[string]string{
					"state":      waitStateFailed,
					"failed_at":  now.UTC().Format(time.RFC3339),
					"last_error": depErr.Error(),
				}); err != nil {
					return nil, err
				}
				if err := clearSessionWaitHoldIfIdle(store, sessionID); err != nil {
					return nil, err
				}
				continue
			}
			return nil, depErr
		}
		if ready {
			if err := store.SetMetadataBatch(wait.ID, map[string]string{
				"state":    waitStateReady,
				"ready_at": now.UTC().Format(time.RFC3339),
			}); err != nil {
				return nil, err
			}
			readyWaitSet[sessionID] = true
		}
	}
	return readyWaitSet, nil
}

func lookupSessionBeadByID(store beads.Store, id string) (beads.Bead, bool, error) {
	if store == nil || strings.TrimSpace(id) == "" {
		return beads.Bead{}, false, nil
	}
	bead, err := store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, false, nil
		}
		return beads.Bead{}, false, err
	}
	if !sessionpkg.IsSessionBeadOrRepairable(bead) {
		return beads.Bead{}, false, nil
	}
	return bead, true, nil
}

func dispatchReadyWaitNudges(cityPath string, store beads.Store, _ runtime.Provider, now time.Time) error {
	return dispatchReadyWaitNudgesWithSnapshot(cityPath, store, now, nil)
}

func dispatchReadyWaitNudgesWithSnapshot(cityPath string, store beads.Store, now time.Time, sessionBeads *sessionBeadSnapshot) error {
	waits, err := loadWaitBeads(store)
	if err != nil {
		return err
	}
	if sessionBeads == nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(store)
		if err != nil {
			return err
		}
	}
	for _, wait := range waits {
		if wait.Metadata["state"] != waitStateReady {
			continue
		}
		sessionID := wait.Metadata["session_id"]
		if sessionID == "" {
			continue
		}
		sessionBead, ok := sessionBeads.FindByID(sessionID)
		if !ok {
			continue
		}
		if !cachedSessionCanReceiveWaitNudge(sessionBead) {
			continue
		}
		nudgeID := waitNudgeID(wait)
		if nudgeID == "" {
			continue
		}
		if _, ok, err := findQueuedNudgeBead(store, nudgeID); err == nil && ok {
			continue
		}
		message := strings.TrimSpace(wait.Description)
		if message == "" {
			message = "Wait satisfied."
		}
		message = fmt.Sprintf("Wait satisfied (%s): %s", wait.ID, message)
		item := newQueuedNudgeWithOptions(waitNudgeAgent(sessionBead), message, "wait", now, queuedNudgeOptions{
			ID:                nudgeID,
			SessionID:         sessionID,
			ContinuationEpoch: wait.Metadata["registered_epoch"],
			Reference:         &nudgeReference{Kind: "bead", ID: wait.ID},
		})
		if err := enqueueQueuedNudgeWithStore(cityPath, store, item); err != nil {
			return err
		}
		if err := store.SetMetadata(wait.ID, "nudge_id", nudgeID); err != nil {
			return fmt.Errorf("setting wait nudge_id: %w", err)
		}
		// provider_kind is stamped from ResolvedProvider.Kind /
		// BuiltinAncestor at session-bead creation, so wrapped codex
		// aliases (e.g. [providers.my-wrapped-codex] base = "builtin:codex")
		// already surface as "codex" here. The provider fallback covers
		// sessions created before provider_kind was stamped.
		if sessionProviderFamily(sessionBead) == "codex" {
			if err := startNudgePoller(cityPath, waitNudgeAgent(sessionBead), sessionBead.Metadata["session_name"]); err != nil {
				return fmt.Errorf("starting wait nudge poller: %w", err)
			}
		}
	}
	return nil
}

func cachedSessionCanReceiveWaitNudge(sessionBead beads.Bead) bool {
	switch sessionpkg.State(strings.TrimSpace(sessionBead.Metadata["state"])) {
	case "", sessionpkg.StateActive, sessionpkg.StateAwake:
		return true
	default:
		return false
	}
}

func finalizeReadyWaitFromNudge(store beads.Store, wait beads.Bead, now time.Time) (bool, error) {
	nudgeID := wait.Metadata["nudge_id"]
	if nudgeID == "" {
		nudgeID = waitNudgeID(wait)
	}
	if nudgeID == "" {
		return false, nil
	}
	nudge, ok, err := findAnyQueuedNudgeBead(store, nudgeID)
	if err != nil || !ok {
		return false, err
	}
	switch nudge.Metadata["state"] {
	case "injected", "accepted_for_injection":
		return true, setWaitTerminalState(store, wait.ID, map[string]string{
			"state":           waitStateClosed,
			"closed_at":       now.UTC().Format(time.RFC3339),
			"nudge_id":        nudgeID,
			"commit_boundary": nudge.Metadata["commit_boundary"],
		})
	case "expired", "failed":
		return true, setWaitTerminalState(store, wait.ID, map[string]string{
			"state":           waitStateFailed,
			"failed_at":       now.UTC().Format(time.RFC3339),
			"nudge_id":        nudgeID,
			"last_error":      nudge.Metadata["terminal_reason"],
			"commit_boundary": nudge.Metadata["commit_boundary"],
		})
	default:
		return false, nil
	}
}

func cancelWaitsForSession(store beads.Store, sessionID string) error {
	if store == nil || sessionID == "" {
		return nil
	}
	waits, err := loadSessionWaitBeads(store, sessionID)
	if err != nil {
		return err
	}
	nudgeIDs := make([]string, 0, len(waits))
	for _, wait := range waits {
		if wait.Metadata["session_id"] != sessionID {
			continue
		}
		if isWaitTerminal(wait.Metadata["state"]) {
			continue
		}
		if nudgeID := wait.Metadata["nudge_id"]; nudgeID != "" {
			nudgeIDs = append(nudgeIDs, nudgeID)
		}
	}
	if err := sessionpkg.CancelWaits(store, sessionID, time.Now().UTC()); err != nil {
		return err
	}
	if cityPath, err := resolveCity(); err == nil {
		if err := withdrawQueuedWaitNudges(cityPath, nudgeIDs); err != nil {
			return err
		}
	}
	return nil
}

func clearSessionWaitHold(store beads.Store, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	batch := map[string]string{
		"wait_hold":    "",
		"sleep_intent": "",
	}
	if store != nil {
		if sessionBead, err := store.Get(sessionID); err == nil && sessionBead.Metadata["sleep_reason"] == "wait-hold" {
			batch["sleep_reason"] = ""
		}
	}
	return store.SetMetadataBatch(sessionID, batch)
}

func clearSessionWaitHoldIfIdle(store beads.Store, sessionID string) error {
	hasWaits, err := hasNonTerminalWaits(store, sessionID)
	if err != nil {
		return err
	}
	if hasWaits {
		return nil
	}
	return clearSessionWaitHold(store, sessionID)
}

func hasNonTerminalWaits(store beads.Store, sessionID string) (bool, error) {
	waits, err := loadWaitBeads(store)
	if err != nil {
		return false, err
	}
	for _, wait := range waits {
		if wait.Metadata["session_id"] != sessionID {
			continue
		}
		if !isWaitTerminal(wait.Metadata["state"]) {
			return true, nil
		}
	}
	return false, nil
}

func isWaitTerminal(state string) bool {
	return sessionpkg.IsWaitTerminalState(state)
}

func waitNudgeID(wait beads.Bead) string {
	attempt := wait.Metadata["delivery_attempt"]
	if attempt == "" {
		attempt = "1"
	}
	epoch := wait.Metadata["registered_epoch"]
	if epoch == "" {
		epoch = "0"
	}
	return "wait-" + strings.ReplaceAll(wait.ID, "/", "-") + "-" + epoch + "-" + attempt
}

func waitNudgeAgent(sessionBead beads.Bead) string {
	if agent := sessionBead.Metadata["agent_name"]; agent != "" {
		return agent
	}
	return sessionBead.Metadata["template"]
}

// sessionProviderFamily returns the built-in provider family for a session
// bead. Preference order matches internal/session.providerKind:
//  1. builtin_ancestor — stamped from ResolvedProvider.BuiltinAncestor
//     at session-bead creation for explicit-base custom providers.
//  2. provider_kind — stamped for command-matched legacy aliases.
//  3. provider — raw provider metadata, last-resort fallback.
//
// Call sites that branch on provider family MUST consume this helper
// instead of reading the provider field directly so wrapped custom
// aliases behave like their built-in ancestor.
func sessionProviderFamily(sessionBead beads.Bead) string {
	if ancestor := strings.TrimSpace(sessionBead.Metadata["builtin_ancestor"]); ancestor != "" {
		return ancestor
	}
	if kind := strings.TrimSpace(sessionBead.Metadata["provider_kind"]); kind != "" {
		return kind
	}
	return strings.TrimSpace(sessionBead.Metadata["provider"])
}

func setWaitTerminalState(store beads.Store, waitID string, batch map[string]string) error {
	if err := store.SetMetadataBatch(waitID, batch); err != nil {
		return err
	}
	return store.Close(waitID)
}

func retryClosedWait(store beads.Store, wait beads.Bead, now string) (beads.Bead, error) {
	nextAttempt, err := nextWaitDeliveryAttempt(store, wait)
	if err != nil {
		return beads.Bead{}, err
	}
	if nextAttempt == "" {
		nextAttempt = wait.Metadata["delivery_attempt"]
		if nextAttempt == "" {
			nextAttempt = "1"
		}
	}
	meta := retryableWaitMetadata(wait.Metadata)
	meta["state"] = waitStateReady
	meta["ready_at"] = now
	meta["delivery_attempt"] = nextAttempt
	meta["nudge_id"] = ""
	meta["commit_boundary"] = ""
	meta["last_error"] = ""
	meta["closed_at"] = ""
	meta["failed_at"] = ""
	meta["expired_at"] = ""
	meta["canceled_at"] = ""
	meta["created_at"] = now
	meta["retried_from_wait"] = wait.ID
	if sessionID := wait.Metadata["session_id"]; sessionID != "" && store != nil {
		if sessionBead, err := store.Get(sessionID); err == nil {
			if epoch := sessionBead.Metadata["continuation_epoch"]; epoch != "" {
				meta["registered_epoch"] = epoch
			}
			if meta["session_name"] == "" {
				meta["session_name"] = sessionBead.Metadata["session_name"]
			}
		}
	}
	return store.Create(beads.Bead{
		Title:       wait.Title,
		Type:        wait.Type,
		Description: wait.Description,
		Labels:      append([]string(nil), wait.Labels...),
		Metadata:    meta,
	})
}

func nextWaitDeliveryAttempt(store beads.Store, wait beads.Bead) (string, error) {
	state := wait.Metadata["state"]
	if state == waitStatePending || state == waitStateReady {
		return "", nil
	}
	attempt, err := strconv.Atoi(wait.Metadata["delivery_attempt"])
	if err != nil || attempt <= 0 {
		attempt = 1
	}
	nudgeID := wait.Metadata["nudge_id"]
	if nudgeID == "" {
		nudgeID = waitNudgeID(wait)
	}
	if nudgeID == "" || store == nil {
		return strconv.Itoa(attempt + 1), nil
	}
	nudge, ok, err := findAnyQueuedNudgeBead(store, nudgeID)
	if err != nil {
		return "", err
	}
	if !ok || isTerminalNudgeState(nudge.Metadata["state"]) {
		return strconv.Itoa(attempt + 1), nil
	}
	return "", nil
}

func isTerminalNudgeState(state string) bool {
	switch state {
	case "accepted_for_injection", "injected", "expired", "failed", "superseded":
		return true
	default:
		return false
	}
}

func withdrawQueuedWaitNudges(cityPath string, nudgeIDs []string) error {
	return nudgequeue.WithdrawWaitNudges(openNudgeBeadStore(cityPath), cityPath, nudgeIDs)
}

func waitLifecycleEnabled() error {
	cityPath, err := resolveCity()
	if err != nil {
		return err
	}
	// Validate config loads successfully. The bead reconciler is always
	// enabled now (legacy reconciler removed), so this just confirms
	// the city is usable.
	_, _, err = config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	return err
}
