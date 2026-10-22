package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// conversationLanesDoctorCheck reports the conversation lanes ([ops]
// authoritative): whether the sessions store can host them, and, from the
// running controller's lane-status, whether its lane host runs, how long ago
// it last passed, and the op rows it quarantined. With the list empty it
// opens no store, and asks the controller only whether it still serves the
// list it started with. It is never fixable.
type conversationLanesDoctorCheck struct {
	cfg *config.City
	// open opens the city store; the check closes it after.
	open func() (beads.Store, error)
	// sessions routes the city store to its sessions class.
	sessions func(beads.Store) beads.Store
	// query asks the city's controller for its lane host's status.
	query func(cityPath string) (conversationLaneStatus, error)
	// lookupEnv is the environment the reconciler mode latches with.
	lookupEnv func(string) (string, bool)
}

// laneMessageUnderV2Warning is the check's warning when message is listed
// under the v2 reconciler (ga-f35wnp, V2-18).
const laneMessageUnderV2Warning = `message is listed under session_reconciler = "v2": the content gate cannot see a stop v2 requests before its drain begins (V2-18), so content can start a turn in a session v2 is about to drain`

func newConversationLanesDoctorCheck(cityPath string, cfg *config.City) *conversationLanesDoctorCheck {
	return &conversationLanesDoctorCheck{
		cfg:       cfg,
		open:      func() (beads.Store, error) { return openCityStoreAtWithConfig(cityPath, cfg) },
		sessions:  func(store beads.Store) beads.Store { return cliSessionStore(store, cfg, cityPath) },
		query:     queryConversationLaneStatus,
		lookupEnv: reconcilerModeLookupEnv,
	}
}

// Name implements doctor.Check.
func (*conversationLanesDoctorCheck) Name() string { return "conversation-lanes" }

// CanFix implements doctor.Check.
func (*conversationLanesDoctorCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check: the host's state is the running
// controller's.
func (*conversationLanesDoctorCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*conversationLanesDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check: with the list empty, OK unless the controller
// still serves, or waits for its store to serve, the list it started with, a
// Warning; an Error when a kind is listed and the sessions store cannot apply
// guarded batches, the controller runs no lane host (a supervisor-hosted
// city), or it refused to start the host; a Warning when the controller does
// not answer, waits for its sessions store, runs another list than
// city.toml's, has quarantined op rows, or failed its last pass or anything
// since its last good one; else OK with the last pass's age. Whatever the
// state, message listed under the v2 reconciler adds an advisory warning
// (laneMessageUnderV2Warning).
func (c *conversationLanesDoctorCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := c.run(ctx)
	if !c.messageUnderV2() {
		return r
	}
	if r.Status == doctor.StatusOK {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.FixHint = "remove message from [ops] authoritative, or run the legacy reconciler"
	}
	r.Message += "; " + laneMessageUnderV2Warning
	return r
}

// messageUnderV2 reports whether [ops] authoritative lists message while the
// city latches the v2 reconciler.
func (c *conversationLanesDoctorCheck) messageUnderV2() bool {
	if !cliLaneExecutesMessage(c.cfg) {
		return false
	}
	lookup := c.lookupEnv
	if lookup == nil {
		lookup = reconcilerModeLookupEnv
	}
	mode, err := latchReconcilerMode(c.cfg, lookup)
	return err == nil && mode == reconcilerV2
}

func (c *conversationLanesDoctorCheck) run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	var kinds []string
	if c.cfg != nil {
		kinds = c.cfg.Ops.Authoritative
	}
	if len(kinds) == 0 {
		c.reportOff(ctx, r)
		return r
	}
	listed := strings.Join(kinds, ", ")
	if c.storeProblem(r, listed) {
		return r
	}
	st, err := c.ask(ctx)
	if laneHostProblem(r, listed, st, err) {
		return r
	}
	reportLaneService(r, listed, kinds, st)
	return r
}

// reportOff reports the list empty on r: OK, unless the controller still
// serves, or waits for its store to serve, the list it started with.
func (c *conversationLanesDoctorCheck) reportOff(ctx *doctor.CheckContext, r *doctor.CheckResult) {
	r.Message = "conversation lanes are off"
	// The controller reads the list once, at start, so it serves the list
	// it started with until it restarts. A controller that does not
	// answer, or refused its list, serves none.
	st, err := c.ask(ctx)
	if err == nil && len(st.Kinds) > 0 && (st.State == laneHostStateRunning || st.State == laneHostStateUnavailable) {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = fmt.Sprintf("conversation lanes are off in city.toml, but the controller runs [ops] authoritative = [%s] until its next start", strings.Join(st.Kinds, ", "))
		r.FixHint = "restart the controller to apply the emptied list"
	}
}

// storeProblem reports on r, and says whether, the sessions store cannot
// host the listed kinds: the city store does not open or has no sessions
// store, a Warning, or cannot apply guarded batches, an Error.
func (c *conversationLanesDoctorCheck) storeProblem(r *doctor.CheckResult, listed string) bool {
	store, err := c.open()
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("conversation lanes on (%s); opening the city store failed: %v", listed, err)
		return true
	}
	capErr := laneStoreCapability(c.sessions(store))
	_ = closeBeadStoreHandle(store)
	switch {
	case errors.Is(capErr, errLaneStoreMissing):
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("conversation lanes on (%s): the city store has no sessions store", listed)
	case capErr != nil:
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("conversation lanes on (%s): %v", listed, capErr)
		r.FixHint = "remove the kind from [ops] authoritative, or bind the sessions class to a native store"
	default:
		return false
	}
	return true
}

// laneHostProblem reports on r, and says whether, the controller's
// lane-status reply st (or its error) shows no lane host serving: no reply,
// or a host that waits for its sessions store, a Warning; no host, or one it
// refused to start, an Error.
func laneHostProblem(r *doctor.CheckResult, listed string, st conversationLaneStatus, err error) bool {
	switch {
	case err != nil:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("conversation lanes on (%s): no lane-status reply (controller not answering, or a gc that predates the lane host)", listed)
		r.Details = append(r.Details, err.Error())
	case st.State == laneHostStateNone || st.State == "":
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("conversation lanes on (%s): the controller runs no lane host for this city, so the listed kinds go unserved", listed)
		r.FixHint = "the running controller predates the lane host; restart the city (`gc stop`, then `gc start`), or empty [ops] authoritative"
	case st.State == laneHostStateUnavailable:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("conversation lanes on (%s): the lane host waits for the controller's sessions store: %s", listed, st.Reason)
		r.FixHint = "check the controller log for why the city store did not open"
	case st.State == laneHostStateRefused:
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("conversation lanes on (%s): the controller refused to start the lane host: %s", listed, st.Reason)
		r.FixHint = "remove the kind from [ops] authoritative, or bind the sessions class to a native store, then restart the controller"
	default:
		return false
	}
	return true
}

// reportLaneService reports a running lane host's service of the listed
// kinds on r: a Warning when it runs another list than city.toml's, has
// quarantined op rows, or failed its last pass or anything since its last
// good one; else OK with the last pass's age. Its quarantined rows, last
// error and waits are details.
func reportLaneService(r *doctor.CheckResult, listed string, kinds []string, st conversationLaneStatus) {
	var warnings []string
	if !slices.Equal(st.Kinds, kinds) {
		warnings = append(warnings, fmt.Sprintf("the controller runs [ops] authoritative = [%s] until its next start", strings.Join(st.Kinds, ", ")))
	}
	if st.Quarantined > 0 {
		warnings = append(warnings, fmt.Sprintf("%d op row(s) this gc cannot decode are quarantined: a newer gc wrote them, or they are not valid to this one", st.Quarantined))
		r.Details = append(r.Details, st.QuarantinedIDs...)
	}
	if st.LastError != "" {
		r.Details = append(r.Details, "last error: "+st.LastError)
	}
	for _, w := range st.Waiting {
		r.Details = append(r.Details, laneWaitingDetail(w))
	}
	pass, failure := lanePassSummary(st)
	if failure != "" {
		warnings = append(warnings, failure)
	}
	if len(warnings) > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = fmt.Sprintf("conversation lanes on (%s), %s: %s", listed, pass, strings.Join(warnings, "; "))
		r.FixHint = "see the details and the controller log; inspect the rows this gc cannot decode, and upgrade it if a newer gc wrote them; restart the controller to apply a changed list"
		return
	}
	r.Message = fmt.Sprintf("conversation lanes on (%s): %s, %d conversation(s) in service", listed, pass, st.Workers)
}

// lanePassSummary is the lane host's last pass as the check reports it, with
// the last success's age after a failure, and the warning its failures
// raise, "" for none.
func lanePassSummary(st conversationLaneStatus) (pass, failure string) {
	pass = "no pass yet"
	if st.LastPassAt != "" {
		pass = "last pass " + laneAge(st.LastPassAgeMS) + " ago"
	}
	switch {
	case st.LastPassFailed:
		failure = "the last pass failed"
	case st.ErrorsSinceSuccess > 0:
		failure = fmt.Sprintf("%d failure(s) since the last pass that succeeded", st.ErrorsSinceSuccess)
	}
	if failure != "" && st.LastSuccessAt != "" {
		pass += ", last success " + laneAge(st.LastSuccessAgeMS) + " ago"
	}
	return pass, failure
}

// ask is the lane-status reply of ctx's city's controller.
func (c *conversationLanesDoctorCheck) ask(ctx *doctor.CheckContext) (conversationLaneStatus, error) {
	if ctx == nil || ctx.CityPath == "" || c.query == nil {
		return conversationLaneStatus{}, errors.New("no city to ask")
	}
	return c.query(ctx.CityPath)
}

// laneAge renders a status age in milliseconds to the second.
func laneAge(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

// laneWaitingDetail is one conversation's waits as a detail line.
func laneWaitingDetail(w laneConversationWaits) string {
	parts := make([]string, 0, len(w.Waits))
	for _, op := range w.Waits {
		p := op.Op + " " + op.Reason
		if op.Detail != "" {
			p += " (" + op.Detail + ")"
		}
		parts = append(parts, p)
	}
	line := "conversation " + w.Conversation + " waits: " + strings.Join(parts, ", ")
	if w.Wake != "" {
		line += "; wake " + w.Wake
	}
	return line
}

// queryConversationLaneStatus asks the city's controller; one that does not
// answer, or predates the command, is an error.
func queryConversationLaneStatus(cityPath string) (conversationLaneStatus, error) {
	var st conversationLaneStatus
	resp, err := sendControllerCommandWithTimeouts(cityPath, laneStatusCommand, 500*time.Millisecond, 500*time.Millisecond, 2*time.Second)
	if err == nil {
		err = json.Unmarshal(resp, &st)
	}
	return st, err
}
