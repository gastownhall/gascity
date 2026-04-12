package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Surface matrix / session-targeting CLI
// - Materialization contract
// - template: scope is factory-targeting only
// - Ambient rig resolution

func TestPhase0CLISessionTargetingSurfaces_RejectTemplateFactoryTargets(t *testing.T) {
	tests := []struct {
		name string
		run  func(stdout, stderr *bytes.Buffer) int
	}{
		{
			name: "gc session attach",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionAttach([]string{"template:worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session wake",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionWake([]string{"template:worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session suspend",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionSuspend([]string{"template:worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session pin",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionPin([]string{"template:worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session unpin",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionUnpin([]string{"template:worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session close",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionClose([]string{"template:worker"}, stdout, stderr)
			},
		},
		{
			name: "gc mail send",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdMailSend([]string{"template:worker", "hello"}, false, false, "", "", "", "", stdout, stderr)
			},
		},
		{
			name: "gc session nudge",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionNudge([]string{"template:worker", "hello"}, nudgeDeliveryImmediate, stdout, stderr)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_DIR", t.TempDir())
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "fake")

			var stdout, stderr bytes.Buffer
			code := tt.run(&stdout, &stderr)
			if code == 0 {
				t.Fatalf("%s accepted template:worker; stdout=%s stderr=%s", tt.name, stdout.String(), stderr.String())
			}
			if count := phase0InterfaceSessionCount(t, cityDir); count != 0 {
				t.Fatalf("%s materialized %d session(s) for template:worker; stdout=%s stderr=%s", tt.name, count, stdout.String(), stderr.String())
			}
		})
	}
}

func TestPhase0CLISessionTargetingSurfaces_BareConfigNameDoesNotMaterializeOrdinarySession(t *testing.T) {
	tests := []struct {
		name string
		run  func(stdout, stderr *bytes.Buffer) int
	}{
		{
			name: "gc session attach",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionAttach([]string{"worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session wake",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionWake([]string{"worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session suspend",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionSuspend([]string{"worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session pin",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionPin([]string{"worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session unpin",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionUnpin([]string{"worker"}, stdout, stderr)
			},
		},
		{
			name: "gc session close",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionClose([]string{"worker"}, stdout, stderr)
			},
		},
		{
			name: "gc mail send",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdMailSend([]string{"worker", "hello"}, false, false, "", "", "", "", stdout, stderr)
			},
		},
		{
			name: "gc session nudge",
			run: func(stdout, stderr *bytes.Buffer) int {
				return cmdSessionNudge([]string{"worker", "hello"}, nudgeDeliveryImmediate, stdout, stderr)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_DIR", t.TempDir())
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "fake")

			var stdout, stderr bytes.Buffer
			code := tt.run(&stdout, &stderr)
			if code == 0 {
				t.Fatalf("%s accepted ordinary config name worker; stdout=%s stderr=%s", tt.name, stdout.String(), stderr.String())
			}
			if count := phase0InterfaceSessionCount(t, cityDir); count != 0 {
				t.Fatalf("%s materialized %d ordinary session(s) for config worker; stdout=%s stderr=%s", tt.name, count, stdout.String(), stderr.String())
			}
		})
	}
}

func TestPhase0CLISessionClose_AllowsAlwaysNamedSessionParityWithAPI(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1

[[named_session]]
template = "worker"
mode = "always"
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "test-city--worker",
			"alias":                     "worker",
			"template":                  "worker",
			"configured_named_session":  "true",
			"configured_named_identity": "worker",
			"configured_named_mode":     "always",
			"state":                     "suspended",
			"continuity_eligible":       "true",
		},
	})
	if err != nil {
		t.Fatalf("Create(named session): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionClose([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionClose(worker) = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

func TestPhase0MailRecipientIdentity_RejectsTemplateFactoryTarget(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
	}

	address, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "template:worker")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveMailRecipientIdentity(template:worker) = (%q, %v), want ErrSessionNotFound", address, err)
	}
	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("mail recipient resolution materialized %d session(s) for template:worker", len(all))
	}
}

func TestPhase0MailRecipientIdentity_BareConfigNameDoesNotMaterializeOrdinarySession(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
		}},
	}

	address, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "worker")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveMailRecipientIdentity(worker) = (%q, %v), want ErrSessionNotFound for ordinary config name", address, err)
	}
	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("mail recipient resolution materialized %d session(s) for ordinary config worker", len(all))
	}
}

func TestPhase0ConfiguredMailboxAddress_RigScopedBareNamedRequiresAmbientRig(t *testing.T) {
	rigDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "witness",
			Dir:  "demo",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
		Rigs: []config.Rig{{
			Name: "demo",
			Path: rigDir,
		}},
	}
	t.Setenv("GC_DIR", t.TempDir())

	address, ok := configuredMailboxAddressWithConfig(t.TempDir(), cfg, "witness")
	if ok {
		t.Fatalf("configuredMailboxAddressWithConfig(witness) = (%q, true), want no bare rig-scoped resolution without ambient rig", address)
	}
}

func TestPhase0NudgeTarget_BareRigScopedNamedRequiresAmbientRig(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DIR", t.TempDir())

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[rig]]
name = "demo"
path = "demo"

[[agent]]
name = "witness"
dir = "demo"
start_command = "true"
max_active_sessions = 1

[[named_session]]
template = "witness"
dir = "demo"
mode = "on_demand"
`)
	t.Setenv("GC_CITY", cityDir)

	target, err := resolveNudgeTarget("witness")
	if !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolveNudgeTarget(witness) = (%+v, %v), want ErrSessionNotFound without ambient rig", target, err)
	}
	if count := phase0InterfaceSessionCount(t, cityDir); count != 0 {
		t.Fatalf("resolveNudgeTarget(witness) materialized %d session(s) without ambient rig", count)
	}
}

func TestPhase0MailRecipientIdentity_HistoricalAliasDoesNotOverrideReservedNamedIdentity(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
			"session_name":  "s-gc-sky",
		},
	}); err != nil {
		t.Fatalf("create historical session: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "mayor",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "mayor",
			Mode:     "on_demand",
		}},
	}

	address, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveMailRecipientIdentity(mayor): %v", err)
	}
	if address != "mayor" {
		t.Fatalf("address = %q, want reserved configured named identity mayor, not historical alias sky", address)
	}
}

func TestPhase0CmdSessionNew_FactoryTargetDoesNotMaterializeNamedIdentity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 3

[[named_session]]
name = "mayor"
template = "worker"
mode = "on_demand"
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := cmdSessionNew([]string{"worker"}, "", "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionNew(worker) = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	b := onlySessionBead(t, cityDir)
	if got := b.Metadata["template"]; got != "worker" {
		t.Fatalf("template = %q, want worker", got)
	}
	if got := b.Metadata["configured_named_identity"]; got != "" {
		t.Fatalf("configured_named_identity = %q, want empty for factory-created ephemeral session", got)
	}
	if got := b.Metadata["configured_named_session"]; got != "" {
		t.Fatalf("configured_named_session = %q, want empty for factory-created ephemeral session", got)
	}
	if got := b.Metadata["alias"]; got != "" {
		t.Fatalf("alias = %q, want empty because factory create must not claim named identity mayor", got)
	}
	if got := b.Metadata["session_origin"]; got != "ephemeral" {
		t.Fatalf("session_origin = %q, want ephemeral", got)
	}
}

func writePhase0InterfaceCity(t *testing.T, cityDir, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func phase0InterfaceSessionCount(t *testing.T, cityDir string) int {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	return len(all)
}
