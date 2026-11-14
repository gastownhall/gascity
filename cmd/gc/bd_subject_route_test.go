package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// bdRoutesOwnerFixture builds the adopted-store city the routes arm exists
// for: config derives an HQ prefix of "ci" (DeriveBeadsPrefix("city")) while
// the city store mints city-*, and routes.jsonl spells the REAL prefixes —
// the registration collectRigRoutes writes since city-fr3t.
type bdRoutesOwnerFixture struct {
	cityPath string
	cfg      *config.City
	cityTgt  execStoreTarget
}

func newBdRoutesOwnerFixture(t *testing.T) (bdRoutesOwnerFixture, string, string) {
	t.Helper()
	cityDir := t.TempDir()
	rigADir := filepath.Join(cityDir, "rig-a")
	rigBDir := filepath.Join(cityDir, "rig-b")
	for _, dir := range []string{cityDir, rigADir, rigBDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"}, // derived HQ prefix: "ci"
		Rigs: []config.Rig{
			{Name: "rig-a", Path: rigADir, Prefix: "gka"},
			{Name: "rig-b", Path: rigBDir, Prefix: "gkb"},
		},
	}
	f := bdRoutesOwnerFixture{
		cityPath: cityDir,
		cfg:      cfg,
		cityTgt:  bdCityScopeTarget(cityDir, cfg),
	}
	return f, rigADir, rigBDir
}

func writeRoutes(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".beads", "routes.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The defect: a by-id invocation for a city-* id from a rig must answer from
// the city store even though no CONFIGURED prefix owns "city" — the routes
// registration does, and bdRoutesOwner reads it.
func TestBdPrefixOwnerRoutesArmResolvesRealIssuePrefix(t *testing.T) {
	f, rigA, _ := newBdRoutesOwnerFixture(t)
	writeRoutes(t, f.cityPath, `{"prefix":"city","path":"."}
{"prefix":"gka","path":"rig-a"}
`)
	writeRoutes(t, rigA, `{"prefix":"city","path":".."}
{"prefix":"gka","path":"."}
`)

	owner, ok := bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "city-fr3t")
	if !ok || owner != f.cityTgt {
		t.Fatalf("bdPrefixOwner(city-fr3t) = (%+v, %v), want the city store", owner, ok)
	}

	// A configured prefix is still answered by the configured arm, routes
	// notwithstanding.
	rigTgt := bdRigScopeTarget(f.cityPath, f.cfg.Rigs[0])
	owner, ok = bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "gka-9x2")
	if !ok || owner != rigTgt {
		t.Fatalf("bdPrefixOwner(gka-9x2) = (%+v, %v), want rig-a's store", owner, ok)
	}

	// An id no registration owns declines to the caller's probe chain.
	if _, ok := bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "unknown-7x"); ok {
		t.Fatal("bdPrefixOwner(unknown-7x) = ok, want false")
	}
}

// The routes arm is what fixes the defect end to end: a by-id read routed by
// its subject's prefix alone, no existence probe.
func TestBdSubjectPrefixTargetRoutesCityIDFromRig(t *testing.T) {
	f, rigA, _ := newBdRoutesOwnerFixture(t)
	writeRoutes(t, f.cityPath, `{"prefix":"city","path":"."}
{"prefix":"gka","path":"rig-a"}
`)
	writeRoutes(t, rigA, `{"prefix":"city","path":".."}
{"prefix":"gka","path":"."}
`)

	target, ok := bdSubjectPrefixTarget(f.cfg, f.cityTgt, f.cityPath, []string{"show", "city-fr3t"})
	if !ok || target != f.cityTgt {
		t.Fatalf("bdSubjectPrefixTarget(show city-fr3t) = (%+v, %v), want the city store", target, ok)
	}

	// Subjects owned by DIFFERENT stores decline: subject routing answers
	// single-store argv only.
	if _, ok := bdSubjectPrefixTarget(f.cfg, f.cityTgt, f.cityPath, []string{"show", "city-fr3t", "gka-9x2"}); ok {
		t.Fatal("bdSubjectPrefixTarget across two stores = ok, want false")
	}
}

// Overlapping registered prefixes resolve by longest match, mirroring the
// configured matcher: "be-front" owns be-front-* and "be" owns the rest of
// be-*. Neither prefix is configured — the rigs stay gka/gkb — so this is the
// routes arm's own longest-match rule.
func TestBdRoutesOwnerLongestPrefixWins(t *testing.T) {
	f, rigA, rigB := newBdRoutesOwnerFixture(t)
	writeRoutes(t, f.cityPath, `{"prefix":"be","path":"rig-a"}
{"prefix":"be-front","path":"rig-b"}
`)
	_ = rigA
	_ = rigB

	rigATgt := bdRigScopeTarget(f.cityPath, f.cfg.Rigs[0])
	owner, ok := bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "be-77a")
	if !ok || owner != rigATgt {
		t.Fatalf("bdPrefixOwner(be-77a) = (%+v, %v), want rig-a (short prefix)", owner, ok)
	}
	rigBTgt := bdRigScopeTarget(f.cityPath, f.cfg.Rigs[1])
	owner, ok = bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "be-front-x1")
	if !ok || owner != rigBTgt {
		t.Fatalf("bdPrefixOwner(be-front-x1) = (%+v, %v), want rig-b (longest match)", owner, ok)
	}
}

// A route naming a path outside the city and the bound rigs is skipped — like
// the API plane's resolver, it says nothing about this city's stores, and the
// caller keeps its probe chain rather than pinning a store routes hallucinate.
func TestBdRoutesOwnerSkipsUnregisteredPath(t *testing.T) {
	f, _, _ := newBdRoutesOwnerFixture(t)
	writeRoutes(t, f.cityPath, `{"prefix":"ghost","path":"../elsewhere"}
`)

	if _, ok := bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "ghost-1a"); ok {
		t.Fatal("bdPrefixOwner(ghost-1a) = ok, want false for an unregistered route target")
	}
}

// The same prefix registered by two stores naming different targets is
// registration telling the truth twice; ambiguity declines to the probes.
func TestBdRoutesOwnerAmbiguousRegistrationDeclines(t *testing.T) {
	f, rigA, _ := newBdRoutesOwnerFixture(t)
	writeRoutes(t, rigA, `{"prefix":"city","path":"."}
`)
	writeRoutes(t, f.cityPath, `{"prefix":"city","path":"rig-b"}
`)

	if _, ok := bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "city-fr3t"); ok {
		t.Fatal("bdPrefixOwner(city-fr3t) = ok, want false when two routes name the same prefix for different stores")
	}
}

// The suffix shape gate travels with the routes arm: a prefix-matching token
// that is not a bead ID shape is not routed on.
func TestBdRoutesOwnerShapeGate(t *testing.T) {
	f, _, _ := newBdRoutesOwnerFixture(t)
	writeRoutes(t, f.cityPath, `{"prefix":"city","path":"."}
`)

	if _, ok := bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "city-123456789"); ok {
		t.Fatal("9-char suffix must not route via the routes arm")
	}
	// An ID-shaped token with a hierarchical part routes.
	owner, ok := bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "city-fr3t.1")
	if !ok || owner != f.cityTgt {
		t.Fatalf("bdPrefixOwner(city-fr3t.1) = (%+v, %v), want the city store", owner, ok)
	}
}

// A configured rig matching the id but lacking a path binding declines BEFORE
// the routes arm — resolveBdScopeTarget deliberately never aliases an unbound
// rig to the city store, and routes must not either.
func TestBdPrefixOwnerUnboundRigDeclinesBeforeRoutes(t *testing.T) {
	f, _, _ := newBdRoutesOwnerFixture(t)
	f.cfg.Rigs[0].Prefix = "city"
	f.cfg.Rigs[0].Path = ""
	writeRoutes(t, f.cityPath, `{"prefix":"city","path":"."}
`)

	if _, ok := bdPrefixOwner(f.cfg, f.cityTgt, f.cityPath, "city-fr3t"); ok {
		t.Fatal("unbound rig match must decline, not fall through to routes")
	}
}
