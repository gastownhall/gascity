package config

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestParseProxiedIdleTimeout(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr string
	}{
		{in: "0", want: 0},
		{in: "0s", want: 0},
		{in: "30m", want: 30 * time.Minute},
		{in: "1m", want: time.Minute},
		{in: "2h", want: 2 * time.Hour},
		{in: "59s", wantErr: "at least 1m0s"},
		{in: "-5m", wantErr: "must not be negative"},
		{in: "soon", wantErr: "not a valid duration"},
	}
	for _, tc := range cases {
		got, err := ParseProxiedIdleTimeout(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseProxiedIdleTimeout(%q) error = %v, want containing %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseProxiedIdleTimeout(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseProxiedIdleTimeout(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestProxiedIdleTimeoutForPrecedence(t *testing.T) {
	city := &City{Beads: BeadsConfig{ProxiedIdleTimeout: "45m"}}
	rig := &Rig{Name: "big", BeadsProxiedIdleTimeout: strPtr("0")}

	t.Setenv(ProxiedIdleTimeoutEnv, "")
	got, err := ProxiedIdleTimeoutFor(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Duration != DefaultProxiedIdleTimeout || got.Source != ProxiedIdleTimeoutSourceDefault {
		t.Fatalf("unset = %+v, want default", got)
	}

	got, err = ProxiedIdleTimeoutFor(city, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Duration != 45*time.Minute || got.Source != ProxiedIdleTimeoutSourceCity {
		t.Fatalf("city = %+v, want 45m from city", got)
	}

	got, err = ProxiedIdleTimeoutFor(city, rig)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Never() || got.Source != ProxiedIdleTimeoutSourceRig {
		t.Fatalf("rig = %+v, want never from rig", got)
	}

	// An explicit "0" on the city is never, distinct from unset.
	got, err = ProxiedIdleTimeoutFor(&City{Beads: BeadsConfig{ProxiedIdleTimeout: "0"}}, &Rig{Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Never() || got.Source != ProxiedIdleTimeoutSourceCity {
		t.Fatalf("city 0 = %+v, want never from city", got)
	}

	// The env override wins over everything and may go below the floor.
	t.Setenv(ProxiedIdleTimeoutEnv, "20s")
	got, err = ProxiedIdleTimeoutFor(city, rig)
	if err != nil {
		t.Fatal(err)
	}
	if got.Duration != 20*time.Second || got.Source != ProxiedIdleTimeoutSourceEnv {
		t.Fatalf("env = %+v, want 20s from env", got)
	}

	t.Setenv(ProxiedIdleTimeoutEnv, "-1s")
	if _, err := ProxiedIdleTimeoutFor(city, rig); err == nil || !strings.Contains(err.Error(), ProxiedIdleTimeoutEnv) {
		t.Fatalf("negative env error = %v, want one naming %s", err, ProxiedIdleTimeoutEnv)
	}
	t.Setenv(ProxiedIdleTimeoutEnv, "nope")
	if _, err := ProxiedIdleTimeoutFor(city, rig); err == nil {
		t.Fatal("garbage env accepted")
	}
}

func TestProxiedIdleTimeoutBdFlagValue(t *testing.T) {
	if got := (ProxiedIdleTimeout{}).BdFlagValue(); got != "0" {
		t.Fatalf("never flag = %q, want 0", got)
	}
	if got := (ProxiedIdleTimeout{Duration: 30 * time.Minute}).BdFlagValue(); got != "30m0s" {
		t.Fatalf("30m flag = %q, want 30m0s", got)
	}
}

func TestValidateProxiedIdleTimeouts(t *testing.T) {
	ok := &City{
		Beads: BeadsConfig{ProxiedIdleTimeout: "30m"},
		Rigs:  []Rig{{Name: "a", BeadsProxiedIdleTimeout: strPtr("0")}, {Name: "b"}},
	}
	if w := ValidateProxiedIdleTimeouts(ok, "city.toml"); len(w) != 0 {
		t.Fatalf("valid config warned: %v", w)
	}
	bad := []*City{
		{Beads: BeadsConfig{ProxiedIdleTimeout: "30s"}},
		{Beads: BeadsConfig{ProxiedIdleTimeout: "-1m"}},
		{Beads: BeadsConfig{ProxiedIdleTimeout: "forever"}},
		{Rigs: []Rig{{Name: "a", BeadsProxiedIdleTimeout: strPtr("10s")}}},
		{Rigs: []Rig{{Name: "a", BeadsProxiedIdleTimeout: strPtr("")}}},
	}
	for i, cfg := range bad {
		if w := ValidateProxiedIdleTimeouts(cfg, "city.toml"); len(w) == 0 {
			t.Errorf("case %d: invalid config accepted", i)
		}
	}
}

// A bad value warns at load instead of failing it: the rest of the city must
// still load, so `gc stop` can retire it. Initializing a scope with it fails.
func TestLoadWarnsOnProxiedIdleTimeoutBelowFloor(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[beads]
proxied_idle_timeout = "30s"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes failed on a bad idle timeout: %v", err)
	}
	found := false
	for _, w := range prov.Warnings {
		found = found || strings.Contains(w, "proxied_idle_timeout")
	}
	if !found {
		t.Fatalf("warnings = %v, want a proxied_idle_timeout warning", prov.Warnings)
	}
	t.Setenv(ProxiedIdleTimeoutEnv, "")
	if _, err := ProxiedIdleTimeoutFor(cfg, nil); err == nil {
		t.Fatal("resolving the bad value succeeded")
	}
}

func TestLoadParsesRigProxiedIdleTimeout(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[beads]
proxied_idle_timeout = "45m"

[[rigs]]
name = "big"
path = "/repos/big"
beads_proxied_idle_timeout = "0"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Beads.ProxiedIdleTimeout != "45m" {
		t.Fatalf("city value = %q, want 45m", cfg.Beads.ProxiedIdleTimeout)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].BeadsProxiedIdleTimeout == nil || *cfg.Rigs[0].BeadsProxiedIdleTimeout != "0" {
		t.Fatalf("rig override not parsed: %+v", cfg.Rigs)
	}
}

// A fragment that defines [beads] without proxied_idle_timeout must not reset
// the root's explicit value, the same rule conditional_writes follows.
func TestLoadWithIncludesPreservesProxiedIdleTimeoutAcrossBeadsFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
proxied_idle_timeout = "2h"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
bd_compatibility = "bd-1.0.5"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Beads.ProxiedIdleTimeout != "2h" {
		t.Fatalf("ProxiedIdleTimeout = %q, want root 2h to survive a [beads] fragment", cfg.Beads.ProxiedIdleTimeout)
	}

	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
proxied_idle_timeout = "0"
`)
	cfg, _, err = LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Beads.ProxiedIdleTimeout != "0" {
		t.Fatalf("ProxiedIdleTimeout = %q, want the fragment's 0 to win", cfg.Beads.ProxiedIdleTimeout)
	}
}

func TestApplyRigPatchProxiedIdleTimeout(t *testing.T) {
	cfg := &City{Rigs: []Rig{{Name: "mo", Path: "/mo"}}}
	if err := ApplyPatches(cfg, Patches{Rigs: []RigPatch{{Name: "mo", BeadsProxiedIdleTimeout: strPtr("0")}}}); err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}
	got := cfg.Rigs[0].BeadsProxiedIdleTimeout
	if got == nil || *got != "0" {
		t.Fatalf("BeadsProxiedIdleTimeout = %v, want 0", got)
	}
	// A patch without the field leaves the rig's value alone.
	if err := ApplyPatches(cfg, Patches{Rigs: []RigPatch{{Name: "mo", Prefix: strPtr("mo")}}}); err != nil {
		t.Fatalf("ApplyPatches: %v", err)
	}
	if got := cfg.Rigs[0].BeadsProxiedIdleTimeout; got == nil || *got != "0" {
		t.Fatalf("BeadsProxiedIdleTimeout = %v after an unrelated patch, want 0", got)
	}
}

func TestProxiedIdleTimeoutForScopeSharedRoot(t *testing.T) {
	t.Setenv(ProxiedIdleTimeoutEnv, "")
	city := &City{Beads: BeadsConfig{ProxiedIdleTimeout: "45m"}}
	rig := &Rig{Name: "r", BeadsProxiedIdleTimeout: strPtr("0")}
	got, ignored, err := ProxiedIdleTimeoutForScope(city, rig, true)
	if err != nil || !ignored || got.Duration != 45*time.Minute {
		t.Fatalf("shared root = %+v ignored=%v err=%v, want the city's 45m with the override ignored", got, ignored, err)
	}
	got, ignored, err = ProxiedIdleTimeoutForScope(city, rig, false)
	if err != nil || ignored || !got.Never() {
		t.Fatalf("own root = %+v ignored=%v err=%v, want the rig's never", got, ignored, err)
	}
	if _, ignored, _ := ProxiedIdleTimeoutForScope(city, &Rig{Name: "plain"}, true); ignored {
		t.Fatal("a shared-root rig with no override reported an ignored override")
	}
}
