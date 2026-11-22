package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// nudgeOrphanStatusJSON is `gc nudge status --orphaned --json`: the city's
// queued items whose key no open session matches.
type nudgeOrphanStatusJSON struct {
	SchemaVersion string                `json:"schema_version"`
	Command       string                `json:"command"`
	CityPath      string                `json:"city_path"`
	Orphaned      []nudgeOrphanItemJSON `json:"orphaned"`
}

// nudgeOrphanItemJSON is one orphaned queue item and how its session fence
// resolves against the predecessor's pool seat.
type nudgeOrphanItemJSON struct {
	ID           string            `json:"id"`
	BeadID       string            `json:"bead_id,omitempty"`
	Agent        string            `json:"agent"`
	Bucket       nudgequeue.Bucket `json:"bucket"`
	Source       string            `json:"source"`
	DeliverAfter time.Time         `json:"deliver_after"`
	ExpiresAt    time.Time         `json:"expires_at"`
	// DeadReason is set for an item in the dead bucket.
	DeadReason string `json:"dead_reason,omitempty"`
	// FenceSessionID is the session the item is fenced to; empty when
	// unfenced.
	FenceSessionID string `json:"fence_session_id,omitempty"`
	// FenceSessionState is the fenced session's state ("closed" for a closed
	// row); empty when it was not read.
	FenceSessionState string                `json:"fence_session_state,omitempty"`
	Resolution        nudgeOrphanResolution `json:"resolution"`
	// Successor is the session that will adopt the item, set when
	// Resolution is successor.
	Successor string `json:"successor,omitempty"`
	Message   string `json:"message"`
}

// nudgeOrphanMessageWidth bounds the message shown in the text view.
const nudgeOrphanMessageWidth = 60

// cmdNudgeStatusOrphaned lists, city-wide, the queued items whose key no open
// session matches, reading the open sessions, the queue and each fenced
// session live. A read failure names itself on stderr and exits 1; with
// --json it writes no partial result.
func cmdNudgeStatusOrphaned(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "gc nudge status: --orphaned lists the whole city and takes no session argument") //nolint:errcheck
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge status: %v\n", err) //nolint:errcheck
		return 1
	}
	items, err := listNudgeOrphans(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge status: listing orphaned nudges: %v\n", err) //nolint:errcheck
		if jsonOutput || items == nil {
			return 1
		}
	}
	if jsonOutput {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc nudge status", nudgeOrphanStatusJSON{
			SchemaVersion: "1",
			Command:       "nudge status",
			CityPath:      cityPath,
			Orphaned:      items,
		})
	}
	writeNudgeOrphansText(stdout, items)
	if err != nil {
		return 1
	}
	return 0
}

// listNudgeOrphans returns the city's pending, in-flight and dead items whose
// key no open session matches, in that bucket order, each with its seat
// resolution. A nil result with an error means nothing could be listed; a
// non-nil one lists every item, the failed fenced-session reads resolving
// lookup-error.
func listNudgeOrphans(cityPath string, stderr io.Writer) ([]nudgeOrphanItemJSON, error) {
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return nil, err
	}
	store, opened, err := openNudgeBeadStoreOwned(cityPath)
	if err != nil {
		return nil, err
	}
	defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
	sessions := cliSessionFrontDoor(opened, cfg, cityPath)
	open, err := loadOpenSessionInfos(cliSessionStore(opened, cfg, cityPath))
	if err != nil {
		return nil, fmt.Errorf("reading open sessions: %w", err)
	}
	state, err := openNudgeQueue(cityPath, store).Queue.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("reading the nudge queue: %w", err)
	}
	targets := make([]nudgeTarget, len(open))
	for i, info := range open {
		targets[i] = resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
	}
	buckets := []struct {
		bucket nudgequeue.Bucket
		items  []queuedNudge
	}{
		{nudgequeue.BucketPending, state.Pending},
		{nudgequeue.BucketInFlight, state.InFlight},
		{nudgequeue.BucketDead, state.Dead},
	}
	var keys []string
	seen := make(map[string]bool)
	for _, b := range buckets {
		for _, item := range b.items {
			if !seen[item.Agent] {
				seen[item.Agent] = true
				keys = append(keys, item.Agent)
			}
		}
	}
	orphaned := make(map[string]bool)
	for _, key := range unmatchedNudgeQueueKeys(keys, targets) {
		orphaned[key] = true
	}
	resolver := newNudgeOrphanSeatResolver(open, sessions.Get, -1)
	items := []nudgeOrphanItemJSON{}
	var errs []error
	for _, b := range buckets {
		for _, item := range b.items {
			if !orphaned[item.Agent] {
				continue
			}
			row, rowErr := nudgeOrphanItem(resolver, b.bucket, item)
			if rowErr != nil {
				errs = append(errs, rowErr)
			}
			items = append(items, row)
		}
	}
	return items, errors.Join(errs...)
}

// nudgeOrphanItem projects item and its seat resolution, returning the
// fenced-session read error that left it lookup-error.
func nudgeOrphanItem(resolver *nudgeOrphanSeatResolver, bucket nudgequeue.Bucket, item queuedNudge) (nudgeOrphanItemJSON, error) {
	seat := resolver.resolve(item)
	row := nudgeOrphanItemJSON{
		ID:             item.ID,
		BeadID:         item.BeadID,
		Agent:          item.Agent,
		Bucket:         bucket,
		Source:         item.Source,
		DeliverAfter:   item.DeliverAfter,
		ExpiresAt:      item.ExpiresAt,
		FenceSessionID: strings.TrimSpace(item.SessionID),
		Resolution:     seat.Resolution,
		Message:        item.Message,
	}
	if bucket == nudgequeue.BucketDead {
		row.DeadReason = deadReason(item)
	}
	if seat.Resolution == nudgeOrphanSuccessor {
		row.Successor = seat.Successor.SessionName
	}
	if row.FenceSessionID == "" {
		return row, nil
	}
	p := resolver.predecessor(row.FenceSessionID)
	switch {
	case p.err != nil:
		return row, p.err
	case p.info.ID == "":
	case p.info.Closed:
		row.FenceSessionState = "closed"
	default:
		row.FenceSessionState = string(p.info.State)
	}
	return row, nil
}

// writeNudgeOrphansText writes the orphan view as one table row per item.
func writeNudgeOrphansText(stdout io.Writer, items []nudgeOrphanItemJSON) {
	if len(items) == 0 {
		fmt.Fprintln(stdout, "No orphaned nudges.") //nolint:errcheck
		return
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tBEAD\tKEY\tBUCKET\tSOURCE\tDUE\tEXPIRES\tFENCE\tFENCE_STATE\tRESOLUTION\tMESSAGE") //nolint:errcheck
	for _, item := range items {
		resolution := string(item.Resolution)
		if item.Successor != "" {
			resolution += " " + item.Successor
		}
		if item.DeadReason != "" {
			resolution += " (dead: " + item.DeadReason + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			item.ID, orDash(item.BeadID), item.Agent, item.Bucket, orDash(item.Source),
			formatDueTime(item.DeliverAfter), item.ExpiresAt.Format(time.RFC3339),
			orDash(item.FenceSessionID), orDash(item.FenceSessionState), resolution,
			nudgeOrphanMessage(item.Message))
	}
	_ = tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// nudgeOrphanMessage returns the first line of message, cut to
// nudgeOrphanMessageWidth bytes.
func nudgeOrphanMessage(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return truncate(line, nudgeOrphanMessageWidth)
}
