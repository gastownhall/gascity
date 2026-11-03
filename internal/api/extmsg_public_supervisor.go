package api

import (
	"context"
	"time"
)

// maintainPublicRelays owns discovery independently of HTTP traffic. It uses
// the same cached Server as HTTP, hence the same named-session resolver/locks.
// Enumeration reads registered in-memory controller state, never probes bd.
func (sm *SupervisorMux) maintainPublicRelays(ctx context.Context) {
	if sm.readOnly || sm.resolver == nil {
		return
	}
	type running struct {
		server *Server
		cancel context.CancelFunc
		done   <-chan struct{}
	}
	active := make(map[string]running)
	stop := func(r running) {
		r.cancel()
		if r.done != nil {
			<-r.done
		}
	}
	defer func() {
		for _, r := range active {
			stop(r)
		}
	}()
	reconcile := func() {
		keep := make(map[string]bool)
		for _, city := range sm.resolver.ListCities() {
			if !city.Running {
				continue
			}
			state := sm.resolver.CityState(city.Name)
			if state == nil {
				continue
			}
			if _, ok := state.(ExtMsgPublicState); !ok {
				continue
			}
			lifecycle, ok := state.(ExtMsgPublicLifecycle)
			if !ok {
				continue
			}
			cityCtx := lifecycle.ExtMsgPublicContext()
			if cityCtx == nil || cityCtx.Done() == nil || cityCtx.Err() != nil {
				continue
			}
			server := sm.getCityServer(city.Name, state)
			keep[city.Name] = true
			if old, ok := active[city.Name]; ok {
				if old.server == server {
					continue
				}
				stop(old)
			}
			child, cancel := context.WithCancel(ctx)
			done := server.startExtMsgPublicRelay(child)
			active[city.Name] = running{server: server, cancel: cancel, done: done}
		}
		for name, r := range active {
			if !keep[name] {
				stop(r)
				delete(active, name)
			}
		}
	}
	reconcile()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}
