package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// legacyDeliveryDoctorCheck reports legacy delivery (OJ-21 21a): the nudge
// queue's depth, the city's live gc nudge poll sidecars, and, from the
// running controller's lane-status, whether the conversation lane runs and
// the legacy-delivery deprecations that apply where it does
// (laneLegacyDeliveryWarnings). Legacy delivery is removed in the next
// release, so a lane city's leftovers drain through it now. It opens no
// store and is never fixable.
type legacyDeliveryDoctorCheck struct {
	cfg *config.City
	// queue reads the city's nudge queue; pollers counts its live pollers;
	// query asks its controller for the lane host's status.
	queue   func(cityPath string) (nudgequeue.State, error)
	pollers func(cityPath string) (int, error)
	query   func(cityPath string) (conversationLaneStatus, error)
}

func newLegacyDeliveryDoctorCheck(cfg *config.City) *legacyDeliveryDoctorCheck {
	return &legacyDeliveryDoctorCheck{
		cfg:     cfg,
		queue:   nudgequeue.LoadState,
		pollers: liveCityNudgePollers,
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

// Run implements doctor.Check: an advisory Warning naming each deprecation
// that applies where the controller's lane host runs kinds (or holds back
// the default with an explicit []), or when the queue or the pollers cannot
// be read; else OK, saying whether the lane or legacy delivery serves the
// city. The queue's depth and the live pollers are details either way.
func (c *legacyDeliveryDoctorCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	if ctx == nil || ctx.CityPath == "" {
		r.Message = "no city"
		return r
	}
	var problems []string
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

	st, qerr := c.query(ctx.CityPath)
	lanes := qerr == nil && st.State == laneHostStateRunning && len(st.Kinds) > 0
	if qerr == nil && st.State == laneHostStateRunning {
		problems = append(problems, laneLegacyDeliveryWarnings(c.cfg, st.Kinds, st.DefaultApplies)...)
	}
	switch {
	case lanes:
		r.Message = fmt.Sprintf("the conversation lane delivers %s; legacy delivery drains leftovers and is removed in the next release", strings.Join(st.Kinds, ", "))
	case qerr != nil:
		r.Message = "legacy delivery: the controller did not answer lane-status, so whether the conversation lane runs is unknown"
	default:
		r.Message = "legacy delivery serves this city; the conversation lane does not run here"
	}
	if len(problems) > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message += ": " + strings.Join(problems, "; ")
		r.FixHint = "keep the deprecated keys until the next release, which retires them with legacy delivery; empty [ops] authoritative only to roll back for this release"
	}
	return r
}
