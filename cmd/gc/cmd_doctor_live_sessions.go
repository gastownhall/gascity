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
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close of a read-only handle
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

// doctorSessionRegistry returns the session registry the agent-sessions and
// orphan-sessions checks read: each open session row's runtime session name,
// mapped to the configured agent it runs. A row whose agent or named session
// is no longer configured is left out: the controller drains that runtime as
// orphaned, so the row must not account for it. It lists the rows the way gc session list
// and the controller do (the type+label union) from the session
// coordination-class store, so a city that relocates its sessions class is
// read from that binding. Each call reads afresh, and a failed or partial read
// is returned as an error, so a check that cannot read the registry says so
// instead of calling every bead-named session orphaned.
func doctorSessionRegistry(cityPath string, cfg *config.City) doctor.SessionRegistry {
	return func() (map[string]string, error) {
		store, err := openSessionProviderStore(cityPath)
		if err != nil {
			return nil, err
		}
		defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close of a read-only handle
		infos, err := cliSessionFrontDoor(store, cfg, cityPath).ListAll(session.ListAllOptions{})
		if err != nil {
			return nil, err
		}
		names := make(map[string]string, len(infos))
		for _, info := range infos {
			if info.Closed || info.SessionName == "" {
				continue
			}
			template := resolvedSessionTemplateInfo(info, cfg)
			if findSessionAgentByTemplate(cfg, template) == nil || removedNamedSessionInfo(info, cfg) {
				continue
			}
			names[info.SessionName] = template
		}
		return names, nil
	}
}

// removedNamedSessionInfo reports whether info is a named-session row whose
// declaration is no longer configured, by the predicate the controller retires
// such rows with: its identity resolves to no named session and is not a bare
// leaf two configured named sessions still claim. Its template may still be
// configured, so the agent filter alone keeps the row.
func removedNamedSessionInfo(info session.Info, cfg *config.City) bool {
	if !isNamedSessionInfo(info) {
		return false
	}
	identity := namedSessionIdentityInfo(info)
	return identity != "" &&
		config.FindNamedSession(cfg, identity) == nil &&
		len(config.NamedSessionBareCandidates(cfg, identity)) < 2
}
