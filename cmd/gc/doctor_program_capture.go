package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// programCaptureFloorChecker is a leaf backend that checks the OSC 7501
// capture capability floor without starting anything: tmux's
// ProgramCaptureFloor.
type programCaptureFloorChecker interface {
	ProgramCaptureFloor() error
}

// newProgramCaptureDoctorCheck is gc doctor's program-status capture check
// (doctor.ProgramCaptureCheck) over the city's session provider: the
// capability floor of every backend that captures, and the reading of every
// running session whose backend reads program status, beside the capture
// its config declares (from the session beads loadSessions reads).
func newProgramCaptureDoctorCheck(cfg *config.City, loadSessions func() (*sessionBeadSnapshot, error), sp runtime.Provider, spErr error) *doctor.ProgramCaptureCheck {
	return doctor.NewProgramCaptureCheck(func() (doctor.ProgramCaptureState, error) {
		return readProgramCaptureState(cfg, loadSessions, sp, spErr)
	})
}

// readProgramCaptureState reads what newProgramCaptureDoctorCheck judges.
func readProgramCaptureState(cfg *config.City, loadSessions func() (*sessionBeadSnapshot, error), sp runtime.Provider, spErr error) (doctor.ProgramCaptureState, error) {
	st := doctor.ProgramCaptureState{Mode: runtime.ProgramStatusOff}
	if cfg == nil {
		return st, nil
	}
	st.Mode = cfg.Session.ProgramStatusMode()
	if st.Mode == runtime.ProgramStatusOff {
		return st, nil
	}
	if sp == nil {
		return st, fmt.Errorf("session provider: %w", spErr)
	}
	for _, leaf := range inventoryLeaves(sp, "") {
		f, ok := leaf.provider.(programCaptureFloorChecker)
		if !ok {
			continue
		}
		st.FloorChecked = true
		if err := f.ProgramCaptureFloor(); err != nil && st.Floor == nil {
			st.Floor = err
		}
	}
	names, err := sp.ListRunning("")
	if err != nil && !runtime.IsPartialListError(err) {
		return st, fmt.Errorf("listing running sessions: %w", err)
	}
	snapshot, err := loadSessions()
	if err != nil {
		return st, fmt.Errorf("loading session beads: %w", err)
	}
	for _, name := range names {
		info, ok := snapshot.FindInfoBySessionName(name)
		if !ok {
			continue
		}
		ps, ok := runtime.ProgramStatusFor(sp, name)
		if !ok {
			continue
		}
		reading, readErr := ps.ProgramStatus(name)
		st.Sessions = append(st.Sessions, doctor.ProgramCaptureSession{
			Name:     name,
			Declared: programCaptureWant(cfg, info).Capture,
			Reading:  reading,
			ReadErr:  readErr,
		})
	}
	return st, nil
}
