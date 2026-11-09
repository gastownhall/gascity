package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// TestWorkDirRefusalsAreMappedNotInternalErrors: a session create or resume
// the working-directory guard refused answers 409 for an occupied directory
// and a retryable 503 for an unverifiable one, in both error mappers, not the
// 500 "internal" the store-error fallback gives an unrecognized error.
func TestWorkDirRefusalsAreMappedNotInternalErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name:   "occupied directory",
			err:    fmt.Errorf("creating session: %w: /work is occupied by session gc-1", session.ErrWorkDirCollision),
			status: http.StatusConflict,
			code:   "workdir_collision",
		},
		{
			name:   "liveness unverifiable",
			err:    fmt.Errorf("resuming session: %w: /work", session.ErrWorkDirLivenessUnavailable),
			status: http.StatusServiceUnavailable,
			code:   "workdir_liveness_unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeSessionManagerError(rec, tt.err)
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.code) {
				t.Errorf("writeSessionManagerError = %d %s, want %d %s", rec.Code, rec.Body.String(), tt.status, tt.code)
			}
			var se huma.StatusError
			got := humaSessionManagerError(tt.err)
			if !errors.As(got, &se) || se.GetStatus() != tt.status || !strings.Contains(got.Error(), tt.code) {
				t.Errorf("humaSessionManagerError = %v, want %d %s", got, tt.status, tt.code)
			}
		})
	}
}

// TestSessionManagerRecordsWorkDirRefusalsToTheEventProvider: the manager the
// API builds carries the city's event provider as its recorder, so a refusal
// is recorded as session.start_refused_cwd rather than discarded. Both of
// sessionManager's constructions are covered: with and without a loaded config.
func TestSessionManagerRecordsWorkDirRefusalsToTheEventProvider(t *testing.T) {
	for _, withConfig := range []bool{true, false} {
		name := "config loaded"
		if !withConfig {
			name = "no config"
		}
		t.Run(name, func(t *testing.T) {
			fs := newFakeState(t)
			srv := New(fs)
			if !withConfig {
				fs.cfg = nil
			}
			mgr := srv.sessionManager(beadstest.Sessions(beads.NewMemStore()))

			dir := t.TempDir()
			create := func(title string) error {
				_, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "probe", Title: title, Command: "claude", WorkDir: dir, Provider: "claude"})
				return err
			}
			if err := create("first"); err != nil {
				t.Fatalf("first create: %v", err)
			}
			if err := create("second"); !errors.Is(err, session.ErrWorkDirCollision) {
				t.Fatalf("second create error = %v, want wrapping ErrWorkDirCollision", err)
			}
			got, err := fs.eventProv.List(events.Filter{Type: events.SessionStartRefusedCwd})
			if err != nil {
				t.Fatalf("listing events: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("recorded %d %s events, want 1: %+v", len(got), events.SessionStartRefusedCwd, got)
			}
		})
	}
}
