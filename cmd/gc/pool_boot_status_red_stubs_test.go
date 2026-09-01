package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
)

// statusDisplayTextWithPoolBoot is the RED-phase adapter for ga-r1l8y2.1.1.
// It deliberately delegates to the legacy phase-only formatter so the new
// assertions fail on missing structured rendering rather than an undefined
// symbol. GREEN removes this adapter and provides the production function.
func statusDisplayTextWithPoolBoot(city api.CityInfo) string {
	return statusDisplayText(city.Status)
}

// runPoolOnBootWithProgress is the RED-phase adapter for ga-r1l8y2.1.1. It
// runs today's hooks while dropping the completion callback, reproducing the
// missing behavior directly. GREEN removes this adapter and adds the callback
// to production without changing runPoolOnBoot's existing callers.
func runPoolOnBootWithProgress(
	cfg *config.City,
	cityPath string,
	runner ScaleCheckRunner,
	stderr io.Writer,
	_ func(done, total int, agent string),
) {
	runPoolOnBoot(cfg, cityPath, runner, stderr)
}
