package main

import (
	"errors"
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
// or a Warning (auto only) when the store cannot record ops, else OK; gates
// that are auto only by their builtin default treat a store that cannot
// record ops at all as off (defaultCannotRecord), and record nothing on a
// sessions store that is not SQLite-backed (defaultNotRecorded). With
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
	if gates != "" && opsGatesBuiltinDefaultOnly(c.flags) {
		if err := opjournal.CheckRecordsByDefault(sessions); err != nil {
			return c.defaultNotRecorded(r, sessions, err)
		}
	}
	st, err := opjournal.NewStore(sessions, c.clk)
	if err != nil {
		if gates == "" {
			return offCannotRead(r, err)
		}
		reason := legacyop.DegradeReason(err)
		if legacyop.CannotRecord(reason) && opsGatesBuiltinDefaultOnly(c.flags) {
			return defaultCannotRecord(r, sessions, reason, err)
		}
		return c.cannotRecord(r, gates, require, reason, err)
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
	return c.scanExpiredAs(r, st, gates, state, capable)
}

// scanExpiredAs is scanExpired reporting state, and capable when nothing is
// wrong.
func (c *opsRecordingDoctorCheck) scanExpiredAs(r *doctor.CheckResult, st *opjournal.Store, gates, state, capable string) *doctor.CheckResult {
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
	for _, g := range opsGateModes(c.flags) {
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

// defaultCannotRecord reports a sessions store that cannot record ops under
// gates that are auto only by their builtin default, which treat it as off:
// OK while the store holds no submitted op row, as with every gate off, and a
// Warning when it holds one, since those ops cannot be checked or swept.
func defaultCannotRecord(r *doctor.CheckResult, sessions beads.SessionStore, reason string, err error) *doctor.CheckResult {
	has, herr := opjournal.HasSubmittedOps(sessions)
	if herr != nil {
		return offCannotRead(r, herr)
	}
	state := fmt.Sprintf("the ops gates are auto by default, but the sessions store cannot record ops (%s), so requests run unrecorded", reason)
	if !has {
		r.Message = state
		return r
	}
	r.Status = doctor.StatusWarning
	r.Message = state + "; it holds open op rows that cannot be checked or swept"
	r.Details = append(r.Details, err.Error())
	r.FixHint = opsRecordingRemedy(reason, false)
	return r
}

// defaultNotRecorded reports a sessions store the builtin auto default does
// not record on (err is the *opjournal.NotRecordedByDefaultError): OK, as
// with every gate off, so the store is only checked for the expired legacy
// ops an earlier explicit gate may have left, which the sweeper still closes.
// A store holding none is told whether an explicit gate would record there
// (explicitGateHint).
func (c *opsRecordingDoctorCheck) defaultNotRecorded(r *doctor.CheckResult, sessions beads.SessionStore, err error) *doctor.CheckResult {
	where := "a sessions store that is not SQLite-backed"
	var notDefault *opjournal.NotRecordedByDefaultError
	if errors.As(err, &notDefault) {
		if notDefault.Backend.DoltServed {
			where = "a Dolt-served sessions store"
		} else {
			where += " (" + notDefault.Backend.Kind + ")"
		}
	}
	state := "ops recording: auto (default) records nothing on " + where
	has, herr := opjournal.HasSubmittedOps(sessions)
	if herr != nil {
		return offCannotRead(r, herr)
	}
	if !has {
		r.Message = state + "; " + c.explicitGateHint(sessions)
		return r
	}
	st, oerr := opjournal.NewStore(sessions, c.clk)
	if oerr != nil {
		reason := legacyop.DegradeReason(oerr)
		if legacyop.CannotRecord(reason) {
			return defaultCannotRecord(r, sessions, reason, oerr)
		}
		return offCannotRead(r, oerr)
	}
	return c.scanExpiredAs(r, st, "", state, state)
}

// explicitGateHint says whether an explicit [ops] gate would record over
// sessions, a store the builtin default records nothing on: it opens the op
// store as such a gate would, capability probe included, so a store that
// cannot record ops at all (a released bd, a store with no id prefix) is not
// offered an opt-in that would only run its requests unrecorded, or refuse
// them under require.
func (c *opsRecordingDoctorCheck) explicitGateHint(sessions beads.SessionStore) string {
	_, err := opjournal.NewStore(sessions, c.clk)
	if err == nil {
		return "set the [ops] gates to auto or require to record ops here"
	}
	if reason := legacyop.DegradeReason(err); legacyop.CannotRecord(reason) {
		return fmt.Sprintf("the store cannot record ops (%s), so an explicit [ops] gate would not record here either", reason)
	}
	return "an [ops] gate set to auto or require records here if the store can record ops"
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
		return "use a sessions store with conditional writes (a native store; no released bd supports --if-revision yet)" + orOff
	case legacyop.DegradeNoNamespace:
		return "configure an id prefix for the sessions store" + orOff
	default:
		return "check that the sessions store is reachable"
	}
}
