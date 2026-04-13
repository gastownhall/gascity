package dashboard

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Activity color constants (inlined from upstream internal/activity).
const (
	colorGreen   = "green"
	colorYellow  = "yellow"
	colorRed     = "red"
	colorUnknown = "unknown"
)

// Activity thresholds for color coding.
const (
	thresholdActive = 5 * time.Minute  // Green threshold
	thresholdStale  = 10 * time.Minute // Yellow threshold (beyond this is red)
)

// Default GUPP violation timeout (30 min, same as upstream).
const defaultGUPPViolationTimeout = 30 * time.Minute

// calculateActivity computes activity info from a last-activity timestamp.
func calculateActivity(lastActivity time.Time) ActivityInfo {
	if lastActivity.IsZero() {
		return ActivityInfo{
			Display:    "unknown",
			ColorClass: colorUnknown,
		}
	}

	d := time.Since(lastActivity)
	if d < 0 {
		d = 0
	}

	return ActivityInfo{
		Display:    formatAge(d),
		ColorClass: colorForDuration(d),
	}
}

// formatAge formats a duration as a short human-readable string.
func formatAge(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// colorForDuration returns the color class for a given duration.
func colorForDuration(d time.Duration) string {
	switch {
	case d < thresholdActive:
		return colorGreen
	case d < thresholdStale:
		return colorYellow
	default:
		return colorRed
	}
}

// extractIssueID unwraps "external:prefix:id" to just "id".
func extractIssueID(id string) string {
	if strings.HasPrefix(id, "external:") {
		parts := strings.SplitN(id, ":", 3)
		if len(parts) == 3 {
			return parts[2]
		}
	}
	return id
}

// formatTimestamp formats a time as "Jan 26, 3:45 PM" (or "Jan 26 2006, 3:45 PM" if different year).
func formatTimestamp(t time.Time) string {
	now := time.Now()
	if t.Year() != now.Year() {
		return t.Format("Jan 2 2006, 3:04 PM")
	}
	return t.Format("Jan 2, 3:04 PM")
}

// formatAgentAddress shortens agent addresses for display.
// "rig/polecats/Toast" -> "Toast (rig)"
// "mayor/" -> "Mayor"
func formatAgentAddress(addr string) string {
	if addr == "" {
		return "\u2014" // em-dash
	}
	if addr == "mayor/" || addr == "mayor" {
		return "Mayor"
	}

	parts := strings.Split(addr, "/")
	if len(parts) >= 3 && parts[1] == "polecats" {
		return fmt.Sprintf("%s (%s)", parts[2], parts[0])
	}
	if len(parts) >= 3 && parts[1] == "crew" {
		return fmt.Sprintf("%s (%s/crew)", parts[2], parts[0])
	}
	if len(parts) >= 2 {
		return fmt.Sprintf("%s/%s", parts[0], parts[len(parts)-1])
	}
	return addr
}

// calculateWorkStatus determines the work status based on progress and activity.
// Returns: "complete", "active", "stale", "stuck", or "waiting"
func calculateWorkStatus(completed, total int, activityColor string) string {
	if total > 0 && completed == total {
		return "complete"
	}

	switch activityColor {
	case colorGreen:
		return "active"
	case colorYellow:
		return "stale"
	case colorRed:
		return "stuck"
	default:
		return "waiting"
	}
}

// calculateWorkerWorkStatus determines the worker's work status based on activity and assignment.
func calculateWorkerWorkStatus(activityAge time.Duration, issueID, workerName string, staleThreshold, stuckThreshold time.Duration) string {
	if workerName == "refinery" {
		return "working"
	}

	if issueID == "" {
		return "idle"
	}

	switch {
	case activityAge < staleThreshold:
		return "working"
	case activityAge < stuckThreshold:
		return "stale"
	default:
		return "stuck"
	}
}

// eventCategory classifies an event type for filtering/display.
func eventCategory(eventType string) string {
	switch eventType {
	case "session.woke", "session.stopped", "session.crashed",
		"session.draining", "session.undrained", "session.quarantined",
		"session.idle_killed", "session.stuck_killed", "session.suspended", "session.updated":
		return "session"
	case "bead.created", "bead.closed", "bead.updated":
		return "work"
	case "mail.sent", "mail.read", "mail.archived",
		"mail.marked_read", "mail.marked_unread",
		"mail.replied", "mail.deleted":
		return "comms"
	case "controller.started", "controller.stopped",
		"city.suspended", "city.resumed",
		"convoy.created", "convoy.closed",
		"order.fired", "order.completed", "order.failed",
		"provider.swapped":
		return "system"
	default:
		return "system"
	}
}

// extractRig extracts the rig name from an actor address like "myrig/polecats/nux".
// Returns "" for city-scoped agents (no "/" in name).
func extractRig(actor string) string {
	if actor == "" || !strings.Contains(actor, "/") {
		return ""
	}
	return strings.SplitN(actor, "/", 2)[0]
}

// eventIcon returns an emoji for an event type.
func eventIcon(eventType string) string {
	icons := map[string]string{
		"session.woke":         "\u25b6\ufe0f", // play
		"session.stopped":      "\u23f9\ufe0f", // stop
		"session.crashed":      "\u2620\ufe0f", // skull and crossbones
		"session.draining":     "\u23f3",       // hourglass
		"session.undrained":    "\u25b6\ufe0f", // play (resumed)
		"session.quarantined":  "\U0001f6ab",   // no entry
		"session.idle_killed":  "\U0001f480",   // skull
		"session.stuck_killed": "\U0001f9ca",   // ice (frozen)
		"session.suspended":    "\u23f8\ufe0f", // pause
		"session.updated":      "\U0001f504",   // counterclockwise arrows
		"bead.created":         "\U0001fa9d",   // hook
		"bead.closed":          "\u2705",       // check mark
		"bead.updated":         "\U0001f4dd",   // memo
		"mail.sent":            "\U0001f4ec",   // mailbox
		"mail.read":            "\U0001f4e8",   // incoming envelope
		"mail.archived":        "\U0001f4e6",   // package
		"controller.started":   "\U0001f680",   // rocket
		"controller.stopped":   "\U0001f6d1",   // stop sign
		"city.suspended":       "\u23f8\ufe0f", // pause
		"city.resumed":         "\u25b6\ufe0f", // play
		"convoy.created":       "\U0001f69a",   // delivery truck
		"convoy.closed":        "\u2705",       // check mark
		"order.fired":          "\u26a1",       // lightning
		"order.completed":      "\u2714\ufe0f", // check
		"order.failed":         "\u274c",       // cross mark
		"provider.swapped":     "\U0001f500",   // shuffle
	}
	if icon, ok := icons[eventType]; ok {
		return icon
	}
	return "\U0001f4cb" // clipboard
}

// eventSummary generates a human-readable summary for an event.
// Real events use Actor/Subject/Message fields from events.Event.
func eventSummary(eventType, actor, subject, message string) string {
	shortActor := formatAgentAddress(actor)

	switch eventType {
	case "session.woke":
		return fmt.Sprintf("%s woke", formatAgentAddress(subject))
	case "session.stopped":
		return fmt.Sprintf("%s stopped", formatAgentAddress(subject))
	case "session.crashed":
		return fmt.Sprintf("%s crashed", formatAgentAddress(subject))
	case "session.draining":
		return fmt.Sprintf("%s draining", formatAgentAddress(subject))
	case "session.undrained":
		return fmt.Sprintf("%s undrained", formatAgentAddress(subject))
	case "session.quarantined":
		return fmt.Sprintf("%s quarantined", formatAgentAddress(subject))
	case "session.idle_killed":
		return fmt.Sprintf("%s idle-killed", formatAgentAddress(subject))
	case "session.stuck_killed":
		if message != "" {
			return fmt.Sprintf("%s stuck-killed: %s", formatAgentAddress(subject), message)
		}
		return fmt.Sprintf("%s stuck-killed", formatAgentAddress(subject))
	case "session.suspended":
		return fmt.Sprintf("%s suspended", formatAgentAddress(subject))
	case "session.updated":
		return fmt.Sprintf("%s updated", formatAgentAddress(subject))
	case "bead.created":
		return fmt.Sprintf("%s created bead %s", shortActor, subject)
	case "bead.closed":
		return fmt.Sprintf("%s closed bead %s", shortActor, subject)
	case "bead.updated":
		return fmt.Sprintf("%s updated bead %s", shortActor, subject)
	case "mail.sent":
		return fmt.Sprintf("%s sent mail to %s", shortActor, formatAgentAddress(subject))
	case "controller.started":
		return "controller started"
	case "controller.stopped":
		return "controller stopped"
	case "order.fired":
		if message != "" {
			return fmt.Sprintf("order fired: %s", message)
		}
		return "order fired"
	case "order.completed":
		return "order completed"
	case "order.failed":
		if message != "" {
			return fmt.Sprintf("order failed: %s", message)
		}
		return "order failed"
	default:
		if message != "" {
			return message
		}
		return eventType
	}
}

// runCmd executes a command with a timeout and returns stdout.
func runCmd(timeout time.Duration, name string, args ...string) (*bytes.Buffer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s timed out after %v", name, timeout)
		}
		return nil, err
	}
	return &stdout, nil
}

// determineCIStatus evaluates the overall CI status from status checks.
func determineCIStatus(checks []struct {
	State      string `json:"state"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
},
) string {
	if len(checks) == 0 {
		return "pending"
	}

	hasFailure := false
	hasPending := false

	for _, check := range checks {
		switch check.Conclusion {
		case "failure", "canceled", "timed_out", "action_required":
			hasFailure = true
		case "success", "skipped", "neutral":
			// Pass
		default:
			switch check.Status {
			case "queued", "in_progress", "waiting", "pending", "requested":
				hasPending = true
			}
			switch check.State {
			case "FAILURE", "ERROR":
				hasFailure = true
			case "PENDING", "EXPECTED":
				hasPending = true
			}
		}
	}

	if hasFailure {
		return "fail"
	}
	if hasPending {
		return "pending"
	}
	return "pass"
}

// determineMergeableStatus converts GitHub's mergeable field to display value.
func determineMergeableStatus(mergeable string) string {
	switch strings.ToUpper(mergeable) {
	case "MERGEABLE":
		return "ready"
	case "CONFLICTING":
		return "conflict"
	default:
		return "pending"
	}
}

// determineColorClass determines the row color based on CI and merge status.
func determineColorClass(ciStatus, mergeable string) string {
	if ciStatus == "fail" || mergeable == "conflict" {
		return "mq-red"
	}
	if ciStatus == "pending" || mergeable == "pending" {
		return "mq-yellow"
	}
	if ciStatus == "pass" && mergeable == "ready" {
		return "mq-green"
	}
	return "mq-yellow"
}

// prResponse represents the JSON response from gh pr list.
type prResponse struct {
	Number            int    `json:"number"`
	Title             string `json:"title"`
	URL               string `json:"url"`
	Mergeable         string `json:"mergeable"`
	StatusCheckRollup []struct {
		State      string `json:"state"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"statusCheckRollup"`
}

// scopedPath rewrites a bare API path for supervisor city-scoped routing.
// When cityScope is empty (standalone mode), returns path unchanged.
// When set, "/v0/sessions" becomes "/v0/city/{cityScope}/sessions".
func scopedPath(path, cityScope string) string {
	if cityScope == "" || !strings.HasPrefix(path, "/v0/") {
		return path
	}
	return "/v0/city/" + cityScope + "/" + strings.TrimPrefix(path, "/v0/")
}

// gitURLToRepoPath converts a git URL to owner/repo format.
func gitURLToRepoPath(gitURL string) string {
	if strings.HasPrefix(gitURL, "https://github.com/") {
		path := strings.TrimPrefix(gitURL, "https://github.com/")
		path = strings.TrimSuffix(path, ".git")
		return path
	}
	if strings.HasPrefix(gitURL, "git@github.com:") {
		path := strings.TrimPrefix(gitURL, "git@github.com:")
		path = strings.TrimSuffix(path, ".git")
		return path
	}
	return ""
}
