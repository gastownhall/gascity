package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Status and Diagnostics / Status

func TestPhase0StatusText_DoesNotExposePoolOntology(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
	}
	sp := runtime.NewFake()
	dops := newDrainOps(sp)

	var stdout bytes.Buffer
	if code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doCityStatus() = %d, want 0", code)
	}
	if strings.Contains(strings.ToLower(stdout.String()), "pool") {
		t.Fatalf("status output should not classify configs as pool/non-pool:\n%s", stdout.String())
	}
}

func TestPhase0StatusJSON_DoesNotEmitPoolField(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
	}
	sp := runtime.NewFake()

	var stdout bytes.Buffer
	if code := doCityStatusJSON(sp, cfg, t.TempDir(), &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doCityStatusJSON() = %d, want 0", code)
	}
	if strings.Contains(stdout.String(), `"pool"`) {
		t.Fatalf("status json should not expose pool field:\n%s", stdout.String())
	}
}

func TestPhase0StatusText_ShowsReservedUnmaterializedNamedIdentity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "refinery",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
			Mode:     "on_demand",
		}},
	}
	sp := runtime.NewFake()
	dops := newDrainOps(sp)

	var stdout bytes.Buffer
	if code := doCityStatus(sp, dops, cfg, t.TempDir(), &stdout, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doCityStatus() = %d, want 0", code)
	}
	out := strings.ToLower(stdout.String())
	if !strings.Contains(out, "reserved-unmaterialized") && !strings.Contains(out, "unmaterialized named") {
		t.Fatalf("status output should show reserved/unmaterialized named identities:\n%s", stdout.String())
	}
}
