package main

import (
	"fmt"
	"strconv"
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
// record ops, else OK, plus a Warning for expired legacy ops left open or a
// scan for them that stopped at its page cap.
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
	ops, undecodable, truncated, err := st.ListOpenLegacy(opSweepLimit)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("ops recording on (%s); listing open legacy ops failed: %v", gates, err)
		return r
	}
	stale, oldest := c.staleOps(r, ops)
	if len(undecodable) > 0 {
		r.Details = append(r.Details, fmt.Sprintf("%d op row(s) this gc cannot decode, which a newer gc may have written:%s",
			len(undecodable), opSweepIDs(undecodable)))
	}
	switch {
	case stale > 0:
		count := strconv.Itoa(stale)
		// The read stopped at the limit or the page cap, so more may wait behind it.
		if truncated || len(ops) == opSweepLimit {
			count = "at least " + count
		}
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("ops recording on (%s); %s open legacy op(s) expired more than %s ago (oldest finish_by %s): the controller's op sweeper is not closing them",
			gates, count, opsRecordingStaleAfter, oldest.UTC().Format(time.RFC3339))
		r.FixHint = "check that the controller is running and look for op-sweep lines in its log; " +
			"restart the controller if the [ops] gates changed since it started (it latches them at boot, " +
			"while each CLI reads them per process)"
	case truncated:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("ops recording on (%s): the sessions store can record ops, but the scan for expired legacy ops stopped at its page cap "+
			"behind a head of lane or undecodable op rows, so the legacy ops behind that head were not checked", gates)
		r.FixHint = "upgrade this gc if a newer gc wrote the rows it cannot decode (see the details), or find why the lane ops are still open; " +
			"the controller's op sweeper misses the same legacy ops until that head shrinks"
	default:
		r.Message = fmt.Sprintf("ops recording on (%s): the sessions store can record ops", gates)
	}
	return r
}

// staleOps counts the ops more than opsRecordingStaleAfter past their
// finish_by, adding the first ten to r's details, and returns the oldest such
// finish_by. ListOpenLegacy returns only open legacy ops.
func (c *opsRecordingDoctorCheck) staleOps(r *doctor.CheckResult, ops []opjournal.Op) (stale int, oldest time.Time) {
	now := c.clk.Now()
	for _, op := range ops {
		if now.Sub(op.FinishBy) <= opsRecordingStaleAfter {
			continue
		}
		if stale == 0 || op.FinishBy.Before(oldest) {
			oldest = op.FinishBy
		}
		stale++
		if len(r.Details) < 10 {
			r.Details = append(r.Details, fmt.Sprintf("%s (%s, %s): finish_by %s", op.ID, op.Kind, op.Executor, op.FinishBy.UTC().Format(time.RFC3339)))
		}
	}
	return stale, oldest
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
