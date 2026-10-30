package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestMissingArgsAppend(t *testing.T) {
	tests := []struct {
		name string
		rp   config.ResolvedProvider
		want []string
	}{
		{
			name: "no args_append",
			rp:   config.ResolvedProvider{Args: []string{"--agent-flag"}},
			want: nil,
		},
		{
			name: "every entry present",
			rp: config.ResolvedProvider{
				Args:       []string{"--agent-flag", "-c", "model_context_window=1000000"},
				ArgsAppend: []string{"-c", "model_context_window=1000000"},
			},
			want: nil,
		},
		{
			name: "dropped by an args override",
			rp: config.ResolvedProvider{
				Args:       []string{"--agent-flag"},
				ArgsAppend: []string{"-c", "model_context_window=1000000"},
			},
			want: []string{"-c", "model_context_window=1000000"},
		},
		{
			name: "only the value is missing",
			rp: config.ResolvedProvider{
				Args:       []string{"-c", "other=1"},
				ArgsAppend: []string{"-c", "model_context_window=1000000"},
			},
			want: []string{"model_context_window=1000000"},
		},
		{
			name: "a repeated entry must be repeated in Args",
			rp: config.ResolvedProvider{
				Args:       []string{"-c", "a=1"},
				ArgsAppend: []string{"-c", "a=1", "-c", "b=2"},
			},
			want: []string{"-c", "b=2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := missingArgsAppend(&tt.rp); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("missingArgsAppend = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProviderArgsAppendDoctorCheckIsAWarning(t *testing.T) {
	check := newProviderArgsAppendDoctorCheck(t.TempDir())
	if check.Name() != "provider-args-append" {
		t.Errorf("Name = %q, want provider-args-append", check.Name())
	}
	if check.CanFix() {
		t.Error("CanFix = true, want false: the check only reports")
	}
	if check.WarmupEligible() {
		t.Error("WarmupEligible = true, want false")
	}
}

func TestProviderArgsAppendDoctorCheckPassesWhenArgsOverrideKeepsAppend(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "t"
provider = "codex"

[providers.codex]
base = "builtin:codex"
args_append = ["-c", "model_context_window=1000000"]

[[agent]]
name = "scout"
provider = "codex"
args = ["--agent-flag"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newProviderArgsAppendDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; message=%q details=%v", r.Status, r.Message, r.Details)
	}
}

func TestProviderArgsAppendDoctorCheckSkipsUnloadableConfig(t *testing.T) {
	cityDir := t.TempDir()
	r := newProviderArgsAppendDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "skipped") {
		t.Fatalf("status = %v message=%q, want OK and skipped", r.Status, r.Message)
	}
}
