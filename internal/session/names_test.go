package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

func TestValidateExplicitName(t *testing.T) {
	longName := strings.Repeat("a", explicitSessionNameMaxLen+1)
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "empty allowed", input: "", want: ""},
		{name: "trimmed", input: "  sky  ", want: "sky"},
		{name: "single char", input: "x", want: "x"},
		{name: "bad syntax", input: "sky.chat", wantErr: ErrInvalidSessionName},
		{name: "reserved prefix", input: "s-gc-123", wantErr: ErrInvalidSessionName},
		{name: "too long", input: longName, wantErr: ErrInvalidSessionName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateExplicitName(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr.Error()) {
					t.Fatalf("error = %v, want contains %q", err, tt.wantErr.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateAlias(t *testing.T) {
	longAlias := strings.Repeat("a", explicitSessionNameMaxLen+1)
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "empty allowed", input: "", want: ""},
		{name: "trimmed qualified", input: "  myrig/worker  ", want: "myrig/worker"},
		{name: "binding qualified", input: "employees.corp--alex", want: "employees.corp--alex"},
		{name: "reserved prefix", input: "s-gc-123", wantErr: ErrInvalidSessionAlias},
		{name: "human reserved", input: "human", wantErr: ErrInvalidSessionAlias},
		{name: "session id syntax reserved", input: "gc-42", wantErr: ErrInvalidSessionAlias},
		{name: "bad syntax", input: "bad:name", wantErr: ErrInvalidSessionAlias},
		{name: "too long", input: longAlias, wantErr: ErrInvalidSessionAlias},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateAlias(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr.Error()) {
					t.Fatalf("error = %v, want contains %q", err, tt.wantErr.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpdatedAliasMetadataPreservesPriorAliases(t *testing.T) {
	got := UpdatedAliasMetadata(map[string]string{
		"alias":         "mayor",
		"alias_history": "witness,sky",
	}, "sky")

	if got["alias"] != "sky" {
		t.Fatalf("alias = %q, want sky", got["alias"])
	}
	if got["alias_history"] != "mayor,witness" {
		t.Fatalf("alias_history = %q, want mayor,witness", got["alias_history"])
	}
}

func TestEnsureSessionNameAvailable_RejectsOpenIdentifierCollisions(t *testing.T) {
	store := beads.NewMemStore()
	open, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"template": "myrig/worker",
		},
	})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}

	if err := ensureSessionNameAvailable(store, "worker"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("ensureSessionNameAvailable(open collision) error = %v, want %v", err, ErrSessionNameExists)
	}

	if err := store.Close(open.ID); err != nil {
		t.Fatalf("Close(open): %v", err)
	}
	if err := ensureSessionNameAvailable(store, "worker"); err != nil {
		t.Fatalf("ensureSessionNameAvailable(closed collision) = %v, want nil", err)
	}
}

func TestEnsureSessionNameAvailable_RejectsLiveAliasCollisions(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias": "worker",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := ensureSessionNameAvailable(store, "worker"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("ensureSessionNameAvailable(alias collision) error = %v, want %v", err, ErrSessionNameExists)
	}
}

func TestEnsureSessionNameAvailable_AllowsLiveAliasHistoryReuse(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := ensureSessionNameAvailable(store, "mayor"); err != nil {
		t.Fatalf("ensureSessionNameAvailable(alias history reuse) = %v, want nil", err)
	}
}

func TestEnsureSessionNameAvailable_AllowsClosedConfiguredNamedSession(t *testing.T) {
	store := beads.NewMemStore()

	// Create a configured named session bead and close it.
	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":              "witness",
			"configured_named_session":  "true",
			"configured_named_identity": "myrig/witness",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A closed configured named session should NOT block reuse of the
	// session name. The design doc states: "Closed historical beads do not
	// poison future canonical materialization of the reserved identity."
	if err := ensureSessionNameAvailable(store, "witness"); err != nil {
		t.Fatalf("ensureSessionNameAvailable(closed configured named) = %v, want nil", err)
	}
}

func TestEnsureSessionNameAvailable_RejectsClosedAdHocSession(t *testing.T) {
	store := beads.NewMemStore()

	// Create an ad-hoc session (no configured_named_session) and close it.
	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "my-custom-session",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Ad-hoc session names remain permanent even after close.
	if err := ensureSessionNameAvailable(store, "my-custom-session"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("ensureSessionNameAvailable(closed ad-hoc) = %v, want %v", err, ErrSessionNameExists)
	}
}

func TestEnsureAliasAvailableWithConfig_AllowsLiveAliasHistoryReuse(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := EnsureAliasAvailable(store, "mayor", ""); err != nil {
		t.Fatalf("EnsureAliasAvailable(history reuse) = %v, want nil", err)
	}
}

func TestEnsureAliasAvailable_AllowsClosedSessionNameReuse(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := EnsureAliasAvailable(store, "sky", ""); err != nil {
		t.Fatalf("EnsureAliasAvailable(closed session_name collision) = %v, want nil", err)
	}
}

func TestEnsureAliasAvailable_RejectsLiveSessionNameCollision(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "sky",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := EnsureAliasAvailable(store, "sky", ""); !errors.Is(err, ErrSessionAliasExists) {
		t.Fatalf("EnsureAliasAvailable(live session_name collision) error = %v, want %v", err, ErrSessionAliasExists)
	}
}

func TestEnsureAliasAvailableWithConfig_RejectsConfiguredSingletonAlias(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
			{Template: "polecat", Dir: "myrig"},
		},
	}

	if err := EnsureAliasAvailableWithConfig(store, cfg, "myrig/polecat", ""); !errors.Is(err, ErrSessionAliasExists) {
		t.Fatalf("EnsureAliasAvailableWithConfig(reserved singleton) error = %v, want %v", err, ErrSessionAliasExists)
	}
}

func TestEnsureAliasAvailableWithConfig_AllowsConfiguredSingletonSelf(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			aliasHistoryMetadataKey:     "old",
			"alias":                     "myrig/polecat",
			"configured_named_session":  "true",
			"configured_named_identity": "myrig/polecat",
			"configured_named_mode":     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg := &config.City{
		NamedSessions: []config.NamedSession{
			{Template: "polecat", Dir: "myrig"},
		},
	}

	if err := EnsureAliasAvailableWithConfig(store, cfg, "myrig/polecat", bead.ID); err != nil {
		t.Fatalf("EnsureAliasAvailableWithConfig(self singleton) = %v, want nil", err)
	}
}

func TestEnsureAliasAvailableWithConfig_RejectsForkedSingletonSelf(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession, "template:myrig/polecat"},
		Metadata: map[string]string{
			"template": "myrig/polecat",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg := &config.City{
		NamedSessions: []config.NamedSession{
			{Template: "polecat", Dir: "myrig"},
		},
	}

	if err := EnsureAliasAvailableWithConfig(store, cfg, "myrig/polecat", bead.ID); !errors.Is(err, ErrSessionAliasExists) {
		t.Fatalf("EnsureAliasAvailableWithConfig(fork self) error = %v, want %v", err, ErrSessionAliasExists)
	}
}

func TestEnsureAliasAvailableWithConfigForOwner_AllowsConfiguredSingletonCreate(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		NamedSessions: []config.NamedSession{
			{Template: "worker"},
			{Template: "polecat", Dir: "myrig"},
		},
	}

	if err := EnsureAliasAvailableWithConfigForOwner(store, cfg, "worker", "", "worker"); err != nil {
		t.Fatalf("EnsureAliasAvailableWithConfigForOwner(worker create) = %v, want nil", err)
	}
	if err := EnsureAliasAvailableWithConfigForOwner(store, cfg, "myrig/polecat", "", "myrig/polecat"); err != nil {
		t.Fatalf("EnsureAliasAvailableWithConfigForOwner(rig singleton create) = %v, want nil", err)
	}
}

func TestEnsureAliasAvailableWithConfigForOwner_AllowsConfiguredAliasAgainstOrdinaryConcreteIdentity(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		NamedSessions: []config.NamedSession{
			{Template: "worker", Dir: "myrig"},
		},
	}
	if _, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-ordinary-worker",
			"template":     "myrig/worker",
			"agent_name":   "myrig/worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := EnsureAliasAvailableWithConfigForOwner(store, cfg, "myrig/worker", "", "myrig/worker"); err != nil {
		t.Fatalf("EnsureAliasAvailableWithConfigForOwner(named owner vs concrete identity) = %v, want nil", err)
	}
}

func TestEnsureSessionNameAvailableWithConfig_UsesResolvedWorkspaceName(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "test-city",
		Workspace: config.Workspace{
			SessionTemplate: "{{.City}}--{{.Name}}",
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}

	if err := EnsureSessionNameAvailableWithConfig(store, cfg, "test-city--mayor", ""); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("EnsureSessionNameAvailableWithConfig(resolved workspace name) error = %v, want %v", err, ErrSessionNameExists)
	}
	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, "test-city--mayor", "", "mayor"); err != nil {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(self resolved workspace name) = %v, want nil", err)
	}
}

func TestWithCitySessionNameLock_EmptyCityPathFallsBackWithoutLockFile(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(tmp): %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()

	called := false
	if err := WithCitySessionNameLock("", "sky", func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithCitySessionNameLock: %v", err)
	}
	if !called {
		t.Fatal("lock function did not execute")
	}
	if _, err := os.Stat(filepath.Join(tmp, ".gc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".gc should not be created for empty cityPath, got err=%v", err)
	}
}

func TestWithCitySessionNameLock_HashesUntrustedIdentifier(t *testing.T) {
	cityPath := t.TempDir()
	identifier := "../escape"

	if err := WithCitySessionNameLock(cityPath, identifier, func() error { return nil }); err != nil {
		t.Fatalf("WithCitySessionNameLock: %v", err)
	}

	lockDir := citylayout.SessionNameLocksDir(cityPath)
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", lockDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("lock files = %d, want 1", len(entries))
	}
	name := entries[0].Name()
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		t.Fatalf("lock file name = %q, want hashed file name without path tokens", name)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(lockDir), "escape.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock escaped lock dir, stat err=%v", err)
	}
}

// BUG: PR #204 — closed named session beads blocked name reuse on restart.
// The real fix (superseding PR #204) is to REOPEN the old bead instead of
// creating a new one, preserving the bead ID for reference continuity.
// ensureSessionNameAvailable intentionally rejects closed explicit names —
// the reopen path in session_template_start.go handles it at a higher level
// via findClosedNamedSessionBead.
//
// This test verifies the low-level name check still rejects closed names
// (which is correct — the reopen path bypasses name reservation entirely).
func TestEnsureSessionNameAvailable_RejectsClosedExplicitName(t *testing.T) {
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "my-worker",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Closed explicit names are intentionally reserved — the higher-level
	// reopen path handles restart by reopening the old bead.
	err = ensureSessionNameAvailable(store, "my-worker")
	if !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("ensureSessionNameAvailable(closed explicit name) = %v, want ErrSessionNameExists (reopen path handles this)", err)
	}
}

func TestEnsureSessionNameAvailableWithConfigForOwner_AllowsClosedSelfReopen(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "test-city",
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}
	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":              "test-city--mayor",
			"alias":                     "mayor",
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
			"configured_named_mode":     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := EnsureAliasAvailableWithConfigForOwner(store, cfg, "mayor", bead.ID, "mayor"); err != nil {
		t.Fatalf("EnsureAliasAvailableWithConfigForOwner(reopen self alias) = %v, want nil", err)
	}
	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, "test-city--mayor", bead.ID, "mayor"); err != nil {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(reopen self session_name) = %v, want nil", err)
	}
}

// TestEnsureConfiguredSessionNameAvailable_AllowsClosedLegacyBeadForOwner
// covers the cold-boot scenario where a closed bead predates the
// configured_named_session flag and still holds the session_name. The
// config-aware path should allow reuse when the caller owns the configured
// named session.
func TestEnsureConfiguredSessionNameAvailable_AllowsClosedLegacyBeadForOwner(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "gc-management",
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}

	// Create a legacy bead: closed/orphaned, holds session_name "mayor",
	// but does NOT have configured_named_session=true (predates the flag).
	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":          "mayor",
			"session_name_explicit": "true",
			"template":              "mayor",
			"close_reason":          "orphaned",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Base check (no config) should still reject — ad-hoc semantics.
	if err := ensureSessionNameAvailable(store, "mayor"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("ensureSessionNameAvailable(legacy closed) = %v, want ErrSessionNameExists", err)
	}

	// Config-aware check with matching owner should allow reuse.
	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, "mayor", "", "mayor"); err != nil {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(legacy closed, matching owner) = %v, want nil", err)
	}
}

// TestEnsureConfiguredSessionNameAvailable_RejectsClosedLegacyBeadWrongOwner
// verifies that the config-aware bypass does not allow a different configured
// named session to steal a name held by a closed legacy bead.
func TestEnsureConfiguredSessionNameAvailable_RejectsClosedLegacyBeadWrongOwner(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "gc-management",
		NamedSessions: []config.NamedSession{
			{Template: "foreman"},
		},
	}

	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":          "mayor",
			"session_name_explicit": "true",
			"template":              "mayor",
			"close_reason":          "orphaned",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// "foreman" does not own "mayor" — should be rejected.
	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, "mayor", "", "foreman"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(wrong owner) = %v, want ErrSessionNameExists", err)
	}
}

// TestEnsureConfiguredSessionNameAvailable_RejectsLiveLegacyBead verifies
// that even with config and matching owner, an open (non-closed) bead still
// blocks name reuse.
func TestEnsureConfiguredSessionNameAvailable_RejectsLiveLegacyBead(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "gc-management",
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}

	// Create a live bead (not closed) that holds the name.
	_, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":          "mayor",
			"session_name_explicit": "true",
			"template":              "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Even with matching owner, a live bead blocks.
	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, "mayor", "", "mayor"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(live bead) = %v, want ErrSessionNameExists", err)
	}
}

// TestEnsureConfiguredSessionNameAvailable_RejectsWithoutConfig verifies that
// the legacy bypass requires config context — nil config gets no special treatment.
func TestEnsureConfiguredSessionNameAvailable_RejectsWithoutConfig(t *testing.T) {
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
			"close_reason": "orphaned",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// No config — should still reject.
	if err := EnsureSessionNameAvailableWithConfigForOwner(store, nil, "mayor", "", "mayor"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(nil config) = %v, want ErrSessionNameExists", err)
	}
}

// TestEnsureConfiguredSessionNameAvailable_AllowsClosedLegacyWithWorkspacePrefix
// covers the case where the runtime name includes a workspace prefix
// (e.g., "gc-management--mayor") which is the standard production format.
func TestEnsureConfiguredSessionNameAvailable_AllowsClosedLegacyWithWorkspacePrefix(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "test-city",
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}

	runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")

	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": runtimeName,
			"close_reason": "orphaned",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, runtimeName, "", "mayor"); err != nil {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(workspace-prefixed) = %v, want nil", err)
	}
}

// TestEnsureConfiguredSessionNameAvailable_RejectsLiveAliasCollisionDespiteLegacyBypass
// verifies that the legacy-bypass path does not suppress rejections from live
// alias collisions. A live ad-hoc session holding the target name as its alias
// must still block, even when a closed legacy bead holds the session_name.
func TestEnsureConfiguredSessionNameAvailable_RejectsLiveAliasCollisionDespiteLegacyBypass(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "gc-management",
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}

	// Closed legacy bead holding the session_name (no configured_named_session flag).
	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
			"close_reason": "orphaned",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close legacy: %v", err)
	}

	// Live ad-hoc session using "mayor" as an alias.
	_, err = store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias": "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create live alias: %v", err)
	}

	// Must reject — live alias collision takes precedence over legacy bypass.
	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, "mayor", "", "mayor"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(live alias collision) = %v, want ErrSessionNameExists", err)
	}
}

// TestEnsureConfiguredSessionNameAvailable_AllowsLiveAliasHistoryReuseDespiteLegacyBypass
// verifies that historical aliases do not reserve namespace for configured
// named session creation.
func TestEnsureConfiguredSessionNameAvailable_AllowsLiveAliasHistoryReuseDespiteLegacyBypass(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "gc-management",
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}

	// Closed legacy bead.
	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
			"close_reason": "orphaned",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close legacy: %v", err)
	}

	// Live session with "mayor" in alias history.
	_, err = store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create live alias history: %v", err)
	}

	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, "mayor", "", "mayor"); err != nil {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(live alias history reuse) = %v, want nil", err)
	}
}

// TestEnsureConfiguredSessionNameAvailable_RejectsLiveIdentifierCollisionDespiteLegacyBypass
// verifies that a live bead's identifier (template/common_name) blocks the legacy bypass.
func TestEnsureConfiguredSessionNameAvailable_RejectsLiveIdentifierCollisionDespiteLegacyBypass(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{
		ResolvedWorkspaceName: "gc-management",
		NamedSessions: []config.NamedSession{
			{Template: "mayor"},
		},
	}

	// Closed legacy bead.
	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
			"close_reason": "orphaned",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close legacy: %v", err)
	}

	// Live session with "mayor" as template identifier.
	_, err = store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"template": "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create live identifier: %v", err)
	}

	if err := EnsureSessionNameAvailableWithConfigForOwner(store, cfg, "mayor", "", "mayor"); !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("EnsureSessionNameAvailableWithConfigForOwner(live identifier collision) = %v, want ErrSessionNameExists", err)
	}
}

func TestWithCitySessionLocks_EmptyCityPathSharesIdentifierNamespace(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	aliasDone := make(chan error, 1)
	go func() {
		aliasDone <- WithCitySessionAliasLock("", "sky", func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	acquired := make(chan struct{})
	nameDone := make(chan error, 1)
	go func() {
		nameDone <- WithCitySessionNameLock("", "sky", func() error {
			close(acquired)
			return nil
		})
	}()

	select {
	case <-acquired:
		t.Fatal("session name lock acquired before alias lock released")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-aliasDone; err != nil {
		t.Fatalf("WithCitySessionAliasLock: %v", err)
	}
	if err := <-nameDone; err != nil {
		t.Fatalf("WithCitySessionNameLock: %v", err)
	}
	<-acquired
}
