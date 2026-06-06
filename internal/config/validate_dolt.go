package config

import "fmt"

// ValidateDoltConfig rejects Dolt config values that would otherwise be
// silently ignored or normalized at runtime.
func ValidateDoltConfig(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
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
	return nil
}
