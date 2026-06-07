// Package deliverywarden implements the delivery-warden sweep — an idempotent
// reconciler that repairs orphan/zombie delivery beads and escalates stalled or
// long-lived PRs. It is registered as an exec-type order and runs on a 2-minute
// cooldown interval.
package deliverywarden

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/delivery"
)

// Warden metadata key constants.
const (
	metaKeyPhaseEnteredAt    = "gc.phase_entered_at"    // Unix timestamp when bead entered current phase
	metaKeyWardenRetries     = "gc.warden_retries"      // number of recovery attempts by the warden
	metaKeyWardenEscalated   = "gc.warden_escalated"    // set when escalation mail has been sent
	metaKeyCreatedAtOverride = "gc.created_at_override" // test hook: override effective creation time (Unix seconds)
)

const maxLifetime = 24 * time.Hour

// phaseDwellBudgets maps each delivery phase to its maximum allowed dwell time.
var phaseDwellBudgets = map[string]time.Duration{
	delivery.PhaseBuilding:        10 * time.Minute,
	delivery.PhaseCIPending:       30 * time.Minute,
	delivery.PhaseReviewPending:   60 * time.Minute,
	delivery.PhaseRework:          30 * time.Minute,
	delivery.PhaseDecisionPending: 20 * time.Minute,
	delivery.PhaseMergePending:    15 * time.Minute,
}

// recoveryTarget maps each delivery phase to the agent pool that should be nudged on stall.
var recoveryTarget = map[string]string{
	delivery.PhaseBuilding:        "voxist-platform/voxist.executor",
	delivery.PhaseCIPending:       "voxist-platform/voxist.executor",
	delivery.PhaseReviewPending:   "voxist-platform/voxist.reviewer",
	delivery.PhaseRework:          "voxist-platform/voxist.executor",
	delivery.PhaseDecisionPending: "voxist-platform/voxist.reviewer",
	delivery.PhaseMergePending:    "voxist-platform/voxist.reviewer",
}

// PullRequest is the subset of GitHub PR data the warden needs.
type PullRequest struct {
	Owner    string
	Repo     string
	Number   int
	URL      string
	HeadRef  string     // e.g. "gc/vp-krai"
	State    string     // "OPEN", "MERGED", "CLOSED"
	MergedAt *time.Time // non-nil when State == "MERGED"
}

// GitHubClient is the GitHub surface used by the warden.
type GitHubClient interface {
	// ListOpenPRs returns all open pull requests for the given owner/repo.
	ListOpenPRs(owner, repo string) ([]PullRequest, error)
	// GetPR returns current state for a PR identified by its HTML URL.
	GetPR(prURL string) (PullRequest, error)
}

// MailSender is the minimal mail interface used for escalation.
type MailSender interface {
	Send(from, to, subject, body string) error
}

// Warden reconciles GitHub PR state against the bead store on each sweep.
type Warden struct {
	store  beads.Store
	github GitHubClient
	mail   MailSender
	clock  func() time.Time // injectable for tests; defaults to time.Now
}

// NewWarden creates a Warden backed by the given store, GitHub client, and mail sender.
func NewWarden(store beads.Store, gh GitHubClient, mail MailSender) *Warden {
	return &Warden{store: store, github: gh, mail: mail, clock: time.Now}
}

// now returns the current time via the injectable clock.
func (w *Warden) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

// RepairOrphan detects open GitHub PRs that have no open delivery bead and no
// open decision bead, then recreates the decision bead so gc-merge-sweep can
// pick up the PR. Only PRs on gc/<bead-id> branches are considered.
// Idempotent: a second call with the same state creates no additional beads.
func (w *Warden) RepairOrphan(owner, repo string) error {
	prs, err := w.github.ListOpenPRs(owner, repo)
	if err != nil {
		return fmt.Errorf("RepairOrphan: ListOpenPRs(%s/%s): %w", owner, repo, err)
	}

	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRef, "gc/") {
			continue // not a delivery branch
		}

		// Is there still an open delivery bead tracking this PR?
		_, found, err := delivery.FindDeliveryBeadByPRURL(w.store, pr.URL)
		if err != nil {
			return fmt.Errorf("RepairOrphan: FindDeliveryBeadByPRURL PR %d: %w", pr.Number, err)
		}
		if found {
			continue // delivery bead still live; not orphaned
		}

		// Is there already an open decision bead for this PR?
		prNumStr := strconv.Itoa(pr.Number)
		existing, err := w.store.ListByMetadata(map[string]string{"gc.merge_pr": prNumStr}, 1)
		if err != nil {
			return fmt.Errorf("RepairOrphan: check decision bead for PR %d: %w", pr.Number, err)
		}
		if len(existing) > 0 {
			continue // already repaired (idempotent)
		}

		// Find the closed source bead to record gc.merge_source.
		sourceID := ""
		closed, err := w.store.ListByMetadata(
			map[string]string{delivery.MetaKeyPRURL: pr.URL},
			1,
			beads.IncludeClosed,
		)
		if err == nil && len(closed) > 0 {
			sourceID = closed[0].ID
		}

		// Recreate the tacit-consent decision bead.
		_, err = w.store.Create(beads.Bead{
			Title: fmt.Sprintf("merge-decision: %s/%s#%d (orphan repair)", owner, repo, pr.Number),
			Type:  "decision",
			Metadata: map[string]string{
				"gc.merge_pr":     prNumStr,
				"gc.merge_repo":   fmt.Sprintf("%s/%s", owner, repo),
				"gc.merge_source": sourceID,
			},
		})
		if err != nil {
			return fmt.Errorf("RepairOrphan: create decision bead for PR %d: %w", pr.Number, err)
		}
	}
	return nil
}

// RepairZombie finds open delivery beads whose GitHub PR is already merged or
// closed, and transitions them to a terminal phase + closes them.
// Zombie repair bypasses the phase-transition table (SetMetadataBatch instead
// of SetPhase) because the PR state on GitHub is ground truth and the bead may
// be in any intermediate phase.
func (w *Warden) RepairZombie() error {
	open, err := w.store.ListOpen()
	if err != nil {
		return fmt.Errorf("RepairZombie: list open beads: %w", err)
	}

	for _, b := range open {
		prURL := b.Metadata[delivery.MetaKeyPRURL]
		if prURL == "" {
			continue // not a delivery bead
		}
		phase := b.Metadata[delivery.MetaKeyPhase]
		if phase == "" || delivery.IsTerminalPhase(phase) {
			continue // not a tracked delivery bead or already terminal
		}

		pr, err := w.github.GetPR(prURL)
		if err != nil {
			// Fail-open: log and continue rather than aborting the sweep.
			continue
		}

		var terminalPhase string
		switch pr.State {
		case "MERGED":
			terminalPhase = delivery.PhaseMerged
		case "CLOSED":
			terminalPhase = delivery.PhaseAbandoned
		default:
			continue // OPEN: not a zombie
		}

		// Force the terminal phase directly — the PR is already resolved on
		// GitHub, so walking the normal transition table is inappropriate.
		if err := w.store.SetMetadataBatch(b.ID, map[string]string{
			delivery.MetaKeyPhase: terminalPhase,
		}); err != nil {
			return fmt.Errorf("RepairZombie: set terminal phase on %s: %w", b.ID, err)
		}
		if err := w.store.Close(b.ID); err != nil {
			return fmt.Errorf("RepairZombie: close zombie bead %s: %w", b.ID, err)
		}
	}
	return nil
}

// CheckPhaseDwell checks every open delivery bead for phase stalls. If a bead
// has been in its current phase longer than the per-phase budget, it increments
// gc.warden_retries and dispatches a recovery action (nudge reviewer or re-route
// to executor). Once retries reach 3, it sends a single escalation mail and sets
// gc.warden_escalated; subsequent calls are no-ops for that bead.
func (w *Warden) CheckPhaseDwell() error {
	open, err := w.store.ListOpen()
	if err != nil {
		return fmt.Errorf("CheckPhaseDwell: list open beads: %w", err)
	}

	now := w.now()

	for _, b := range open {
		phase := b.Metadata[delivery.MetaKeyPhase]
		if phase == "" || delivery.IsTerminalPhase(phase) {
			continue
		}

		budget, ok := phaseDwellBudgets[phase]
		if !ok {
			continue
		}

		// Bootstrap gc.phase_entered_at on first encounter; skip this tick.
		enteredAtStr := b.Metadata[metaKeyPhaseEnteredAt]
		if enteredAtStr == "" {
			_ = w.store.SetMetadata(b.ID, metaKeyPhaseEnteredAt, strconv.FormatInt(now.Unix(), 10))
			continue
		}

		enteredAtUnix, err := strconv.ParseInt(enteredAtStr, 10, 64)
		if err != nil {
			continue
		}

		if now.Sub(time.Unix(enteredAtUnix, 0)) <= budget {
			continue
		}

		// Already escalated — skip (idempotent).
		if b.Metadata[metaKeyWardenEscalated] != "" {
			continue
		}

		retries, _ := strconv.Atoi(b.Metadata[metaKeyWardenRetries])

		if retries >= 3 {
			subject := fmt.Sprintf("stall: %s phase=%s", b.ID, phase)
			body := fmt.Sprintf("Bead %s stalled in %s (budget %s, %d retries exhausted).", b.ID, phase, budget, retries)
			if err := w.mail.Send("voxist-platform/voxist.warden", "voxist-platform/human", subject, body); err != nil {
				return fmt.Errorf("CheckPhaseDwell: escalate %s: %w", b.ID, err)
			}
			if err := w.store.SetMetadata(b.ID, metaKeyWardenEscalated, "1"); err != nil {
				return fmt.Errorf("CheckPhaseDwell: set escalated on %s: %w", b.ID, err)
			}
		} else {
			target, ok := recoveryTarget[phase]
			if !ok {
				target = "voxist-platform/voxist.reviewer"
			}
			subject := fmt.Sprintf("phase stall: %s phase=%s (retry %d)", b.ID, phase, retries+1)
			body := fmt.Sprintf("Bead %s stalled in %s for >%s.", b.ID, phase, budget)
			if err := w.mail.Send("voxist-platform/voxist.warden", target, subject, body); err != nil {
				return fmt.Errorf("CheckPhaseDwell: recovery nudge %s: %w", b.ID, err)
			}
			if err := w.store.SetMetadata(b.ID, metaKeyWardenRetries, strconv.Itoa(retries+1)); err != nil {
				return fmt.Errorf("CheckPhaseDwell: update retries on %s: %w", b.ID, err)
			}
		}
	}
	return nil
}

// CheckGlobalLifetime escalates any delivery bead that has been alive for more
// than maxLifetime (24 h) regardless of phase or retry count. The escalation
// is idempotent: once gc.warden_escalated=global is set the bead is skipped
// on subsequent sweeps.
func (w *Warden) CheckGlobalLifetime() error {
	open, err := w.store.ListOpen()
	if err != nil {
		return fmt.Errorf("CheckGlobalLifetime: list open beads: %w", err)
	}

	now := w.now()

	for _, b := range open {
		if b.Metadata[delivery.MetaKeyPhase] == "" {
			continue // not a delivery bead
		}
		if b.Metadata[metaKeyWardenEscalated] == "global" {
			continue // already escalated (idempotent)
		}
		if now.Sub(b.CreatedAt) <= maxLifetime {
			continue // within lifetime budget
		}

		prURL := b.Metadata[delivery.MetaKeyPRURL]
		subject := fmt.Sprintf("lifetime breach: %s", b.ID)
		body := fmt.Sprintf("Bead %s is %s old (max %s). PR: %s", b.ID, now.Sub(b.CreatedAt).Round(time.Minute), maxLifetime, prURL)
		if err := w.mail.Send("voxist-platform/voxist.warden", "voxist-platform/human", subject, body); err != nil {
			return fmt.Errorf("CheckGlobalLifetime: escalate %s: %w", b.ID, err)
		}
		if err := w.store.SetMetadata(b.ID, metaKeyWardenEscalated, "global"); err != nil {
			return fmt.Errorf("CheckGlobalLifetime: set escalated on %s: %w", b.ID, err)
		}
	}
	return nil
}

// WriteHeartbeat writes the current epoch timestamp (in seconds) to the
// heartbeat file so the launchd supervisor can detect a stale warden.
// heartbeatFile defaults to /tmp/gc-delivery-warden.heartbeat when empty.
func WriteHeartbeat(heartbeatFile string) error {
	if heartbeatFile == "" {
		heartbeatFile = "/tmp/gc-delivery-warden.heartbeat"
	}
	content := fmt.Sprintf("%d\n", time.Now().Unix())
	return os.WriteFile(heartbeatFile, []byte(content), 0o600)
}

// Sweep runs all five reconcile rules in order, then writes the heartbeat.
// It is fail-open: an error in one rule does not prevent subsequent rules from
// running; all errors are collected and returned as a joined error.
func (w *Warden) Sweep(repos [][2]string, heartbeatFile string) error {
	var errs []error

	for _, ownerRepo := range repos {
		if err := w.RepairOrphan(ownerRepo[0], ownerRepo[1]); err != nil {
			errs = append(errs, err)
		}
	}
	if err := w.RepairZombie(); err != nil {
		errs = append(errs, err)
	}
	if err := w.CheckPhaseDwell(); err != nil {
		errs = append(errs, err)
	}
	if err := w.CheckGlobalLifetime(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		if err := WriteHeartbeat(heartbeatFile); err != nil {
			errs = append(errs, fmt.Errorf("WriteHeartbeat: %w", err))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return fmt.Errorf("warden sweep errors: %s", strings.Join(msgs, "; "))
}
