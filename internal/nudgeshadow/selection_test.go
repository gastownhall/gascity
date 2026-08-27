package nudgeshadow

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func nudgeShadowPtr(value string) *string { return &value }

func TestResolve(t *testing.T) {
	tests := []struct {
		name       string
		city       *config.City
		want       Selection
		wantErr    string
		wantNeeded bool
	}{
		{
			name: "omitted selects built-in off",
			city: &config.City{},
			want: Selection{Mode: Off, Provenance: Builtin},
		},
		{
			name: "explicit off selects configured off",
			city: &config.City{Daemon: config.DaemonConfig{NudgeShadow: nudgeShadowPtr("off")}},
			want: Selection{Mode: Off, Provenance: Config},
		},
		{
			name:       "required works while session reconciler is off",
			city:       &config.City{Daemon: config.DaemonConfig{NudgeShadow: nudgeShadowPtr("required"), SessionReconciler: "off"}},
			want:       Selection{Mode: Required, Provenance: Config},
			wantNeeded: true,
		},
		{
			name: "session reconciler require alone remains built-in off",
			city: &config.City{Daemon: config.DaemonConfig{SessionReconciler: "require"}},
			want: Selection{Mode: Off, Provenance: Builtin},
		},
		{
			name:    "nil config is rejected",
			wantErr: "nil",
		},
		{
			name:    "explicit empty is rejected",
			city:    &config.City{Daemon: config.DaemonConfig{NudgeShadow: nudgeShadowPtr("")}},
			wantErr: `invalid nudge_shadow ""`,
		},
		{
			name:    "auto is rejected",
			city:    &config.City{Daemon: config.DaemonConfig{NudgeShadow: nudgeShadowPtr("auto")}},
			wantErr: "auto",
		},
		{
			name:    "require is rejected",
			city:    &config.City{Daemon: config.DaemonConfig{NudgeShadow: nudgeShadowPtr("require")}},
			wantErr: "require",
		},
		{
			name:    "leading whitespace is rejected",
			city:    &config.City{Daemon: config.DaemonConfig{NudgeShadow: nudgeShadowPtr(" required")}},
			wantErr: " required",
		},
		{
			name:    "trailing whitespace is rejected",
			city:    &config.City{Daemon: config.DaemonConfig{NudgeShadow: nudgeShadowPtr("off ")}},
			wantErr: "off ",
		},
		{
			name:    "garbage is rejected",
			city:    &config.City{Daemon: config.DaemonConfig{NudgeShadow: nudgeShadowPtr("garbage")}},
			wantErr: "garbage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.city)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("Resolve error = nil, want error")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Resolve error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Resolve = %+v, want %+v", got, tt.want)
			}
			if got.Required() != tt.wantNeeded {
				t.Fatalf("Selection.Required() = %t, want %t", got.Required(), tt.wantNeeded)
			}
		})
	}
}

func TestResolveRejectsExplicitEmptyParsedFromRootTOML(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[daemon]
nudge_shadow = ""
`)

	cfg, _, err := config.LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	_, err = Resolve(cfg)
	if err == nil {
		t.Fatal("Resolve error = nil, want explicit empty nudge_shadow rejected")
	}
	if !strings.Contains(err.Error(), `invalid nudge_shadow ""`) {
		t.Fatalf("Resolve error = %q, want explicit empty nudge_shadow error", err)
	}
}

func TestResolveRejectsExplicitEmptyParsedFromFragmentTOML(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[daemon]
nudge_shadow = "required"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[daemon]
nudge_shadow = ""
`)

	cfg, _, err := config.LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	_, err = Resolve(cfg)
	if err == nil {
		t.Fatal("Resolve error = nil, want fragment's explicit empty nudge_shadow rejected")
	}
	if !strings.Contains(err.Error(), `invalid nudge_shadow ""`) {
		t.Fatalf("Resolve error = %q, want fragment's explicit empty nudge_shadow error", err)
	}
}
