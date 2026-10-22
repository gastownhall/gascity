package config

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestStalledBeadsOverrides parses explicit limits without filling absent defaults.
func TestStalledBeadsOverrides(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		config   StalledBeadsConfig
		want     map[int]time.Duration
		fallback time.Duration
	}{
		{name: "unset", want: map[int]time.Duration{}},
		{
			name: "priority labels and fallback",
			config: StalledBeadsConfig{
				Thresholds:        map[string]string{"P0": "90m", "p3": "48h", "P7": "30m"},
				FallbackThreshold: "8h",
			},
			want:     map[int]time.Duration{0: 90 * time.Minute, 3: 48 * time.Hour, 7: 30 * time.Minute},
			fallback: 8 * time.Hour,
		},
		{
			name:   "positive fraction without fallback",
			config: StalledBeadsConfig{Thresholds: map[string]string{"P1": "0.5h"}},
			want:   map[int]time.Duration{1: 30 * time.Minute},
		},
		{
			name:     "fallback without priorities",
			config:   StalledBeadsConfig{FallbackThreshold: "45m"},
			want:     map[int]time.Duration{},
			fallback: 45 * time.Minute,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, fallback, err := tt.config.Overrides()
			if err != nil {
				t.Fatalf("parse overrides: %v", err)
			}
			if !maps.Equal(got, tt.want) || fallback != tt.fallback {
				t.Fatalf("overrides = (%v, %s), want (%v, %s)", got, fallback, tt.want, tt.fallback)
			}
			if warnings := ValidateDurations(&City{StalledBeads: tt.config}, "limits.toml"); len(warnings) != 0 {
				t.Fatalf("valid thresholds warned: %v", warnings)
			}
		})
	}
}

// TestStalledBeadsInvalidOverridesHaveContext requires errors and load warnings to identify bad settings.
func TestStalledBeadsInvalidOverridesHaveContext(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		config StalledBeadsConfig
		key    string
	}{
		{name: "non label", config: StalledBeadsConfig{Thresholds: map[string]string{"urgent": "1h"}}, key: "urgent"},
		{name: "negative priority", config: StalledBeadsConfig{Thresholds: map[string]string{"P-1": "1h"}}, key: "P-1"},
		{name: "priority overflow", config: StalledBeadsConfig{Thresholds: map[string]string{"P999999999999999999999999999999": "1h"}}, key: "P999999999999999999999999999999"},
		{name: "invalid threshold", config: StalledBeadsConfig{Thresholds: map[string]string{"P1": "soon"}}, key: "thresholds.P1"},
		{name: "zero threshold", config: StalledBeadsConfig{Thresholds: map[string]string{"P1": "0s"}}, key: "thresholds.P1"},
		{name: "negative threshold", config: StalledBeadsConfig{Thresholds: map[string]string{"P1": "-1h"}}, key: "thresholds.P1"},
		{name: "invalid fallback", config: StalledBeadsConfig{FallbackThreshold: "soon"}, key: "fallback_threshold"},
		{name: "zero fallback", config: StalledBeadsConfig{FallbackThreshold: "0s"}, key: "fallback_threshold"},
		{name: "negative fallback", config: StalledBeadsConfig{FallbackThreshold: "-1h"}, key: "fallback_threshold"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := tt.config.Overrides()
			if err == nil {
				t.Fatal("invalid override was accepted")
			}
			for _, part := range []string{"[stalled_beads]", tt.key} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q omits %q", err, part)
				}
			}
			warnings := ValidateDurations(&City{StalledBeads: tt.config}, "limits.toml")
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want one threshold warning", warnings)
			}
			if !strings.Contains(warnings[0], "limits.toml") || !strings.Contains(warnings[0], err.Error()) {
				t.Fatalf("warning = %q, want source and parse error %q", warnings[0], err)
			}
		})
	}
}

// TestSessionEscalationRecipient honors configured addresses and empty log-only mode.
func TestSessionEscalationRecipient(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, raw, want string }{
		{name: "unset", raw: "", want: ""},
		{name: "whitespace is log only", raw: " \t\n", want: ""},
		{name: "configured recipient", raw: "ops/alerts", want: "ops/alerts"},
		{name: "surrounding whitespace", raw: " \tops/alerts\n", want: "ops/alerts"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := SessionConfig{EscalationTo: tt.raw}
			if got := cfg.EscalationRecipient(); got != tt.want {
				t.Fatalf("recipient = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestComposeStalledBeadsSection proves that present fragments replace the section and absent ones preserve it.
func TestComposeStalledBeadsSection(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		fragment  string
		want      map[int]time.Duration
		fallback  time.Duration
		recipient string
	}{
		{name: "section absent", fragment: "[workspace]\nname = \"fragment\"\n", want: map[int]time.Duration{1: 6 * time.Hour}, fallback: 24 * time.Hour, recipient: "ops/root"},
		{name: "section replaced", fragment: "[stalled_beads]\nfallback_threshold = \"8h\"\n[stalled_beads.thresholds]\np3 = \"90m\"\n[session]\nescalation_to = \"  ops/fragment  \"\n", want: map[int]time.Duration{3: 90 * time.Minute}, fallback: 8 * time.Hour, recipient: "ops/fragment"},
		{name: "partial section replaces prior fallback", fragment: "[stalled_beads.thresholds]\nP0 = \"30m\"\n", want: map[int]time.Duration{0: 30 * time.Minute}, recipient: "ops/root"},
		{name: "empty section clears overrides", fragment: "[stalled_beads]\n", want: map[int]time.Duration{}, recipient: "ops/root"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte(`include = ["fragment.toml"]
[workspace]
name = "root"
[session]
escalation_to = "ops/root"
[stalled_beads]
fallback_threshold = "24h"
[stalled_beads.thresholds]
P1 = "6h"
`)
			fs.Files["/city/fragment.toml"] = []byte(tt.fragment)
			cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
			if err != nil {
				t.Fatalf("load included thresholds: %v", err)
			}
			got, fallback, err := cfg.StalledBeads.Overrides()
			if err != nil {
				t.Fatalf("parse composed thresholds: %v", err)
			}
			if !maps.Equal(got, tt.want) || fallback != tt.fallback {
				t.Fatalf("composed overrides = (%v, %s), want (%v, %s)", got, fallback, tt.want, tt.fallback)
			}
			if got := cfg.Session.EscalationRecipient(); got != tt.recipient {
				t.Fatalf("composed recipient = %q, want %q", got, tt.recipient)
			}
		})
	}
}
