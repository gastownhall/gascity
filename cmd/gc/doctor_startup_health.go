package main

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

// startupHealthEpisodesCheck surfaces active startup-health episodes (a
// session whose provider start keeps failing) so an operator sees a session
// stuck at or past its wake-failure threshold without needing to know the
// startup_health_* metadata keys or query the session-class store by hand.
type startupHealthEpisodesCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newStartupHealthEpisodesCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *startupHealthEpisodesCheck {
	return &startupHealthEpisodesCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *startupHealthEpisodesCheck) Name() string { return "startup-health-episodes" }

// CanFix returns false: an operator (or a dedicated recovery order), not this
// check, decides whether to wake, reset, or leave a quarantined session alone.
func (c *startupHealthEpisodesCheck) CanFix() bool { return false }

// Fix is a no-op. Detection only.
func (c *startupHealthEpisodesCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run classifies every persisted startup-health episode against the same
// defaultMaxWakeAttempts threshold session_lifecycle_parallel.go quarantines
// on, so an operator sees exactly the sessions a provider-start keeps
// refusing without needing to know the startup_health_* metadata keys. A
// malformed episode (no session name recorded) is reported on its own since
// it cannot be attributed to a session. ep.LastDetail is deliberately never
// copied into the result: provider error text can carry secrets (tokens,
// credentials) and must stay out of Message/Details/FixHint.
func (c *startupHealthEpisodesCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	store, err := c.newStore(c.cityPath)
	if err != nil {
		return warnCheck(c.Name(),
			fmt.Sprintf("skipping startup-health episode scan: opening bead store: %v", err),
			"fix bead store access, then rerun gc doctor",
			nil)
	}
	episodes, err := session.NewStore(beads.SessionStore{Store: store}).ListStartupHealthEpisodes()
	if err != nil {
		return warnCheck(c.Name(),
			fmt.Sprintf("skipping startup-health episode scan: listing episodes: %v", err),
			"fix bead store access, then rerun gc doctor",
			nil)
	}

	var details []string
	for _, ep := range episodes {
		if ep.SessionName == "" {
			details = append(details, fmt.Sprintf("malformed startup-health episode: missing session name (consecutive=%d)", ep.ConsecutiveCount))
			continue
		}
		if ep.ConsecutiveCount >= defaultMaxWakeAttempts {
			details = append(details, fmt.Sprintf("%s: %d consecutive startup_death failures", ep.SessionName, ep.ConsecutiveCount))
		}
	}
	if len(details) == 0 {
		return okCheck(c.Name(), "no active startup-health episodes")
	}
	sort.Strings(details)
	return errorCheck(c.Name(),
		fmt.Sprintf("%d startup-health episode issue(s) found", len(details)),
		"investigate the affected session's provider start failures; a malformed episode's bead metadata needs manual repair. The episode clears automatically on the session's next successful start.",
		details)
}
