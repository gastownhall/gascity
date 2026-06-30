//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

func TestSupervisorManagedProviderStartupCrashSmoke(t *testing.T) {
	tmuxtest.RequireTmux(t)

	const startupCrashTimeout = 10 * time.Second

	guard := tmuxtest.NewGuard(t)
	workDir := t.TempDir()
	crashScript := filepath.Join(workDir, "codex-like-startup-crash.sh")
	if err := os.WriteFile(crashScript, []byte(strings.Join([]string{
		"#!/bin/sh",
		"printf '%s\\n' 'WARNING: proceeding, even though we could not update PATH: Operation not permitted (os error 1)' >&2",
		"printf '%s\\n' 'Error: Operation not permitted (os error 1)' >&2",
		"sleep 1",
		"exit 1",
		"",
	}, "\n")), 0o755); err != nil {
		t.Fatalf("write crash script: %v", err)
	}

	sp, err := newSessionProviderByName("", config.SessionConfig{
		Socket:             guard.SocketName(),
		SetupTimeout:       startupCrashTimeout.String(),
		NudgeReadyTimeout:  "500ms",
		NudgeRetryInterval: "25ms",
		NudgeLockTimeout:   "500ms",
	}, guard.CityName(), workDir)
	if err != nil {
		t.Fatalf("new tmux provider: %v", err)
	}

	store := beads.NewMemStore()
	sessionName := guard.SessionName("mayor")
	sessionBead, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"agent_name":                 "mayor",
			"template":                   "mayor",
			"state":                      "asleep",
			"wake_attempts":              "4",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	prepared := preparedStart{
		candidate: startCandidate{
			session: &sessionBead,
			tp: TemplateParams{
				Command:                 crashScript,
				SessionName:             sessionName,
				TemplateName:            "mayor",
				ConfiguredNamedIdentity: "mayor",
				ConfiguredNamedMode:     "always",
			},
		},
		cfg: runtime.Config{
			Command:           crashScript,
			WorkDir:           workDir,
			ReadyPromptPrefix: "never-ready>",
			ReadyDelayMs:      10,
			ProviderName:      "codex",
		},
	}

	results := executePreparedStartWave(context.Background(), []preparedStart{prepared}, sp, store, startupCrashTimeout)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	result := results[0]
	if result.err == nil {
		t.Fatal("startup result err = nil, want provider startup failure")
	}
	if !strings.Contains(result.err.Error(), "Operation not permitted (os error 1)") {
		t.Fatalf("startup error missing provider output:\n%v", result.err)
	}

	var stdout, stderr bytes.Buffer
	clk := &clock.Fake{Time: time.Now().UTC()}
	if commitStartResult(result, store, clk, events.Discard, 0, &stdout, &stderr) {
		t.Fatalf("commitStartResult reported wake success; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("reload session bead: %v", err)
	}
	if got := updated.Metadata["quarantined_until"]; got == "" {
		t.Fatalf("quarantined_until empty after startup crash; metadata=%v", updated.Metadata)
	}
	if got := updated.Metadata["sleep_reason"]; got != "quarantine" {
		t.Fatalf("sleep_reason = %q, want quarantine", got)
	}

	info := sessionpkg.Info{
		ID:          updated.ID,
		Template:    "mayor",
		State:       sessionpkg.StateAsleep,
		SessionName: sessionName,
	}
	reason := sessionReason(info, map[string]beads.Bead{updated.ID: updated}, nil, sp, nil, nil)
	if reason != "startup-failure" {
		t.Fatalf("sessionReason = %q, want startup-failure; metadata=%v", reason, updated.Metadata)
	}

	attachStart := time.Now()
	attachErr := sp.Attach(sessionName)
	if attachErr == nil {
		t.Fatal("Attach succeeded, want dead startup pane refusal")
	}
	if time.Since(attachStart) > 750*time.Millisecond {
		t.Fatalf("Attach took %s, want fast refusal; err=%v", time.Since(attachStart), attachErr)
	}
	if !errors.Is(attachErr, runtime.ErrSessionNotFound) &&
		!strings.Contains(attachErr.Error(), "not running") &&
		!strings.Contains(attachErr.Error(), "session not found") {
		t.Fatalf("Attach error = %v, want missing/dead session context", attachErr)
	}
}
