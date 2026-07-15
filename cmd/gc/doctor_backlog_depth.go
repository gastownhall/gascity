package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// backlogDepthCheck reports the city store's claimable backlog depth by
// classifying the raw open-bead population into actionable work versus
// observability noise. `bd ready` (the beads CLI) lists control-plane session
// beads and short-lived nudge/mail notification chores alongside real work, so
// raw queue depth reads as a huge backlog even when only one task is actually
// claimable — the recurring "fleet idle / backlog huge" false alarm
// (gastownhall/gascity#3021).
//
// The check reads the city beads store only; work tracked in rig stores is not
// included. It is pure observability: it never mutates beads and never gates
// (SeverityAdvisory). It does not touch the claiming path — agents' work
// queries already filter this noise via the Ready-tier contract; the gap it
// closes is the operator/dashboard view.
type backlogDepthCheck struct {
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newBacklogDepthCheck(cityPath string, newStore func(string) (beads.Store, error)) *backlogDepthCheck {
	return &backlogDepthCheck{cityPath: cityPath, newStore: newStore}
}

func (c *backlogDepthCheck) Name() string { return "backlog-depth" }

func (c *backlogDepthCheck) CanFix() bool { return false }

func (c *backlogDepthCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *backlogDepthCheck) WarmupEligible() bool { return false }

// backlogBreakdown is the classified census of the raw open-bead population.
// The buckets are mutually exclusive and sum to total, so the operator can read
// the equation raw - control-plane - notification - epic - other = real.
type backlogBreakdown struct {
	total        int          // every status=open bead (the "raw" population)
	controlPlane int          // type=session / gc:session: the session registry
	notification int          // nudge:/mail: chores, type=message, gc:nudge
	epic         int          // epic parents: containers, not claimable leaves
	other        int          // deferred, dep-blocked, or other infra/excluded Ready-tier types
	real         []beads.Bead // genuinely claimable Ready-tier work
}

// isControlPlaneBacklogBead reports whether a bead is a controller-owned
// session-registry bead rather than claimable work.
func isControlPlaneBacklogBead(b beads.Bead) bool {
	return b.Type == sessionBeadType || hasLabel(b.Labels, sessionBeadLabel)
}

// isNotificationBacklogBead reports whether a bead is a short-lived delivery
// chore (nudge or mail) rather than durable backlog. It mirrors the
// nudge-mail-reaper notification predicate: the nudge:/mail: title prefix, the
// gc:nudge label, and the mail bead type.
func isNotificationBacklogBead(b beads.Bead) bool {
	if beadmail.IsMessageBead(b) || hasLabel(b.Labels, nudgeBeadLabel) {
		return true
	}
	title := strings.TrimSpace(b.Title)
	return strings.HasPrefix(title, "nudge:") || strings.HasPrefix(title, "mail:")
}

// classifyBacklog partitions the raw open-bead population into one bucket each
// (first match wins) so the buckets sum to total. "real" is the set of beads
// whose IDs appear in readyIDs — the dep-checked Ready output from the store —
// after subtracting the noise classes above.
func classifyBacklog(open []beads.Bead, readyIDs map[string]bool) backlogBreakdown {
	b := backlogBreakdown{total: len(open)}
	for _, bead := range open {
		switch {
		case isControlPlaneBacklogBead(bead):
			b.controlPlane++
		case isNotificationBacklogBead(bead):
			b.notification++
		case bead.Type == "epic":
			b.epic++
		case readyIDs[bead.ID]:
			b.real = append(b.real, bead)
		default:
			b.other++
		}
	}
	return b
}

func (c *backlogDepthCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}
	if c.newStore == nil || strings.TrimSpace(c.cityPath) == "" {
		res.Status = doctor.StatusWarning
		res.Message = "backlog depth unknown: no city bead store configured"
		return res
	}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("backlog depth unknown: opening city bead store: %v", err)
		return res
	}
	open, err := store.ListOpen("open")
	if err != nil {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("backlog depth unknown: listing open beads: %v", err)
		return res
	}
	ready, err := store.Ready()
	if err != nil {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("backlog depth unknown: listing ready beads: %v", err)
		return res
	}

	readyIDs := make(map[string]bool, len(ready))
	for _, r := range ready {
		readyIDs[r.ID] = true
	}
	b := classifyBacklog(open, readyIDs)

	res.Status = doctor.StatusOK
	res.Message = fmt.Sprintf(
		"city store: %d claimable (of %d open — %d control-plane, %d notification, %d epic, %d other)",
		len(b.real), b.total, b.controlPlane, b.notification, b.epic, b.other)

	details := make([]string, 0, len(b.real))
	for _, bead := range b.real {
		details = append(details, fmt.Sprintf("claimable: %s %s", bead.ID, strings.TrimSpace(bead.Title)))
	}
	sort.Strings(details)
	res.Details = details
	return res
}
