package doctor

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// ProgramCaptureSession is one running session's OSC 7501 program-status
// capture as gc doctor reads it: whether config declares capture for it, and
// the runtime's judged reading of its agent pane.
type ProgramCaptureSession struct {
	Name     string
	Declared bool                  // the session's provider declares capture under the city switch
	Reading  runtime.ProgramStatus // the runtime's reading; zero when ReadErr is set
	ReadErr  error                 // the reading failed
}

// ProgramCaptureState is what [ProgramCaptureCheck] judges.
type ProgramCaptureState struct {
	// Mode is the city's [session] program_status switch.
	Mode runtime.ProgramStatusMode
	// FloorChecked is true when a runtime that captures program status
	// checked its capability floor; Floor is then its verdict, nil when
	// capture can arm. A Floor wrapping [runtime.ErrRuntimeUnavailable]
	// could not be checked (no server running).
	FloorChecked bool
	Floor        error
	// Sessions are the running sessions whose runtime reads program status.
	Sessions []ProgramCaptureSession
}

// ProgramCaptureCheck reports program-status capture that is not doing its
// job: a capability floor that keeps anything from arming (companion, tmux
// version, socket path), declared sessions not armed, whose reader was lost,
// that stay silent, or whose program probed for support and got no answer,
// armed sessions no longer declared, and valid readings whose tap liveness
// went unchecked because the process-table snapshot was skipped or degraded.
// The controller heals capture on its own; this check only reports.
type ProgramCaptureCheck struct {
	read func() (ProgramCaptureState, error)
}

// NewProgramCaptureCheck returns a check judging the state read returns.
func NewProgramCaptureCheck(read func() (ProgramCaptureState, error)) *ProgramCaptureCheck {
	return &ProgramCaptureCheck{read: read}
}

// Name returns the check identifier.
func (c *ProgramCaptureCheck) Name() string { return "program-status-capture" }

// CanFix returns false: the controller's heal re-arms and disarms capture.
func (c *ProgramCaptureCheck) CanFix() bool { return false }

// Fix is a no-op; see CanFix.
func (c *ProgramCaptureCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false: the check reads every running session.
func (c *ProgramCaptureCheck) WarmupEligible() bool { return false }

// Run judges the capture state. Every finding is advisory.
func (c *ProgramCaptureCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	st, err := c.read()
	if err != nil {
		r.Status, r.Severity = StatusWarning, SeverityAdvisory
		r.Message = fmt.Sprintf("cannot read program-status capture: %v", err)
		return r
	}
	if st.Mode != runtime.ProgramStatusShadow && st.Mode != runtime.ProgramStatusOn {
		r.Status = StatusOK
		r.Message = "program-status capture is off ([session] program_status)"
		return r
	}

	var problems, notes []string
	switch {
	case !st.FloorChecked:
	case errors.Is(st.Floor, runtime.ErrRuntimeUnavailable):
		notes = append(notes, fmt.Sprintf("capability floor not checked: %v", st.Floor))
	case st.Floor != nil:
		problems = append(problems, fmt.Sprintf("capability floor failed, nothing is armed: %v", st.Floor))
		if errors.Is(st.Floor, programstatus.ErrCompanionNotFound) || errors.Is(st.Floor, programstatus.ErrCompanionCodec) {
			r.FixHint = "install the " + programstatus.CompanionName + " companion built with this gc next to the gc binary or on PATH (make install installs both)"
		} else {
			r.FixHint = "run the city on tmux 3.2 or later with a socket path of only [A-Za-z0-9._/-]"
		}
	}

	sessions := append([]ProgramCaptureSession(nil), st.Sessions...)
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Name < sessions[j].Name })
	capturing, unchecked := 0, 0
	for _, s := range sessions {
		if s.ReadErr != nil {
			problems = append(problems, fmt.Sprintf("session %s: reading program status: %v", s.Name, s.ReadErr))
			continue
		}
		reading := s.Reading
		if !s.Declared {
			switch reading.Validity {
			case runtime.ProgramStatusValid, runtime.ProgramStatusSilent, runtime.ProgramStatusCleared, runtime.ProgramStatusReaderLost:
				problems = append(problems, fmt.Sprintf("session %s: armed but no longer declared (the controller disarms it)", s.Name))
			}
			continue
		}
		switch reading.Validity {
		case runtime.ProgramStatusNotArmed:
			problems = append(problems, fmt.Sprintf("session %s: declared but not armed (the controller arms it at its next inventory pass)", s.Name))
		case runtime.ProgramStatusReaderLost:
			problems = append(problems, fmt.Sprintf("session %s: capture reader lost: its pipe closed or its tap is gone (the controller re-arms it once the pipe is closed)", s.Name))
		case runtime.ProgramStatusSilent:
			if reading.Probed {
				problems = append(problems, fmt.Sprintf("session %s: probed but unanswered: the program sent the OSC 7501 support probe and nothing answered it, so it reports nothing (declare relay = true in the provider's program_status)", s.Name))
			} else {
				problems = append(problems, fmt.Sprintf("session %s: declared but silent: armed, with no report this incarnation (a program that does not report OSC 7501, or one still before its first prompt)", s.Name))
			}
		case runtime.ProgramStatusValid:
			capturing++
			if !reading.ReaderVerified {
				unchecked++
				notes = append(notes, fmt.Sprintf("session %s: tap liveness unchecked: the process-table snapshot was skipped or degraded", s.Name))
			}
		case runtime.ProgramStatusCleared:
			capturing++
		}
	}

	r.Details = slices.Concat(problems, notes)
	if len(problems) == 0 {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%d declared session(s) reporting program status", capturing)
		if unchecked > 0 {
			r.Message += fmt.Sprintf("; %d with tap liveness unchecked", unchecked)
		}
		return r
	}
	r.Status, r.Severity = StatusWarning, SeverityAdvisory
	r.Message = fmt.Sprintf("%d program-status capture finding(s)", len(problems))
	return r
}
