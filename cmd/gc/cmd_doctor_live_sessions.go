package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

// doctorLiveSessionInfos lists every live (non-closed) session from the
// session store. Doctor checks that scope themselves to live sessions share it
// so they agree on what live means.
func doctorLiveSessionInfos(cityPath string, cfg *config.City) ([]session.Info, error) {
	store, err := openSessionProviderStore(cityPath)
	if err != nil {
		return nil, err
	}
	infos, err := session.NewStore(beads.SessionStore{Store: cliSessionStore(store, cfg, cityPath)}).ListLabeledSessionInfosUnfiltered()
	if err != nil {
		return nil, err
	}
	live := infos[:0]
	for _, info := range infos {
		if !info.Closed {
			live = append(live, info)
		}
	}
	return live, nil
}

// doctorLiveSessionNames returns a lazy lookup of the tmux session name of
// every live session, for the nudge-unconfirmed check: the diagnostic files it
// scans live under a directory named after the session, and are never cleared
// when the session closes. Laziness keeps the session store out of doctor
// check construction; a store failure is returned so the check can say it
// could not filter instead of reporting a city with no live sessions.
func doctorLiveSessionNames(cityPath string, cfg *config.City) doctor.LiveSessionNamesFunc {
	return func() (map[string]struct{}, error) {
		infos, err := doctorLiveSessionInfos(cityPath, cfg)
		if err != nil {
			return nil, err
		}
		names := make(map[string]struct{}, len(infos))
		for _, info := range infos {
			if info.SessionName != "" {
				names[info.SessionName] = struct{}{}
			}
		}
		return names, nil
	}
}
