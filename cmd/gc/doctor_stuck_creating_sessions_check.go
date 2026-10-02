package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// stuckCreatingSessionsCheck reports session beads wedged mid-create that the
// sweep and `gc session wake` already judge abandoned: the pending-create
// lease has lapsed and the attempt is past the staleness bound. That judgement
// was computed and discarded (sessionStartRequestedInfo returns on
// PendingCreateClaim before staleness is consulted), so nothing told an
// operator that a session had held its pool slot, and the work routed behind
// it, for days.
//
// Detection only: releasing the slot is `gc session close`, an operator call.
type stuckCreatingSessionsCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newStuckCreatingSessionsCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *stuckCreatingSessionsCheck {
	return &stuckCreatingSessionsCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *stuckCreatingSessionsCheck) Name() string { return "stuck-creating-sessions" }

// CanFix returns false: closing a session releases its slot, which only an
// operator who knows the runtime is gone can decide.
func (c *stuckCreatingSessionsCheck) CanFix() bool { return false }

// Fix is a no-op. Detection only: it never closes a session or touches a bead.
func (c *stuckCreatingSessionsCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *stuckCreatingSessionsCheck) WarmupEligible() bool { return false }

// stuckCreatingFinding is one abandoned in-flight create in one store scope.
type stuckCreatingFinding struct {
	scope         string
	id            string
	template      string
	state         string
	since         time.Time // zero when neither pending_create_started_at nor CreatedAt is recorded
	routed        int
	routedErr     error // non-nil means routed is unmeasured, never a zero
	countsAgainst bool
	maxSessions   *int
}

func (f stuckCreatingFinding) age(now time.Time) string {
	if f.since.IsZero() {
		return "an unknown time"
	}
	return formatDuration(now.Sub(f.since))
}

func (f stuckCreatingFinding) routedText() string {
	if f.routedErr != nil {
		return "routed beads unknown"
	}
	return fmt.Sprintf("%d unclaimed routed bead(s)", f.routed)
}

// summary is the form carried in the Message, which doctor always prints.
func (f stuckCreatingFinding) summary(now time.Time) string {
	return fmt.Sprintf("%s stuck in %s for %s (%s)", f.id, f.state, f.age(now), f.routedText())
}

// describe is the verbose per-session line carried in Details.
func (f stuckCreatingFinding) describe(now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s session %s (template %s) stuck in %s for %s", f.scope, f.id, f.template, f.state, f.age(now))
	if !f.since.IsZero() {
		fmt.Fprintf(&b, " (since %s)", f.since.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "; %s", f.routedText())
	if f.routedErr != nil {
		fmt.Fprintf(&b, ": %v", f.routedErr)
	}
	if f.countsAgainst {
		b.WriteString("; counts against capacity")
		if f.maxSessions != nil {
			fmt.Fprintf(&b, " (max_active_sessions=%d)", *f.maxSessions)
		}
	}
	return b.String()
}

// Run puts the id, age and routed-work count in the Message because doctor
// prints Details only under -v and a stuck session should be loud without it.
func (c *stuckCreatingSessionsCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil || c.newStore == nil {
		return warnCheck(c.Name(), "stuck-creating check could not run: no config or bead store available",
			"fix city.toml and bead store access, then rerun gc doctor", nil)
	}
	now := time.Now()
	findings, skipped := c.collect()
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no session is stuck in creating")
	}
	details := make([]string, 0, len(findings)+len(skipped))
	summaries := make([]string, 0, len(findings))
	for _, f := range findings {
		details = append(details, f.describe(now))
		summaries = append(summaries, f.summary(now))
	}
	details = append(details, skipped...)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("stuck-creating check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	msg := fmt.Sprintf("%d session(s) with an abandoned create: %s", len(findings), strings.Join(summaries, "; "))
	if len(skipped) > 0 {
		msg = fmt.Sprintf("%s; %d scope(s) skipped", msg, len(skipped))
	}
	return warnCheck(c.Name(),
		msg,
		"if a session's runtime is gone, release its slot with gc session close <session-id>; gc session wake cannot complete a stuck create",
		details)
}

// collect scans every in-scope bead store (the city plus every non-suspended,
// path-bearing rig) for abandoned in-flight creates, using the same scope
// iteration as poolIdleRoutedWorkCheck so the two agree on what "in scope" means.
func (c *stuckCreatingSessionsCheck) collect() (findings []stuckCreatingFinding, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
	for _, rig := range c.cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
	}
	for _, sc := range scopes {
		if strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		scopeFindings, err := c.collectStoreFindings(store, sc.label)
		findings = append(findings, scopeFindings...)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: %v", sc.label, err))
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].scope != findings[j].scope {
			return findings[i].scope < findings[j].scope
		}
		return findings[i].id < findings[j].id
	})
	return findings, skipped
}

// collectStoreFindings lists one store's sessions and measures the unclaimed
// routed work behind each abandoned create. "Abandoned" is
// sessionWakeCreateAbandonedInfo, the predicate `gc session wake` and the
// sweep already use: it checks the pending-create lease before staleness, so a
// create the reconciler still protects is not reported.
func (c *stuckCreatingSessionsCheck) collectStoreFindings(store beads.Store, label string) ([]stuckCreatingFinding, error) {
	sessions, err := cliSessionFrontDoor(store, c.cfg, c.cityPath).List("", "")
	if err != nil {
		return nil, err
	}
	startupTimeout := c.cfg.Session.StartupTimeoutDuration()

	var findings []stuckCreatingFinding
	for _, info := range sessions {
		if !sessionWakeCreateAbandonedInfo(info, startupTimeout) {
			continue
		}
		f := stuckCreatingFinding{
			scope:         label,
			id:            info.ID,
			template:      normalizedSessionTemplateInfo(info, c.cfg),
			state:         strings.TrimSpace(info.MetadataState),
			since:         stuckCreatingSinceInfo(info),
			countsAgainst: sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info)).CountsAgainstCap,
		}
		if agent := sessionWakeResolveAgentInfo(info, c.cfg); agent != nil {
			f.maxSessions = agent.EffectiveMaxActiveSessions()
		}
		f.routed, f.routedErr = routedWorkCount(store, f.template)
		findings = append(findings, f)
	}
	return findings, nil
}

// routedWorkCount counts the unclaimed work routed to template. A metadata
// filter with an empty value matches every bead that lacks the key, so a
// session with no template has no measurable lane: report that as an error
// rather than count all unrouted work against it.
func routedWorkCount(store beads.Store, template string) (int, error) {
	if template == "" {
		return 0, errors.New("session has no template")
	}
	ids, err := unclaimedRoutedBeadIDs(store, template)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}
