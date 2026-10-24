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
// opjournal.NewStore over the city's sessions-class store. With a gate on,
// and with every gate off once that store holds an op row, it also warns
// about expired legacy ops the controller's sweeper has left open.
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

// Run implements doctor.Check. With a gate on: an Error (any gate require)
// or a Warning (auto only) when the store cannot record ops, else OK. With
// every gate off: OK while the sessions store holds no submitted op row
// (opjournal.HasSubmittedOps), and a Warning when that cannot be read. Then,
// for either, a Warning for expired legacy ops left open or a scan for them
// that stopped at its page cap: with every gate off the sweeper stops once a
// pass leaves no work, so the ops it leaves are reported here.
func (c *opsRecordingDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	if c.resolveErr != nil {
		r.Message = "ops recording not checked: the rollout gates did not resolve (see the rollout checks)"
		return r
	}
	on, require := c.gatesOn()
	gates := strings.Join(on, ", ")
	store, err := c.open()
	if err != nil {
		err = fmt.Errorf("opening the city store: %w", err)
		if gates == "" {
			return offCannotRead(r, err)
		}
		return c.cannotRecord(r, gates, require, legacyop.DegradeStoreError, err)
	}
	defer func() { _ = closeBeadStoreHandle(store) }()
	sessions := beads.SessionStore{Store: c.sessions(store)}
	if gates == "" {
		// Nothing to sweep until an op row exists, and no op store to open:
		// with every gate off the store need not be able to record ops.
		has, err := opjournal.HasSubmittedOps(sessions)
		if err != nil {
			return offCannotRead(r, err)
		}
		if !has {
			r.Message = "ops recording is off"
			return r
		}
	}
	st, err := opjournal.NewStore(sessions, c.clk)
	if err != nil {
		if gates == "" {
			return offCannotRead(r, err)
		}
		return c.cannotRecord(r, gates, require, legacyop.DegradeReason(err), err)
	}
	return c.scanExpired(r, st, gates)
}

// scanExpired warns about the open legacy ops more than
// opsRecordingStaleAfter past their finish_by, and about a scan for them that
// stopped at its page cap. gates lists the gates that are on; it is empty
// with every gate off.
func (c *opsRecordingDoctorCheck) scanExpired(r *doctor.CheckResult, st *opjournal.Store, gates string) *doctor.CheckResult {
	state, capable := "ops recording is off", "ops recording is off"
	if gates != "" {
		state = "ops recording on (" + gates + ")"
		capable = state + ": the sessions store can record ops"
	}
	scan, err := st.ListExpiredLegacy(c.clk.Now().Add(-opsRecordingStaleAfter), opSweepLimit)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("%s; listing expired legacy ops failed: %v", state, err)
		return r
	}
	oldest := staleOpDetails(r, scan.Expired)
	if len(scan.Undecodable) > 0 {
		r.Details = append(r.Details, fmt.Sprintf("%d op row(s) this gc cannot decode (a newer gc wrote them, or they are not valid to this one):%s",
			len(scan.Undecodable), opSweepIDs(scan.Undecodable)))
	}
	switch {
	case len(scan.Expired) > 0:
		count := strconv.Itoa(len(scan.Expired))
		// The read stopped at the limit or the page cap, so more may wait behind it.
		if scan.Full || scan.Truncated {
			count = "at least " + count
		}
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("%s; %s open legacy op(s) expired more than %s ago (oldest finish_by %s): the controller's op sweeper is not closing them",
			state, count, opsRecordingStaleAfter, oldest.UTC().Format(time.RFC3339))
		r.FixHint = "check that the controller is running and look for op-sweep lines in its log; " +
			"restart the controller if the [ops] gates changed since it started (it latches them at boot, " +
			"while each CLI reads them per process)"
		if gates == "" {
			r.FixHint = "restart the controller: with every ops gate off its op sweeper runs from its start only until a pass leaves no work, " +
				"so a legacy op that expires after that (one a gc run with a GC_OPS_* override recorded) stays open; " +
				"look for op-sweep lines in its log if a restart does not close them"
		}
	case scan.Truncated:
		r.Status = doctor.StatusWarning
		r.Message = capable + ", but the scan for expired legacy ops stopped at its page cap " +
			"behind a head of lane, undecodable or not yet stale legacy op rows, so the legacy ops behind that head were not checked"
		r.FixHint = "inspect the rows this gc cannot decode (see the details), and upgrade it if a newer gc wrote them, or find why so many lane or legacy ops are still open; " +
			"the controller's op sweeper misses the legacy ops behind a head of lane, undecodable or in-flight rows the same way, until that head shrinks"
		if gates == "" {
			r.FixHint += " (with every ops gate off it stops at that head, so restart the controller once the head has shrunk)"
		}
	default:
		r.Message = capable
	}
	return r
}

// staleOpDetails adds the first ten of the stale ops to r's details and
// returns the oldest finish_by among them.
func staleOpDetails(r *doctor.CheckResult, stale []opjournal.Op) (oldest time.Time) {
	for i, op := range stale {
		if i == 0 || op.FinishBy.Before(oldest) {
			oldest = op.FinishBy
		}
		if len(r.Details) < 10 {
			r.Details = append(r.Details, fmt.Sprintf("%s (%s, %s): finish_by %s", op.ID, op.Kind, op.Executor, op.FinishBy.UTC().Format(time.RFC3339)))
		}
	}
	return oldest
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
	r.FixHint = opsRecordingRemedy(reason, true)
	return r
}

// offCannotRead reports, with every gate off, a sessions store that could
// not be checked for expired legacy ops: a Warning, as nothing is refused,
// but the ops the sweeper may have left open go unseen.
func offCannotRead(r *doctor.CheckResult, err error) *doctor.CheckResult {
	reason := legacyop.DegradeReason(err)
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("ops recording is off, but the sessions store could not be checked for expired legacy ops (%s)", reason)
	r.Details = append(r.Details, err.Error())
	r.FixHint = opsRecordingRemedy(reason, false)
	return r
}

// opsRecordingRemedy is the remedy for a sessions store that cannot record
// ops for reason; gatesOn offers setting the gates to off as one.
func opsRecordingRemedy(reason string, gatesOn bool) string {
	orOff := ""
	if gatesOn {
		orOff = ", or set the ops gates to off"
	}
	switch reason {
	case legacyop.DegradeConditionalWritesUnsupported:
		return "use a sessions store with conditional writes (for example a bd that supports --if-revision)" + orOff
	case legacyop.DegradeNoNamespace:
		return "configure an id prefix for the sessions store" + orOff
	default:
		return "check that the sessions store is reachable"
	}
}
