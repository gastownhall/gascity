package config

import (
	"fmt"
	"regexp"
)

// ValidateStuckPatterns returns a non-nil error if any entry in
// cfg.Daemon.StuckErrorPatterns fails regexp.Compile. The error is indexed
// ("stuck_error_patterns[<i>]: <compile err>") so operators can locate the
// offending pattern in city.toml.
//
// This is a hard-error pre-flight check invoked on the same path as
// ValidateAgents: an invalid regex must cause `gc start` to exit non-zero
// rather than silently disabling the feature (AC10). The in-process
// newStuckTracker path remains as defense-in-depth.
func ValidateStuckPatterns(cfg *City) error {
	if cfg == nil {
		return nil
	}
	for i, src := range cfg.Daemon.StuckErrorPatterns {
		if _, err := regexp.Compile(src); err != nil {
			return fmt.Errorf("stuck_error_patterns[%d]: %w", i, err)
		}
	}
	return nil
}
