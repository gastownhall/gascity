package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestNormalizedLeaseRenewalDefaultsToAuto pins the one place this gate departs
// from its beads.* siblings: omitted means AUTO, not off. Off is the state
// gas-76r exists to end — bd stamps a claim lease on every claim and bd reclaim
// is live, so shipping no renewal driver is what makes a working holder
// indistinguishable from a dead one.
func TestNormalizedLeaseRenewalDefaultsToAuto(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedLeaseRenewal(); got != "auto" {
		t.Fatalf("NormalizedLeaseRenewal = %q, want auto when omitted", got)
	}
}

// TestNormalizedLeaseRenewalPassesAnExplicitOffThrough proves the kill switch
// survives normalization: an operator who sets off gets off, not the default.
func TestNormalizedLeaseRenewalPassesAnExplicitOffThrough(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[beads]
lease_renewal = "off"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedLeaseRenewal(); got != "off" {
		t.Fatalf("NormalizedLeaseRenewal = %q, want off", got)
	}
}

// TestLoadWithIncludesPreservesLeaseRenewalAcrossBeadsFragment is the
// load-bearing regression, mirroring its two siblings: an included fragment
// that defines ONLY an unrelated [beads] key must NOT reset the root's explicit
// lease_renewal. Without the per-field preservation branch this is a silent
// downgrade of a correctness gate through routine config layering.
func TestLoadWithIncludesPreservesLeaseRenewalAcrossBeadsFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
lease_renewal = "off"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
bd_compatibility = "bd-1.0.5"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedLeaseRenewal(); got != "off" {
		t.Fatalf("NormalizedLeaseRenewal = %q, want the root's off to survive a [beads] fragment", got)
	}
	if cfg.Beads.NormalizedBDCompatibility() != "bd-1.0.5" {
		t.Fatalf("BDCompatibility = %q, want the fragment's bd-1.0.5", cfg.Beads.NormalizedBDCompatibility())
	}
}

// TestLeaseRenewalFragmentOverrideStillWins is the other half of preservation:
// a fragment that DOES set lease_renewal must override the root, or the
// preservation branch would freeze the root value permanently.
func TestLeaseRenewalFragmentOverrideStillWins(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
lease_renewal = "auto"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
lease_renewal = "off"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.NormalizedLeaseRenewal(); got != "off" {
		t.Fatalf("NormalizedLeaseRenewal = %q, want the fragment's off to win", got)
	}
}

// TestLeaseRenewalRejectsAnOutOfEnumValue proves a typo fails config load
// rather than silently meaning something else. "off" by typo is the dangerous
// direction here: the operator would believe leases are renewed while every
// live holder's lease lapses mid-work.
func TestLeaseRenewalRejectsAnOutOfEnumValue(t *testing.T) {
	_, err := Parse([]byte("[beads]\nlease_renewal = \"requre\"\n"))
	if err == nil {
		t.Fatal("Parse accepted an out-of-enum lease_renewal value")
	}
	if !strings.Contains(err.Error(), "beads.lease_renewal") {
		t.Errorf("error %q does not name the offending key", err)
	}
	// The in-enum spellings and the unset default must still load.
	for _, raw := range []string{"off", "auto", "require"} {
		if _, err := Parse([]byte("[beads]\nlease_renewal = \"" + raw + "\"\n")); err != nil {
			t.Errorf("Parse rejected valid lease_renewal %q: %v", raw, err)
		}
	}
}
