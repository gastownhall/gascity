package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// resolveManagedDoltUnresponsiveRestartAfter returns the city's
// [daemon].dolt_unresponsive_restart_after window. A city without city.toml
// uses the default; an existing config that cannot be loaded, or a malformed
// or negative setting, is an error so recovery never picks a stop policy the
// operator did not configure.
func resolveManagedDoltUnresponsiveRestartAfter(cityPath string) (time.Duration, error) {
	if strings.TrimSpace(cityPath) == "" {
		return config.DefaultDoltUnresponsiveRestartAfter, nil
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.DefaultDoltUnresponsiveRestartAfter, nil
		}
		return 0, fmt.Errorf("resolve dolt_unresponsive_restart_after: %w", err)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return 0, fmt.Errorf("resolve dolt_unresponsive_restart_after: load city config: %w", err)
	}
	window, err := cfg.Daemon.DoltUnresponsiveRestartAfterDuration()
	if err != nil {
		return 0, fmt.Errorf("resolve dolt_unresponsive_restart_after: %w", err)
	}
	return window, nil
}
