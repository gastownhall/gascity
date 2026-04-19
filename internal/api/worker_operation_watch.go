package api

import (
	"context"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

func (s *Server) watchSessionWorkerOperationSignals(ctx context.Context, info session.Info) <-chan struct{} {
	return s.watchWorkerOperationSignals(ctx, info.ID, info.SessionName)
}

func (s *Server) resolveAgentSessionSubjects(name string, cfg *config.City) (string, string) {
	if s == nil || s.state == nil || cfg == nil {
		return "", ""
	}

	sessionName := strings.TrimSpace(agentSessionName(s.state.CityName(), name, cfg.Workspace.SessionTemplate))
	if sessionName == "" {
		return "", ""
	}

	sessionID := ""
	if store := s.state.CityBeadStore(); store != nil {
		if id, err := s.resolveSessionIDWithConfig(store, sessionName); err == nil {
			sessionID = strings.TrimSpace(id)
		}
	}

	return sessionName, sessionID
}

func (s *Server) watchAgentWorkerOperationSignals(ctx context.Context, name string, cfg *config.City) <-chan struct{} {
	sessionName, sessionID := s.resolveAgentSessionSubjects(name, cfg)
	if sessionName == "" {
		return nil
	}
	return s.watchWorkerOperationSignals(ctx, sessionID, sessionName)
}

func (s *Server) watchWorkerOperationSignals(ctx context.Context, subjects ...string) <-chan struct{} {
	if s == nil || s.state == nil {
		return nil
	}
	ep := s.state.EventProvider()
	if ep == nil {
		return nil
	}

	watchSubjects := make(map[string]struct{}, len(subjects))
	for _, subject := range subjects {
		subject = strings.TrimSpace(subject)
		if subject == "" {
			continue
		}
		watchSubjects[subject] = struct{}{}
	}
	if len(watchSubjects) == 0 {
		return nil
	}

	afterSeq, err := ep.LatestSeq()
	if err != nil {
		return nil
	}
	watcher, err := ep.Watch(ctx, afterSeq)
	if err != nil {
		return nil
	}

	signals := make(chan struct{}, 1)
	go func() {
		defer close(signals)
		defer watcher.Close() //nolint:errcheck // best-effort cleanup
		for {
			event, err := watcher.Next()
			if err != nil {
				return
			}
			if !workerOperationEventMatchesSubjects(watchSubjects, event) {
				continue
			}
			select {
			case signals <- struct{}{}:
			default:
			}
		}
	}()
	return signals
}

func workerOperationEventMatchesSubjects(subjects map[string]struct{}, event events.Event) bool {
	if event.Type != events.WorkerOperation {
		return false
	}
	subject := strings.TrimSpace(event.Subject)
	if subject == "" {
		return false
	}
	_, ok := subjects[subject]
	return ok
}
