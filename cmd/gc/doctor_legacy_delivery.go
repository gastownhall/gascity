package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// legacyDeliveryDoctorCheck reports legacy delivery (OJ-21 21a, D-NOLANE):
// the nudge queue's depth, the city's live gc nudge poll sidecars, whether
// the city's sessions store can host the conversation lane (laneStoreFitFor),
// and, from the running controller's lane-status, whether the lane runs and
// the legacy-delivery deprecations that apply where it does
// (laneLegacyDeliveryWarnings). Lanes need a split store, and legacy
// delivery is removed in a future release (one after gc init splits storage
// by default), so a city whose sessions store cannot host lanes is told to
// split, and one whose store hosts them off SQLite is noted that a split is
// cheaper. It is never fixable.
type legacyDeliveryDoctorCheck struct {
	cfg *config.City
	// queue reads the city's nudge queue; pollers counts its live pollers;
	// fit probes its sessions store; query asks its controller for the lane
	// host's status.
	queue   func(cityPath string) (nudgequeue.State, error)
	pollers func(cityPath string) (int, error)
	fit     func() (laneStoreFit, string, error)
	query   func(cityPath string) (conversationLaneStatus, error)
}

// legacyDeliverySplitRemedy is where a city learns to split its storage.
const legacyDeliverySplitRemedy = "docs/runbooks/split-storage-classes.md, gc storage migrate"

func newLegacyDeliveryDoctorCheck(cfg *config.City, fit func() (laneStoreFit, string, error)) *legacyDeliveryDoctorCheck {
	return &legacyDeliveryDoctorCheck{
		cfg:     cfg,
		queue:   nudgequeue.LoadState,
		pollers: liveCityNudgePollers,
		fit:     fit,
		query:   queryConversationLaneStatus,
	}
}

// Name implements doctor.Check.
func (*legacyDeliveryDoctorCheck) Name() string { return "legacy-delivery" }

// CanFix implements doctor.Check.
func (*legacyDeliveryDoctorCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check: the lane's kinds are the running
// controller's.
func (*legacyDeliveryDoctorCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*legacyDeliveryDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check: an advisory Warning when the sessions store
// cannot host the conversation lane (split the storage), naming each
// deprecation that applies where the controller's lane host runs kinds (or
// holds back the default with an explicit []), or when the queue or the
// pollers cannot be read; else OK, saying whether the lane or legacy
// delivery serves the city, with a note where a split would move lane
// records to SQLite. The queue's depth, the live pollers and a sessions
// store that could not be probed are details either way.
func (c *legacyDeliveryDoctorCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	if ctx == nil || ctx.CityPath == "" {
		r.Message = "no city"
		return r
	}
	var problems, hints []string
	state, err := c.queue(ctx.CityPath)
	if err != nil {
		problems = append(problems, fmt.Sprintf("reading the nudge queue: %v", err))
	}
	pollers, err := c.pollers(ctx.CityPath)
	if err != nil {
		problems = append(problems, fmt.Sprintf("counting nudge pollers: %v", err))
	}
	r.Details = append(r.Details,
		fmt.Sprintf("nudge queue: %d pending, %d in flight, %d dead", len(state.Pending), len(state.InFlight), len(state.Dead)),
		fmt.Sprintf("live nudge pollers: %d", pollers))

	var note string
	switch fit, binding, ferr := c.fit(); {
	case ferr != nil:
		r.Details = append(r.Details, fmt.Sprintf("sessions store: not read (%v)", ferr))
	case fit == laneStoreFitNone:
		problems = append(problems, fmt.Sprintf("the sessions store cannot host conversation lanes (it is bound to %s), and legacy delivery is removed in a future release", binding))
		hints = append(hints, "split this city's storage so its sessions class moves to SQLite and runs the conversation lane ("+legacyDeliverySplitRemedy+"); "+
			"once legacy delivery is removed, a city whose sessions store cannot host lanes must split before it starts")
	case fit == laneStoreFitListed:
		note = "splitting this city's storage moves lane records to SQLite, which is cheaper"
		r.Details = append(r.Details, "note: "+note+" ("+legacyDeliverySplitRemedy+")")
	}

	st, qerr := c.query(ctx.CityPath)
	lanes := qerr == nil && st.State == laneHostStateRunning && len(st.Kinds) > 0
	if qerr == nil && st.State == laneHostStateRunning {
		if warnings := laneLegacyDeliveryWarnings(c.cfg, st.Kinds, st.DefaultApplies); len(warnings) > 0 {
			problems = append(problems, warnings...)
			hints = append(hints, "keep the deprecated keys until legacy delivery is removed in a future release; set [ops] authoritative = [] only as a rollback")
		}
	}
	switch {
	case lanes:
		r.Message = fmt.Sprintf("the conversation lane delivers %s; legacy delivery drains leftovers and is removed in a future release", strings.Join(st.Kinds, ", "))
	case qerr != nil:
		r.Message = "legacy delivery: the controller did not answer lane-status, so whether the conversation lane runs is unknown"
	default:
		r.Message = "legacy delivery serves this city; the conversation lane does not run here"
	}
	if len(problems) > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message += ": " + strings.Join(problems, "; ")
		if len(hints) == 0 {
			hints = append(hints, "fix what reading legacy delivery's state reported, then rerun gc doctor")
		}
		r.FixHint = strings.Join(hints, "; ")
	}
	if note != "" {
		r.Message += "; note: " + note
	}
	return r
}
