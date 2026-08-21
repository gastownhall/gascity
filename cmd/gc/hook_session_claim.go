package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/session"
)

// sessionCurrentClaimFrontDoor opens the routed session-class write front door
// for the city this process is running in. It is the shared root for the two
// halves of the claim back-channel: `gc hook --claim` stamps the claimed bead id
// onto the calling session's bead through it, and `gc hook current` reads that
// stamp back through it.
//
// It routes through cliSessionFrontDoor so a [beads.classes.sessions] relocation
// reaches both halves — a raw work-store front door would stamp the claim onto
// the work store while the real session bead lives in the relocated store, and
// `gc hook current` would then read back nothing forever. The no-refresh config
// loader matches the other hook-path roots (cmd_prime.go's
// persistPrimeHookProviderSessionKey): this runs on every claim, and a nil cfg
// leaves cliSessionStore identity to the input store.
func sessionCurrentClaimFrontDoor() (*session.Store, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil, err
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	return cliSessionFrontDoor(store, cfg, cityPath), nil
}

// hookStampSessionCurrentClaim records beadID as the work bead the session
// identified by sessionID is currently running. It is the production
// implementation of the hookClaimOps.StampSessionClaim seam.
//
// The write goes through session.Store.SetCurrentClaim, which resolves the id
// EXACTLY and refuses a non-session bead before writing anything: bd's fuzzy id
// resolver would otherwise let a post-claim update land on a prefix-colliding
// session if the intended one disappeared concurrently, which is why the claim
// path decorates the session bead only through this guarded seam (see
// publishHookClaimRunMap, which stays a file-based sidecar for exactly that
// reason). SetCurrentClaim also compare-and-skips, so the per-tick adoption
// re-run issues no write once the value is current.
func hookStampSessionCurrentClaim(sessionID, beadID string) error {
	sessFront, err := sessionCurrentClaimFrontDoor()
	if err != nil {
		return err
	}
	_, err = sessFront.SetCurrentClaim(sessionID, beadID)
	return err
}
