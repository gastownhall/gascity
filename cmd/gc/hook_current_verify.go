package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// hookCurrentContext is what `gc hook current` verifies a claim stamp
// against: the session front door the stamp is read through, the city config
// the session's identities resolve under, and the work legs the claimed bead
// is read from live by residency.
type hookCurrentContext struct {
	sessFront *session.Store
	cfg       *config.City
	legs      WorkLegs
}

// openHookCurrentContext opens the city the calling shell resolves for one
// `gc hook current` read. It reads only a real city (a directory holding
// city.toml): opening a store anywhere else provisions one. Every failure is
// returned, since a claim this command cannot verify is not one it may print.
func openHookCurrentContext() (hookCurrentContext, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return hookCurrentContext{}, err
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		return hookCurrentContext{}, fmt.Errorf("%s is not a city: %w", cityPath, err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return hookCurrentContext{}, fmt.Errorf("loading city config: %w", err)
	}
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return hookCurrentContext{}, fmt.Errorf("opening city store: %w", err)
	}
	rigs, _ := openStandaloneRigStores(cityRigs(cfg), cityPath, oneShotRigStoreOpener(cfg), sequentialRigStoreOpens)
	return hookCurrentContext{
		sessFront: cliSessionFrontDoor(store, cfg, cityPath),
		cfg:       cfg,
		legs:      workLegsFromCensus(cityPath, cfg, cityWorkLegOf(cliStores(cityPath, store).Work()), rigs),
	}, nil
}

// claimCheck reads claim id live from the work leg that owns it by residency
// (WorkLegs.readLive) and reports why sessionID does not currently hold it
// under scope ("" when it does). A read failure is an error, never a verdict.
func (c hookCurrentContext) claimCheck(id, sessionID string, scope ReleaseScope) (claimStaleReason, error) {
	work, found, err := c.legs.readLive(id)
	if err != nil {
		return "", fmt.Errorf("reading claim %s: %w", id, err)
	}
	if !found {
		return claimStaleGone, nil
	}
	return currentClaimStaleReason(work, sessionID, scope), nil
}

// resolveHookCurrent is the claim sessionID currently holds, as `gc hook
// current` prints it. The session's claim stamp is printed only when the live
// ledger says the session still holds it. It never writes: repairing the
// stamp is the claim path's job.
func resolveHookCurrent(c hookCurrentContext, sessionID string) (string, error) {
	info, err := c.sessFront.Get(sessionID)
	if err != nil {
		return "", err
	}
	stamp := strings.TrimSpace(info.CurrentClaimBeadID)
	if stamp == "" {
		return "", fmt.Errorf("session %s has no current claim (nothing claimed through gc hook --claim)", sessionID)
	}
	scope := releaseScope(info, c.cfg)
	reason, err := c.claimCheck(stamp, sessionID, scope)
	if err != nil {
		return "", err
	}
	if reason != "" {
		return "", fmt.Errorf("session %s has no current claim: its claim stamp %s is stale (%s)", sessionID, stamp, reason)
	}
	return stamp, nil
}
