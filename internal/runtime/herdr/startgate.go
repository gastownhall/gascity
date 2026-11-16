package herdr

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// startCommandRefusal reports a command in cfg that the platform refuses to
// run (runtime.ShellCommandUnavailable) and whose refusal fails Start: the raw
// session command, or a pre_start command. Each is refused again where it
// runs; Start checks first so a refusal changes nothing. Found later, it left
// pre_start run, a pane placed and the session's metadata and binding
// published, which the session manager reads as a started session, and each
// retry ran pre_start again. session_setup is not checked: its failures are
// warnings that leave the session running.
func startCommandRefusal(cfg runtime.Config) error {
	if spec := launchSpecFor(cfg.Command); spec.Raw != "" {
		if reason := runtime.ShellCommandUnavailable(spec.Raw); reason != "" {
			return fmt.Errorf("raw command refused: %s", reason)
		}
	}
	for i, cmd := range cfg.PreStart {
		if reason := runtime.ShellCommandUnavailable(cmd); reason != "" {
			return fmt.Errorf("pre_start[%d] refused: %s", i, reason)
		}
	}
	return nil
}
