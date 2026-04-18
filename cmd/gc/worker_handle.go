package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func newWorkerSessionHandleWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, spec worker.SessionSpec) (*worker.SessionHandle, error) {
	return worker.NewSessionHandle(worker.SessionHandleConfig{
		Manager: newSessionManagerWithConfig(cityPath, store, sp, cfg),
		Session: spec,
	})
}

func workerHandleForSessionWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, id string) (*worker.SessionHandle, error) {
	mgr := newSessionManagerWithConfig(cityPath, store, sp, cfg)
	info, err := mgr.Get(id)
	if err != nil {
		return nil, err
	}

	spec := worker.SessionSpec{
		ID:       id,
		Provider: info.Provider,
		WorkDir:  info.WorkDir,
		Resume: session.ProviderResume{
			ResumeFlag:    info.ResumeFlag,
			ResumeStyle:   info.ResumeStyle,
			ResumeCommand: info.ResumeCommand,
		},
	}
	if store != nil {
		if bead, beadErr := store.Get(id); beadErr == nil {
			if profile := strings.TrimSpace(bead.Metadata["worker_profile"]); profile != "" {
				spec.Profile = worker.Profile(profile)
			}
		}
	}

	return worker.NewSessionHandle(worker.SessionHandleConfig{
		Manager: mgr,
		Session: spec,
	})
}
