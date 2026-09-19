package config

import (
	"fmt"
	"strings"
)

// ValidateDoltConfig rejects Dolt config values that would otherwise be
// silently ignored or normalized at runtime.
func ValidateDoltConfig(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	mode := strings.TrimSpace(cfg.Dolt.Mode)
	if mode != "" && mode != "server" && mode != "proxied-server" {
		return fmt.Errorf("%s: [dolt] mode must be \"server\" or \"proxied-server\": got %q", source, cfg.Dolt.Mode)
	}
	// proxied-server may optionally front an externally managed Dolt server;
	// the bd adapter translates host/port into its --proxied-server-external-*
	// flags. An omitted endpoint remains the managed-local proxy shape.
	checkNonNegative := func(field string, value int) error {
		if value < 0 {
			return fmt.Errorf("%s: [dolt] %s must not be negative: got %d", source, field, value)
		}
		return nil
	}
	if err := checkNonNegative("max_connections", cfg.Dolt.MaxConnections); err != nil {
		return err
	}
	if err := checkNonNegative("read_timeout_millis", cfg.Dolt.ReadTimeoutMillis); err != nil {
		return err
	}
	if err := checkNonNegative("write_timeout_millis", cfg.Dolt.WriteTimeoutMillis); err != nil {
		return err
	}
	if err := checkNonNegative("wait_timeout_seconds", cfg.Dolt.WaitTimeoutSeconds); err != nil {
		return err
	}
	return nil
}
