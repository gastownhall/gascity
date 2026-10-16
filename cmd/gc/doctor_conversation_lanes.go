package main

import (
	"encoding/json"
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
// it last passed, and the op rows it quarantined. It opens no store with the
// list empty. It is never fixable.
type conversationLanesDoctorCheck struct {
	cfg *config.City
	// open opens the city store; the check closes it after.
	open func() (beads.Store, error)
	// sessions routes the city store to its sessions class.
	sessions func(beads.Store) beads.Store
	// query asks the city's controller for its lane host's status.
	query func(cityPath string) (conversationLaneStatus, error)
}

func newConversationLanesDoctorCheck(cityPath string, cfg *config.City) *conversationLanesDoctorCheck {
	return &conversationLanesDoctorCheck{
		cfg:      cfg,
		open:     func() (beads.Store, error) { return openCityStoreAtWithConfig(cityPath, cfg) },
		sessions: func(store beads.Store) beads.Store { return cliSessionStore(store, cfg, cityPath) },
		query:    queryConversationLaneStatus,
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

// Run implements doctor.Check: OK with the list empty; an Error when a kind
// is listed and the sessions store cannot apply guarded batches, or the
// controller refused to start the host; a Warning when the controller does
// not answer, runs no lane host, runs another list than city.toml's, or has
// quarantined op rows; else OK with the last pass's age.
func (c *conversationLanesDoctorCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	var kinds []string
	if c.cfg != nil {
		kinds = c.cfg.Ops.Authoritative
	}
	if len(kinds) == 0 {
		r.Message = "conversation lanes are off"
		return r
	}
	listed := strings.Join(kinds, ", ")
	store, err := c.open()
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("conversation lanes on (%s); opening the city store failed: %v", listed, err)
		return r
	}
	capErr := laneStoreCapability(c.sessions(store))
	_ = closeBeadStoreHandle(store)
	if capErr != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("conversation lanes on (%s): %v", listed, capErr)
		r.FixHint = "remove the kind from [ops] authoritative, or bind the sessions class to a native store"
		return r
	}
	var st conversationLaneStatus
	if ctx != nil && ctx.CityPath != "" && c.query != nil {
		st, err = c.query(ctx.CityPath)
	} else {
		err = fmt.Errorf("no city to ask")
	}
	switch {
	case err != nil:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("conversation lanes on (%s): no lane-status reply (controller not answering, or a gc that predates the lane host)", listed)
		r.Details = append(r.Details, err.Error())
		return r
	case st.State == laneHostStateNone || st.State == "":
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("conversation lanes on (%s): the controller runs no lane host for this city", listed)
		r.FixHint = "run the city under a standalone controller (`gc start`); a supervisor-hosted city does not start the lane host yet"
		return r
	case st.State == laneHostStateRefused:
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("conversation lanes on (%s): the controller refused to start the lane host: %s", listed, st.Reason)
		r.FixHint = "remove the kind from [ops] authoritative, or bind the sessions class to a native store, then restart the controller"
		return r
	}
	var warnings []string
	if !slices.Equal(st.Kinds, kinds) {
		warnings = append(warnings, fmt.Sprintf("the controller runs [ops] authoritative = [%s] until its next start", strings.Join(st.Kinds, ", ")))
	}
	if st.Quarantined > 0 {
		warnings = append(warnings, fmt.Sprintf("%d op row(s) this gc cannot decode are quarantined, which a newer gc may have written", st.Quarantined))
		r.Details = append(r.Details, st.QuarantinedIDs...)
	}
	if st.LastError != "" {
		r.Details = append(r.Details, "last error: "+st.LastError)
	}
	pass := "no pass yet"
	if st.LastPassAt != "" {
		pass = "last pass " + (time.Duration(st.LastPassAgeMS) * time.Millisecond).Round(time.Second).String() + " ago"
	}
	if len(warnings) > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = fmt.Sprintf("conversation lanes on (%s), %s: %s", listed, pass, strings.Join(warnings, "; "))
		r.FixHint = "upgrade this gc if a newer gc wrote the rows it cannot decode; restart the controller to apply a changed list"
		return r
	}
	r.Message = fmt.Sprintf("conversation lanes on (%s): %s, %d conversation(s) in service", listed, pass, st.Workers)
	return r
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
