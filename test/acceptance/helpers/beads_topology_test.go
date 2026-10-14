package acceptancehelpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveLegacyGCBinarySeparatesUnsetFromMisconfigured pins the distinction
// the M5 shape used to lose. A "" meant both "the operator did not set
// GC_ACCEPTANCE_LEGACY_GC_BIN" and "they set it to something unusable", and
// StartTopology reads "" as the former, so a deleted binary or a typo skipped
// M5 with a message telling the operator to set a variable they already set.
func TestResolveLegacyGCBinarySeparatesUnsetFromMisconfigured(t *testing.T) {
	dir := t.TempDir()
	usable := filepath.Join(dir, "gc-pre-journal")
	if err := os.WriteFile(usable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write fixture binary: %v", err)
	}

	t.Run("unset is absence", func(t *testing.T) {
		bin, err := resolveLegacyGCBinary("")
		if err != nil || bin != "" {
			t.Fatalf("resolveLegacyGCBinary(\"\") = (%q, %v), want (\"\", nil)", bin, err)
		}
	})

	t.Run("blank is absence", func(t *testing.T) {
		bin, err := resolveLegacyGCBinary("   ")
		if err != nil || bin != "" {
			t.Fatalf("resolveLegacyGCBinary(blank) = (%q, %v), want (\"\", nil)", bin, err)
		}
	})

	t.Run("missing file fails", func(t *testing.T) {
		missing := filepath.Join(dir, "gone")
		bin, err := resolveLegacyGCBinary(missing)
		if err == nil {
			t.Fatalf("resolveLegacyGCBinary(%q) = (%q, nil), want an error", missing, bin)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Fatalf("error %q does not name the path %q", err, missing)
		}
		if !strings.Contains(err.Error(), "is not an executable file") {
			t.Fatalf("error %q does not say the path is unusable", err)
		}
	})

	t.Run("directory fails", func(t *testing.T) {
		if _, err := resolveLegacyGCBinary(dir); err == nil {
			t.Fatalf("resolveLegacyGCBinary(%q) accepted a directory", dir)
		}
	})

	t.Run("usable file resolves absolute", func(t *testing.T) {
		bin, err := resolveLegacyGCBinary(usable)
		if err != nil {
			t.Fatalf("resolveLegacyGCBinary(%q): %v", usable, err)
		}
		if !filepath.IsAbs(bin) || bin != usable {
			t.Fatalf("resolveLegacyGCBinary(%q) = %q, want %q", usable, bin, usable)
		}
	})
}

// TestEveryProxiedTopologyDeclaresACityStoreExpectation is the positive half of
// council C-F6's fix.
//
// The misleading half is now inexpressible: CityStore and CityStoreNativeLane
// live on BeadsTopology and are named for the scope they describe, so a Rig
// ScopeShape can no longer declare a store expectation nothing reads. What is
// left to guard is that moving them did not drop one — a shape that silently
// stopped declaring an expectation would make assertTopologyBeadsStore return
// early and the matrix would report a pass for a store nobody checked.
//
// It lives in the helpers package, which carries no build tag, so it runs in
// ordinary CI rather than only under the acceptance job the matrix needs.
func TestEveryProxiedTopologyDeclaresACityStoreExpectation(t *testing.T) {
	topologies := BeadsTopologies()
	if len(topologies) == 0 {
		t.Fatal("BeadsTopologies() is empty; this guard would pass vacuously")
	}
	proxied, offLaneFences := 0, 0
	for _, topo := range topologies {
		if topo.City.DoltMode != "proxied-server" {
			if topo.CityStoreNativeLane != nil {
				offLaneFences++
			}
			continue
		}
		proxied++
		if topo.CityStore == (BeadsStoreExpectation{}) {
			t.Errorf("%s is a proxied shape with no CityStore expectation: assertTopologyBeadsStore "+
				"returns early on the zero value, so the matrix would report a pass for a store it "+
				"never looked at", topo.Name)
		}
		if topo.CityStoreNativeLane == nil {
			t.Errorf("%s is a proxied shape that does not run the flag-on lane; the proxied shapes are "+
				"the only ones whose behavior the flag can change, so each must state its flag-on "+
				"expectation", topo.Name)
		}
	}
	if proxied == 0 {
		t.Fatal("no proxied topology found; the scan is broken, not satisfied")
	}
	// Counted over the NON-proxied shapes only (council pr2 D-F13). The guard
	// used to count every shape with a flag-on expectation and require two,
	// but the loop above already fails any proxied shape without one and there
	// are three proxied shapes, so the count was >= 3 whenever the test got
	// here: removing M2-direct-local's opt-in left it at 3, the guard passed,
	// and the matrix silently stopped running the flag-on lane on the only
	// shape that proves the flag changes nothing off its own lane.
	if offLaneFences == 0 {
		t.Fatal("no non-proxied shape runs the flag-on lane; at least one must opt in as the fence that " +
			"says the flag changes nothing off its own lane")
	}
}
