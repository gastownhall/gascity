package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
)

// opsRecordingStaleAfter is how long past its finish_by an open legacy op
// may stay before the check says the controller's sweeper is not closing it:
// two sweep intervals.
const opsRecordingStaleAfter = 2 * opSweepInterval

// opsRecordingDoctorCheck reports whether the city can record ops for the
// ops gates that are on ([ops] content, control, lifecycle): it probes
// opjournal.NewStore over the city's sessions-class store, and warns about
// expired legacy ops the controller's sweeper has left open.
type opsRecordingDoctorCheck struct {
	cfg        *config.City
	flags      rollout.Flags
	resolveErr error
	// open opens the city store; the check closes it after.
	open func() (beads.Store, error)
	// sessions routes the city store to its sessions class.
	sessions func(beads.Store) beads.Store
	clk      clock.Clock
}

func newOpsRecordingDoctorCheck(cityPath string, cfg *config.City, flags rollout.Flags, resolveErr error) *opsRecordingDoctorCheck {
	return &opsRecordingDoctorCheck{
		cfg:        cfg,
		flags:      flags,
		resolveErr: resolveErr,
		open:       func() (beads.Store, error) { return openCityStoreAtWithConfig(cityPath, cfg) },
		sessions:   func(store beads.Store) beads.Store { return cliSessionStore(store, cfg, cityPath) },
		clk:        clock.Real{},
	}
}

// Name implements doctor.Check.
func (*opsRecordingDoctorCheck) Name() string { return "ops-recording" }

// CanFix implements doctor.Check.
func (*opsRecordingDoctorCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check.
func (*opsRecordingDoctorCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*opsRecordingDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check: OK with every gate off; with a gate on, an
// Error (any gate require) or a Warning (auto only) when the store cannot
// record ops, else OK, plus a Warning for expired legacy ops left open.
func (c *opsRecordingDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	if c.resolveErr != nil {
		r.Message = "ops recording not checked: the rollout gates did not resolve (see the rollout checks)"
		return r
	}
	on, require := c.gatesOn()
	if len(on) == 0 {
		r.Message = "ops recording is off"
		return r
	}
	gates := strings.Join(on, ", ")
	store, err := c.open()
	if err != nil {
		return c.cannotRecord(r, gates, require, legacyop.DegradeStoreError, fmt.Errorf("opening the city store: %w", err))
	}
	defer func() { _ = closeBeadStoreHandle(store) }()
	st, err := opjournal.NewStore(beads.SessionStore{Store: c.sessions(store)}, c.clk)
	if err != nil {
		return c.cannotRecord(r, gates, require, legacyop.DegradeReason(err), err)
	}
	ops, _, _, err := st.ListOpenLegacy(opSweepLimit)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("ops recording on (%s); listing open legacy ops failed: %v", gates, err)
		return r
	}
	now := c.clk.Now()
	stale := 0
	var oldest time.Time
	for _, op := range ops {
		if op.Executor.Legacy() && op.Status != opjournal.StatusClosed && now.Sub(op.FinishBy) > opsRecordingStaleAfter {
			if stale == 0 || op.FinishBy.Before(oldest) {
				oldest = op.FinishBy
			}
			stale++
			if len(r.Details) < 10 {
				r.Details = append(r.Details, fmt.Sprintf("%s (%s, %s): finish_by %s", op.ID, op.Kind, op.Executor, op.FinishBy.UTC().Format(time.RFC3339)))
			}
		}
	}
	if stale > 0 {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("ops recording on (%s); %d open legacy op(s) expired more than %s ago (oldest finish_by %s): the controller's op sweeper is not closing them",
			gates, stale, opsRecordingStaleAfter, oldest.UTC().Format(time.RFC3339))
		r.FixHint = "check that the controller is running and look for op-sweep lines in its log; " +
			"restart the controller if the [ops] gates changed since it started (it latches them at boot, " +
			"while each CLI reads them per process)"
		return r
	}
	r.Message = fmt.Sprintf("ops recording on (%s): the sessions store can record ops", gates)
	return r
}

// gatesOn lists the gates that are not off, as key=mode, and whether any is
// require.
func (c *opsRecordingDoctorCheck) gatesOn() (on []string, require bool) {
	for _, g := range []struct {
		key  string
		mode rollout.Mode
	}{
		{rollout.KeyOpsContent, c.flags.OpsContent()},
		{rollout.KeyOpsControl, c.flags.OpsControl()},
		{rollout.KeyOpsLifecycle, c.flags.OpsLifecycle()},
	} {
		if g.mode == rollout.Auto || g.mode == rollout.Require {
			on = append(on, g.key+"="+string(g.mode))
			require = require || g.mode == rollout.Require
		}
	}
	return on, require
}

// cannotRecord reports a store the gates cannot record into: an Error when a
// gate is require (its requests are refused), else a Warning (they run
// unrecorded).
func (c *opsRecordingDoctorCheck) cannotRecord(r *doctor.CheckResult, gates string, require bool, reason string, err error) *doctor.CheckResult {
	effect := "requests run unrecorded"
	r.Status = doctor.StatusWarning
	if require {
		effect = "require refuses every request it cannot record"
		r.Status = doctor.StatusError
	}
	r.Message = fmt.Sprintf("ops recording on (%s) but the sessions store cannot record ops (%s): %s", gates, reason, effect)
	r.Details = append(r.Details, err.Error())
	r.FixHint = opsRecordingRemedy(reason)
	return r
}

// opsRecordingRemedy is the remedy for a sessions store that cannot record
// ops for reason.
func opsRecordingRemedy(reason string) string {
	switch reason {
	case legacyop.DegradeConditionalWritesUnsupported:
		return "use a sessions store with conditional writes (for example a bd that supports --if-revision), or set the ops gates to off"
	case legacyop.DegradeNoNamespace:
		return "configure an id prefix for the sessions store, or set the ops gates to off"
	default:
		return "check that the sessions store is reachable"
	}
}
