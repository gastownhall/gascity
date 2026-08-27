// Package nudgeshadow resolves the nudge-shadow configuration selection.
package nudgeshadow

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ScopeQueuedExactDueTargetSelection is the only supported nudge-shadow scope.
const ScopeQueuedExactDueTargetSelection = "queued_exact_due_target_selection"

// ErrTraceRecordingUnavailable means required shadow observations cannot be
// recorded by the current controller.
var ErrTraceRecordingUnavailable = errors.New("nudge shadow trace recording is unavailable")

// Mode is the resolved nudge-shadow mode.
type Mode string

const (
	// Off preserves the existing behavior without requiring nudge shadowing.
	Off Mode = "off"
	// Required requires the nudge-shadow path.
	Required Mode = "required"
)

// Provenance identifies whether a selection was built in or configured.
type Provenance string

const (
	// Builtin identifies the default selection used when nudge_shadow is omitted.
	Builtin Provenance = "builtin"
	// Config identifies a selection explicitly set in city configuration.
	Config Provenance = "config"
)

// Selection is the resolved nudge-shadow mode and its provenance.
type Selection struct {
	Mode       Mode
	Provenance Provenance
}

// Requirements describes runtime capabilities needed by required mode.
type Requirements struct {
	CityPath       string
	TraceRecording bool
}

// Required reports whether the selection requires nudge shadowing.
func (s Selection) Required() bool {
	return s.Mode == Required
}

// Validate checks that city and requirements can continue running this
// boot-latched selection. A configured mode change requires a controller
// restart; unchanged required mode must still satisfy its runtime requirements.
func (s Selection) Validate(city *config.City, requirements Requirements) error {
	configured, err := Resolve(city)
	if err != nil {
		return err
	}
	bootMode := s.Mode
	if bootMode == "" {
		bootMode = Off
	}
	if configured.Mode != bootMode {
		return fmt.Errorf(
			"nudge shadow mode changed from %q to %q; controller restart required",
			bootMode,
			configured.Mode,
		)
	}
	if bootMode != Required {
		return nil
	}
	if city.Daemon.NudgeDispatcherMode() != "supervisor" {
		return fmt.Errorf(
			"requiring nudge shadow scope %q: nudge dispatcher must be supervisor",
			ScopeQueuedExactDueTargetSelection,
		)
	}
	if city.Daemon.SessionReconcilerMode() != "off" {
		return fmt.Errorf(
			"requiring nudge shadow scope %q: session reconciler must be off",
			ScopeQueuedExactDueTargetSelection,
		)
	}
	if strings.TrimSpace(requirements.CityPath) == "" {
		return fmt.Errorf(
			"requiring nudge shadow scope %q: city path is blank",
			ScopeQueuedExactDueTargetSelection,
		)
	}
	if !requirements.TraceRecording {
		return fmt.Errorf(
			"requiring nudge shadow scope %q: %w",
			ScopeQueuedExactDueTargetSelection,
			ErrTraceRecordingUnavailable,
		)
	}
	return nil
}

// Resolve resolves the explicit nudge-shadow selection from city configuration.
func Resolve(city *config.City) (Selection, error) {
	if city == nil {
		return Selection{}, fmt.Errorf("resolving nudge shadow: nil city config")
	}

	if city.Daemon.NudgeShadow == nil {
		return Selection{Mode: Off, Provenance: Builtin}, nil
	}

	value := *city.Daemon.NudgeShadow
	switch value {
	case string(Off):
		return Selection{Mode: Off, Provenance: Config}, nil
	case string(Required):
		return Selection{Mode: Required, Provenance: Config}, nil
	default:
		return Selection{}, fmt.Errorf("resolving nudge shadow: invalid nudge_shadow %q (want off or required)", value)
	}
}

// Preflight resolves the configured selection and validates the runtime
// capabilities required by the queued exact due-target selection shadow.
// Off selections return without consulting any runtime requirement.
func Preflight(city *config.City, requirements Requirements) (Selection, error) {
	selection, err := Resolve(city)
	if err != nil {
		return Selection{}, err
	}
	if err := selection.Validate(city, requirements); err != nil {
		return Selection{}, err
	}
	return selection, nil
}
