package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/api"
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

type waitSetStateResult struct {
	WaitID      string
	ReadyWaitID string
	Retried     bool
	RetriedFrom string
}

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
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List durable waits",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdWaitList(stateFilter, sessionFilter, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&stateFilter, "state", "", "filter by wait state")
	cmd.Flags().StringVar(&sessionFilter, "session", "", "filter by session ID")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return cmd
}

func newWaitInspectCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "inspect <wait-id>",
		Short: "Show details for a wait",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdWaitInspect(args[0], jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return cmd
}

func newWaitCancelCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel <wait-id>",
		Short: "Cancel a wait",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOutput {
				result, code := cmdWaitSetStateResult(args[0], waitStateCanceled, io.Discard, stderr)
				if code != 0 {
					return errExit
				}
				return writeManagementActionJSON(stdout, managementActionResult{
					Command: commandName("wait", "cancel"),
					Action:  "cancel",
					Name:    result.WaitID,
					State:   waitStateCanceled,
				})
			}
			if cmdWaitSetState(args[0], waitStateCanceled, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	return cmd
}

func newWaitReadyCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "ready <wait-id>",
		Short: "Manually mark a wait ready",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOutput {
				result, code := cmdWaitSetStateResult(args[0], waitStateReady, io.Discard, stderr)
				if code != 0 {
					return errExit
				}
				payload := managementActionResult{
					Command: commandName("wait", "ready"),
					Action:  "ready",
					Name:    result.WaitID,
					State:   waitStateReady,
				}
				if result.Retried {
					payload.Retried = managementBoolPtr(true)
					payload.RetriedFrom = result.RetriedFrom
					payload.ReadyWaitID = result.ReadyWaitID
				}
				return writeManagementActionJSON(stdout, payload)
			}
			if cmdWaitSetState(args[0], waitStateReady, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	return cmd
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
	sb, err := sessionFrontDoor(store).PersistedMarkers(sessionID)
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
		"session_name":       sb.SessionName,
		"kind":               "deps",
		"state":              state,
		"dep_ids":            strings.Join(depIDs, ","),
		"dep_mode":           "all",
		"registered_epoch":   sb.ContinuationEpoch,
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
		if err := sessionFrontDoor(store).ApplyPatch(sessionID, map[string]string{
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

func cmdWaitList(stateFilter, sessionFilter string, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc wait list: %v\n", err) //nolint:errcheck
		return 1
	}
	c, reason := waitListAPIClient(cityPath)
	return routeWaitList(cityPath, c, reason, stateFilter, sessionFilter, jsonOutput, stdout, stderr)
}

// waitListAPIClient is indirected so tests inject a client pointed at
// httptest.Server (or force a specific fallback reason) without spinning
// up a real controller.
var waitListAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeWaitList dispatches `gc wait list` through the supervisor API when a
// controller is up; otherwise falls back to the local store iterator.
// Exactly one route=... line per exit path (gated on GC_DEBUG).
//
// Wait beads are located via the generic beads endpoint using the
// sessionpkg.WaitBeadLabel contract: GET /v0/city/{name}/beads?label=gc:wait.
// The label constant is the shared invariant between CLI and server, so
// callers reference it rather than inlining the string.
func routeWaitList(cityPath string, c *api.Client, nilReason, stateFilter, sessionFilter string, jsonOutput bool, stdout, stderr io.Writer) int {
	const cmdName = "wait list"
	if c != nil {
		cr, err := c.ListBeads(api.ListBeadsOpts{
			Label: sessionpkg.WaitBeadLabel,
			Limit: 1000,
		})
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderWaitListFromAPI(cityPath, cr, stateFilter, sessionFilter, jsonOutput, stdout, stderr)
		}
		if !api.ShouldFallbackForRead(err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc wait list: %v\n", err) //nolint:errcheck
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doWaitListFallback(cityPath, stateFilter, sessionFilter, jsonOutput, stdout, stderr)
}

// renderWaitListFromAPI applies the same IsWaitBead + closed-excluded filter
// as the fallback path. The beads endpoint filters by label, not by type, so
// a stray non-wait bead tagged gc:wait would otherwise leak through. IsWaitBead
// also covers the legacy "wait" type for back-compat with older stores.
func renderWaitListFromAPI(cityPath string, cr api.CachedRead[[]beads.Bead], stateFilter, sessionFilter string, jsonOutput bool, stdout, stderr io.Writer) int {
	items := make([]beads.Bead, 0, len(cr.Body))
	for _, item := range cr.Body {
		if item.Status == "closed" {
			continue
		}
		if !sessionpkg.IsWaitBead(item) {
			continue
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	filtered := filterWaitListItems(items, stateFilter, sessionFilter)
	if jsonOutput {
		return writeWaitListJSON(stdout, stderr, cityPath, filtered)
	}
	writeWaitListTable(filtered, stdout)
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck
	}
	return 0
}

func doWaitListFallback(cityPath, stateFilter, sessionFilter string, jsonOutput bool, stdout, stderr io.Writer) int {
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "store_open_failed", fmt.Sprintf("gc wait list: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc wait list: %v\n", err)                  //nolint:errcheck
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck
		return 1
	}
	var items []beads.Bead
	if sessionFilter != "" {
		items, err = loadSessionWaitBeads(store, sessionFilter)
	} else {
		items, err = loadWaitBeads(store)
	}
	if err != nil {
		if !isWaitLookupLimitError(err) {
			fmt.Fprintf(stderr, "gc wait list: %v\n", err) //nolint:errcheck
			return 1
		}
		fmt.Fprintf(stderr, "gc wait list: %v; showing capped results\n", err) //nolint:errcheck
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	filtered := filterWaitListItems(items, stateFilter, "")
	if jsonOutput {
		return writeWaitListJSON(stdout, stderr, cityPath, filtered)
	}
	writeWaitListTable(filtered, stdout)
	return 0
}

func filterWaitListItems(items []beads.Bead, stateFilter, sessionFilter string) []beads.Bead {
	filtered := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		if stateFilter != "" && item.Metadata["state"] != stateFilter {
			continue
		}
		if sessionFilter != "" && item.Metadata["session_id"] != sessionFilter {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func writeWaitListTable(items []beads.Bead, stdout io.Writer) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WAIT\tSESSION\tSTATE\tKIND\tNOTE") //nolint:errcheck
	for _, item := range items {
		note := item.Description
		if note == "" {
			note = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", item.ID, item.Metadata["session_id"], item.Metadata["state"], item.Metadata["kind"], note) //nolint:errcheck
	}
	_ = tw.Flush()
}

func cmdWaitInspect(waitID string, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc wait inspect: %v\n", err) //nolint:errcheck
		return 1
	}
	c, reason := waitInspectAPIClient(cityPath)
	return routeWaitInspect(cityPath, c, reason, waitID, jsonOutput, stdout, stderr)
}

var waitInspectAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeWaitInspect dispatches `gc wait inspect <id>` through the supervisor
// API and falls back to a direct store lookup otherwise. Keeps the
// sessionpkg.IsWaitBead type guard on both paths so a non-wait bead ID does
// not render as a wait.
func routeWaitInspect(cityPath string, c *api.Client, nilReason, waitID string, jsonOutput bool, stdout, stderr io.Writer) int {
	const cmdName = "wait inspect"
	if c != nil {
		cr, err := c.GetBead(waitID)
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderWaitInspectFromAPI(cityPath, cr, waitID, jsonOutput, stdout, stderr)
		}
		if !api.ShouldFallbackForRead(err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc wait inspect: %v\n", err) //nolint:errcheck
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doWaitInspectFallback(cityPath, waitID, jsonOutput, stdout, stderr)
}

func renderWaitInspectFromAPI(cityPath string, cr api.CachedRead[beads.Bead], waitID string, jsonOutput bool, stdout, stderr io.Writer) int {
	if !sessionpkg.IsWaitBead(cr.Body) {
		fmt.Fprintf(stderr, "gc wait inspect: %s is not a wait\n", waitID) //nolint:errcheck
		return 1
	}
	if jsonOutput {
		return writeWaitInspectJSON(stdout, stderr, cityPath, cr.Body)
	}
	writeWaitDetail(cr.Body, stdout)
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck
	}
	return 0
}

func doWaitInspectFallback(cityPath, waitID string, jsonOutput bool, stdout, stderr io.Writer) int {
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "store_open_failed", fmt.Sprintf("gc wait inspect: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc wait inspect: %v\n", err)               //nolint:errcheck
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck
		return 1
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
	if jsonOutput {
		return writeWaitInspectJSON(stdout, stderr, cityPath, b)
	}
	writeWaitDetail(b, stdout)
	return 0
}

func writeWaitDetail(b beads.Bead, stdout io.Writer) {
	fmt.Fprintf(stdout, "Wait:       %s\n", b.ID)                                               //nolint:errcheck
	fmt.Fprintf(stdout, "Session:    %s\n", b.Metadata["session_id"])                           //nolint:errcheck
	fmt.Fprintf(stdout, "State:      %s\n", b.Metadata["state"])                                //nolint:errcheck
	fmt.Fprintf(stdout, "Kind:       %s\n", b.Metadata["kind"])                                 //nolint:errcheck
	fmt.Fprintf(stdout, "Deps:       %s (%s)\n", b.Metadata["dep_ids"], b.Metadata["dep_mode"]) //nolint:errcheck
	fmt.Fprintf(stdout, "Epoch:      %s\n", b.Metadata["registered_epoch"])                     //nolint:errcheck
	fmt.Fprintf(stdout, "Attempt:    %s\n", b.Metadata["delivery_attempt"])                     //nolint:errcheck
	fmt.Fprintf(stdout, "Nudge:      %s\n", b.Metadata["nudge_id"])                             //nolint:errcheck
	fmt.Fprintf(stdout, "Note:       %s\n", b.Description)                                      //nolint:errcheck
}

type waitJSON struct {
	ID              string   `json:"id"`
	SessionID       string   `json:"session_id"`
	SessionName     string   `json:"session_name,omitempty"`
	State           string   `json:"state"`
	Kind            string   `json:"kind"`
	DepIDs          []string `json:"dep_ids,omitempty"`
	DepMode         string   `json:"dep_mode,omitempty"`
	RegisteredEpoch string   `json:"registered_epoch,omitempty"`
	DeliveryAttempt string   `json:"delivery_attempt,omitempty"`
	NudgeID         string   `json:"nudge_id,omitempty"`
	Note            string   `json:"note,omitempty"`
	Status          string   `json:"status"`
	CreatedAt       string   `json:"created_at,omitempty"`
}

type waitListJSONEnvelope struct {
	SchemaVersion string     `json:"schema_version"`
	CityPath      string     `json:"city_path"`
	Waits         []waitJSON `json:"waits"`
}

type waitInspectJSONEnvelope struct {
	SchemaVersion string   `json:"schema_version"`
	CityPath      string   `json:"city_path"`
	Wait          waitJSON `json:"wait"`
}

func waitJSONFromBead(b beads.Bead) waitJSON {
	return waitJSON{
		ID:              b.ID,
		SessionID:       b.Metadata["session_id"],
		SessionName:     b.Metadata["session_name"],
		State:           b.Metadata["state"],
		Kind:            b.Metadata["kind"],
		DepIDs:          splitWaitIDs(b.Metadata["dep_ids"]),
		DepMode:         b.Metadata["dep_mode"],
		RegisteredEpoch: b.Metadata["registered_epoch"],
		DeliveryAttempt: b.Metadata["delivery_attempt"],
		NudgeID:         b.Metadata["nudge_id"],
		Note:            b.Description,
		Status:          b.Status,
		CreatedAt:       formatOptionalTime(b.CreatedAt),
	}
}

func splitWaitIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func writeWaitListJSON(stdout, stderr io.Writer, cityPath string, waits []beads.Bead) int {
	rows := make([]waitJSON, 0, len(waits))
	for _, wait := range waits {
		rows = append(rows, waitJSONFromBead(wait))
	}
	payload := waitListJSONEnvelope{
		SchemaVersion: "1",
		CityPath:      cityPath,
		Waits:         rows,
	}
	if err := writeCLIJSONLine(stdout, payload); err != nil {
		fmt.Fprintf(stderr, "gc wait list: encode JSON: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

func writeWaitInspectJSON(stdout, stderr io.Writer, cityPath string, wait beads.Bead) int {
	payload := waitInspectJSONEnvelope{
		SchemaVersion: "1",
		CityPath:      cityPath,
		Wait:          waitJSONFromBead(wait),
	}
	if err := writeCLIJSONLine(stdout, payload); err != nil {
		fmt.Fprintf(stderr, "gc wait inspect: encode JSON: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

func cmdWaitSetState(waitID, state string, stdout, stderr io.Writer) int {
	_, code := cmdWaitSetStateResult(waitID, state, stdout, stderr)
	return code
}

func cmdWaitSetStateResult(waitID, state string, stdout, stderr io.Writer) (waitSetStateResult, int) {
	result := waitSetStateResult{WaitID: waitID}
	store, code := openCityStore(stderr, "gc wait")
	if store == nil {
		return result, code
	}
	b, err := store.Get(waitID)
	if err != nil {
		fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
		return result, 1
	}
	if !sessionpkg.IsWaitBead(b) {
		fmt.Fprintf(stderr, "gc wait: %s is not a wait\n", waitID) //nolint:errcheck
		return result, 1
	}
	if state == waitStateReady {
		if err := waitLifecycleEnabled(); err != nil {
			fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
			return result, 1
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if state == waitStateReady && b.Status == "closed" {
		retried, err := retryClosedWait(store, b, now)
		if err != nil {
			fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
			return result, 1
		}
		fmt.Fprintf(stdout, "Retried wait %s as %s.\n", waitID, retried.ID) //nolint:errcheck
		result.WaitID = retried.ID
		result.ReadyWaitID = retried.ID
		result.Retried = true
		result.RetriedFrom = waitID
		return result, 0
	}
	batch := map[string]string{"state": state}
	switch state {
	case waitStateReady:
		batch["ready_at"] = now
		nextAttempt, err := nextWaitDeliveryAttempt(nudgeFrontDoor(beads.NudgesStore{Store: store}), b)
		if err != nil {
			fmt.Fprintf(stderr, "gc wait: %v\n", err) //nolint:errcheck
			return result, 1
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
		return result, 1
	}
	if state == waitStateCanceled {
		if cityPath, err := resolveCity(); err == nil {
			if err := withdrawQueuedWaitNudges(cityPath, []string{b.Metadata["nudge_id"]}); err != nil {
				fmt.Fprintf(stderr, "gc wait: withdrawing queued nudge: %v\n", err) //nolint:errcheck
				return result, 1
			}
		}
		if err := clearSessionWaitHoldIfIdle(store, b.Metadata["session_id"]); err != nil {
			fmt.Fprintf(stderr, "gc wait: clearing session wait hold: %v\n", err) //nolint:errcheck
			return result, 1
		}
	}
	fmt.Fprintf(stdout, "Updated wait %s to %s.\n", waitID, state) //nolint:errcheck
	return result, 0
}

func loadWaitBeads(store beads.Store) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	return loadWaitBeadsByLabel(store)
}

func loadSessionWaitBeads(store beads.Store, sessionID string) ([]beads.Bead, error) {
	return sessionpkg.ListSessionWaitBeads(store, sessionID)
}

const waitLookupLimit = sessionpkg.SessionWaitLookupLimit

func isWaitLookupLimitError(err error) bool {
	return beads.IsLookupLimitError(err)
}

func stampWaitLookupCapDiagnostic(sessFront *sessionpkg.InfoStore, sessionID string, err error, now time.Time, source string) {
	if sessFront == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	var limitErr beads.LookupLimitError
	if !errors.As(err, &limitErr) {
		return
	}
	label := limitErr.Label
	if label == "" {
		label = "session:" + sessionID
	}
	if source == "" {
		source = "wait-lookup"
	}
	batch := map[string]string{}
	sessionpkg.StampWaitLookupCapMetadata(batch, label, limitErr.Limit, now, source)
	if err := sessFront.ApplyPatch(sessionID, batch); err != nil {
		log.Printf("gc wait: recording lookup cap diagnostic for session %s failed: %v", sessionID, err)
	}
}

func stampGlobalWaitLookupCapDiagnostics(sessFront *sessionpkg.InfoStore, sessionBeads *sessionBeadSnapshot, err error, now time.Time) {
	for _, sessionBead := range sessionBeads.Open() {
		stampWaitLookupCapDiagnostic(sessFront, sessionBead.ID, err, now, "wake-state-global")
	}
}

func loadWaitBeadsByLabel(store beads.Store) ([]beads.Bead, error) {
	all, err := store.List(beads.ListQuery{
		Label: waitBeadLabel,
		Limit: waitLookupLimit + 1,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		return nil, err
	}
	capped := len(all) > waitLookupLimit
	if capped {
		all = all[:waitLookupLimit]
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
	if capped {
		return result, beads.LookupLimitError{Kind: "wait", Label: waitBeadLabel, Limit: waitLookupLimit}
	}
	return result, nil
}

func loadWaitBeadsForWakeState(store beads.Store, sessionBeads *sessionBeadSnapshot) ([]beads.Bead, error) {
	// Open sessions get per-session coverage; waits tied only to closed
	// sessions can fall outside the newest global capped window under
	// saturation, with cap diagnostics as the operator signal.
	waits, seen, err := loadWaitBeadsForOpenSessionsWithSeen(store, sessionBeads)
	if err != nil {
		return nil, err
	}
	globalWaits, err := loadWaitBeads(store)
	if err != nil {
		if !isWaitLookupLimitError(err) {
			return nil, err
		}
		stampGlobalWaitLookupCapDiagnostics(sessionFrontDoor(store), sessionBeads, err, time.Now().UTC())
		log.Printf("gc wait: global wake-state wait lookup failed; continuing with open-session waits: %v", err)
	}
	for _, wait := range globalWaits {
		if seen[wait.ID] {
			continue
		}
		seen[wait.ID] = true
		waits = append(waits, wait)
	}
	return waits, nil
}

func loadWaitBeadsForOpenSessions(store beads.Store, sessionBeads *sessionBeadSnapshot) ([]beads.Bead, error) {
	waits, _, err := loadWaitBeadsForOpenSessionsWithSeen(store, sessionBeads)
	return waits, err
}

func loadWaitBeadsForOpenSessionsWithSeen(store beads.Store, sessionBeads *sessionBeadSnapshot) ([]beads.Bead, map[string]bool, error) {
	seen := map[string]bool{}
	if store == nil || sessionBeads == nil {
		return nil, seen, nil
	}
	waits := []beads.Bead(nil)
	for _, sessionBead := range sessionBeads.Open() {
		sessionWaits, err := loadSessionWaitBeads(store, sessionBead.ID)
		if err != nil {
			if !isWaitLookupLimitError(err) {
				return nil, seen, err
			}
			stampWaitLookupCapDiagnostic(sessionFrontDoor(store), sessionBead.ID, err, time.Now().UTC(), "wake-state-session")
			log.Printf("gc wait: session %s wait lookup capped; continuing with filtered partial waits: %v", sessionBead.ID, err)
		}
		for _, wait := range sessionWaits {
			if seen[wait.ID] {
				continue
			}
			seen[wait.ID] = true
			waits = append(waits, wait)
		}
	}
	return waits, seen, nil
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
	return prepareWaitWakeStateForCityWithSnapshot(cityPath, beads.SessionStore{Store: store}, now, nil)
}

func prepareWaitWakeStateForCityWithSnapshot(cityPath string, store beads.SessionStore, now time.Time, sessionBeads *sessionBeadSnapshot) (map[string]bool, error) {
	if sessionBeads == nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(store.Store)
		if err != nil {
			return nil, err
		}
	}
	waits, err := loadWaitBeadsForWakeState(store.Store, sessionBeads)
	if err != nil {
		return nil, err
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
				sessionBead, found, err = lookupSessionBeadByID(store.Store, sessionID)
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
			if err := setWaitTerminalState(store.Store, wait.ID, map[string]string{
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
		if sessionBead.Status == "closed" {
			if err := setWaitTerminalState(store, wait.ID, map[string]string{
				"state":       waitStateCanceled,
				"canceled_at": now.UTC().Format(time.RFC3339),
				"last_error":  "session-closed",
			}); err != nil {
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
	return dispatchReadyWaitNudgesWithSnapshot(cityPath, nil, store, now, nil)
}

func dispatchReadyWaitNudgesWithSnapshot(cityPath string, cfg *config.City, store beads.Store, now time.Time, sessionBeads *sessionBeadSnapshot) error {
	if sessionBeads == nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(store)
		if err != nil {
			return err
		}
	}
	waits, err := loadWaitBeadsForOpenSessions(store, sessionBeads)
	if err != nil {
		return err
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
		_, ok, err := nudgeFrontDoor(beads.NudgesStore{Store: store}).Find(nudgeID)
		if err != nil {
			if beads.IsLookupLimitError(err) {
				stampWaitLookupCapDiagnostic(sessionFrontDoor(store), sessionID, err, now, "ready-wait-nudge")
				continue
			}
			return err
		}
		if ok {
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
		if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: store}, item); err != nil {
			return err
		}
		if err := store.SetMetadata(wait.ID, "nudge_id", nudgeID); err != nil {
			return fmt.Errorf("setting wait nudge_id: %w", err)
		}
		// provider_kind is stamped from ResolvedProvider.Kind /
		// BuiltinAncestor at session-bead creation, so wrapped aliases
		// already surface as their built-in family here. The provider
		// fallback covers sessions created before provider_kind was stamped.
		if waitNudgeProviderNeedsPoller(sessionBead) && !nudgeDispatcherIsSupervisor(cfg) {
			if err := startNudgePoller(cityPath, waitNudgePollerKey(sessionBead), sessionBead.Metadata["session_name"]); err != nil {
				return fmt.Errorf("starting wait nudge poller: %w", err)
			}
		}
	}
	return nil
}

func waitNudgeProviderNeedsPoller(sessionBead beads.Bead) bool {
	switch sessionProviderFamily(sessionBead) {
	case "codex", "pi":
		return true
	default:
		return false
	}
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
	nudge, ok, err := nudgeFrontDoor(beads.NudgesStore{Store: store}).FindIncludingTerminal(nudgeID)
	if err != nil {
		if beads.IsLookupLimitError(err) {
			stampWaitLookupCapDiagnostic(sessionFrontDoor(store), wait.Metadata["session_id"], err, now, "ready-wait-finalize-nudge")
			return false, nil
		}
		return false, err
	}
	if !ok {
		return false, err
	}
	switch nudge.State {
	case "injected", "accepted_for_injection":
		return true, setWaitTerminalState(store, wait.ID, map[string]string{
			"state":           waitStateClosed,
			"closed_at":       now.UTC().Format(time.RFC3339),
			"nudge_id":        nudgeID,
			"commit_boundary": nudge.CommitBoundary,
		})
	case "expired", "failed":
		return true, setWaitTerminalState(store, wait.ID, map[string]string{
			"state":           waitStateFailed,
			"failed_at":       now.UTC().Format(time.RFC3339),
			"nudge_id":        nudgeID,
			"last_error":      nudge.TerminalReason,
			"commit_boundary": nudge.CommitBoundary,
		})
	default:
		return false, nil
	}
}

func cancelWaitsForSession(store beads.Store, sessionID string) error {
	if store == nil || sessionID == "" {
		return nil
	}
	nudgeIDs, _, err := sessionpkg.CancelWaitsAndCollectNudgeIDs(store, sessionID, time.Now().UTC())
	if err != nil {
		if !isWaitLookupLimitError(err) {
			return err
		}
	}
	if cityPath, err := resolveCity(); err == nil {
		if err := withdrawQueuedWaitNudges(cityPath, nudgeIDs); err != nil {
			return err
		}
	}
	return err
}

func clearSessionWaitHold(sessFront *sessionpkg.InfoStore, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	batch := map[string]string{
		"wait_hold":    "",
		"sleep_intent": "",
	}
	if sessFront != nil {
		if markers, err := sessFront.PersistedMarkers(sessionID); err == nil && markers.SleepReason == "wait-hold" {
			batch["sleep_reason"] = ""
		}
	}
	return sessFront.ApplyPatch(sessionID, batch)
}

func clearSessionWaitHoldIfIdle(store beads.Store, sessionID string) error {
	hasWaits, err := hasNonTerminalWaits(store, sessionID)
	if err != nil {
		return err
	}
	if hasWaits {
		return nil
	}
	return clearSessionWaitHold(sessionFrontDoor(store), sessionID)
}

func hasNonTerminalWaits(store beads.Store, sessionID string) (bool, error) {
	waits, err := loadSessionWaitBeads(store, sessionID)
	if err != nil && !isWaitLookupLimitError(err) {
		return false, err
	}
	capped := err != nil
	for _, wait := range waits {
		if !isWaitTerminal(wait.Metadata["state"]) {
			return true, nil
		}
	}
	if capped {
		log.Printf("gc wait: session %s wait-hold lookup capped; preserving wait hold: %v", sessionID, err)
		return true, nil
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

func waitNudgePollerKey(sessionBead beads.Bead) string {
	return sessionpkg.PollerKeyFromBead(sessionBead)
}

// sessionProviderFamily returns the built-in provider family for a session bead.
func sessionProviderFamily(sessionBead beads.Bead) string {
	return sessionpkg.ProviderFamilyFromMetadata(sessionBead.Metadata, "")
}

func setWaitTerminalState(store beads.Store, waitID string, batch map[string]string) error {
	if err := store.SetMetadataBatch(waitID, batch); err != nil {
		return err
	}
	return store.Close(waitID)
}

func retryClosedWait(store beads.Store, wait beads.Bead, now string) (beads.Bead, error) {
	nextAttempt, err := nextWaitDeliveryAttempt(nudgeFrontDoor(beads.NudgesStore{Store: store}), wait)
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
		if markers, err := sessionFrontDoor(store).PersistedMarkers(sessionID); err == nil {
			if epoch := markers.ContinuationEpoch; epoch != "" {
				meta["registered_epoch"] = epoch
			}
			if meta["session_name"] == "" {
				meta["session_name"] = markers.SessionName
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

func nextWaitDeliveryAttempt(front *nudgequeue.Store, wait beads.Bead) (string, error) {
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
	if nudgeID == "" || front == nil {
		return strconv.Itoa(attempt + 1), nil
	}
	nudge, ok, err := front.FindIncludingTerminal(nudgeID)
	if err != nil {
		return "", err
	}
	if !ok || nudgequeue.IsTerminalState(nudge.State) {
		return strconv.Itoa(attempt + 1), nil
	}
	return "", nil
}

func withdrawQueuedWaitNudges(cityPath string, nudgeIDs []string) error {
	return nudgequeue.WithdrawWaitNudges(openNudgeBeadStore(cityPath).Store, cityPath, nudgeIDs)
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
