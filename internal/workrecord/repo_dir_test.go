package workrecord

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// fixedScopeDirs is a plane's checkout table, spelled out. The real
// implementations read a loaded city config; what the rule needs from them is
// only "the city directory" and "this rig's directory, if the plane configures
// that rig at all".
type fixedScopeDirs struct {
	city string
	rigs map[string]string
}

func (d fixedScopeDirs) CityDir() string { return d.city }

func (d fixedScopeDirs) RigDir(name string) (string, bool) {
	dir, ok := d.rigs[name]
	return dir, ok
}

// TestRepoDirFor is the shared rule both close doors run. It answers the one
// question the work-record contract cannot answer from the bead's outcome
// alone — "reachable on WHICH repository?" — from the bead's OWNER
// (gc.root_store_ref), not from the store the row happened to be read through.
//
// The distinction is the residency doctrine: a relocated class binding is where
// a row LIVES, and gc.root_store_ref is who OWNS it. A rig-owned bead resident
// in the city's binding has its commits on the RIG's checkout, so asking the
// city repo returns "not reachable" — a false refusal under enforcement.
func TestRepoDirFor(t *testing.T) {
	scopes := fixedScopeDirs{
		city: "/city",
		rigs: map[string]string{
			"frontend": "/city/rigs/frontend",
			"pathless": "",
		},
	}

	tests := []struct {
		name     string
		scopes   ScopeDirs
		metadata map[string]string
		wantDir  string
		wantKind ScopeKind
	}{
		{
			name: "a recorded work dir outranks the owning scope",
			metadata: map[string]string{
				beadmeta.WorkDirMetadataKey:      "/work/elsewhere",
				beadmeta.RootStoreRefMetadataKey: "rig:frontend",
				beadmeta.WorkOutcomeMetadataKey:  beadmeta.WorkOutcomeShipped,
				beadmeta.WorkBranchMetadataKey:   "main",
			},
			wantDir:  "/work/elsewhere",
			wantKind: ScopeWorkDir,
		},
		{
			name:     "a rig-owned bead answers to the rig checkout",
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:frontend"},
			wantDir:  "/city/rigs/frontend",
			wantKind: ScopeRig,
		},
		{
			name:     "a city-owned bead answers to the city checkout",
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "city:test-city"},
			wantDir:  "/city",
			wantKind: ScopeCity,
		},
		{
			// A binding is city scope: it serves the whole city's relocated
			// classes and belongs to no rig, so a bead whose OWNER is the
			// binding answers exactly as a city-owned one does.
			name:     "a binding-owned bead is city scope",
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "class:gmnos"},
			wantDir:  "/city",
			wantKind: ScopeCity,
		},
		{
			// Never the city. Falling back would ask about a repository that is
			// not the bead's, and under enforcement that is a false refusal
			// rather than a degraded clause.
			name:     "a rig this plane does not configure is unknown, not the city",
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:ghost"},
			wantDir:  "",
			wantKind: ScopeUnknown,
		},
		{
			name:     "a configured rig that names no checkout is unknown, not the city",
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:pathless"},
			wantDir:  "",
			wantKind: ScopeUnknown,
		},
		{
			name:     "a city-owned bead in a plane with no city checkout is unknown",
			scopes:   fixedScopeDirs{},
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "city:test-city"},
			wantDir:  "",
			wantKind: ScopeUnknown,
		},
		{
			// The compatibility arm. A city that never stamped a root ref —
			// every single-store city before the residency census — keeps the
			// answer its door already gave, byte for byte.
			name:     "a bead with no owner leaves the answer to the caller",
			metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped},
			wantDir:  "",
			wantKind: ScopeUnrooted,
		},
		{
			name:     "a legacy bare label is not an owner",
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "frontend"},
			wantDir:  "",
			wantKind: ScopeUnrooted,
		},
		{
			name:     "a rig ref with no rig name is not an owner",
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:"},
			wantDir:  "",
			wantKind: ScopeUnrooted,
		},
		{
			name:     "surrounding whitespace does not hide the owner",
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "  rig:frontend  "},
			wantDir:  "/city/rigs/frontend",
			wantKind: ScopeRig,
		},
		{
			name:     "a bead with no metadata at all leaves the answer to the caller",
			wantDir:  "",
			wantKind: ScopeUnrooted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dirs := tc.scopes
			if dirs == nil {
				dirs = scopes
			}
			bead := beads.Bead{Type: "task", Metadata: beads.StringMap(tc.metadata)}
			gotDir, gotKind := RepoDirFor(bead, dirs)
			if gotDir != tc.wantDir || gotKind != tc.wantKind {
				t.Fatalf("RepoDirFor = (%q, %s), want (%q, %s)", gotDir, gotKind, tc.wantDir, tc.wantKind)
			}
		})
	}
}

// TestRepoDirForWithoutScopeDirs pins the defensive arm: a caller that hands no
// checkout table at all gets "unknown" rather than a panic. The rule runs on
// both close doors, and a nil table is a caller bug that must not take the
// close down with it.
func TestRepoDirForWithoutScopeDirs(t *testing.T) {
	rooted := beads.Bead{Type: "task", Metadata: beads.StringMap{beadmeta.RootStoreRefMetadataKey: "rig:frontend"}}
	if dir, kind := RepoDirFor(rooted, nil); dir != "" || kind != ScopeUnknown {
		t.Fatalf("RepoDirFor(rig-owned, nil) = (%q, %s), want (\"\", %s)", dir, kind, ScopeUnknown)
	}
	recorded := beads.Bead{Type: "task", Metadata: beads.StringMap{beadmeta.WorkDirMetadataKey: "/work/here"}}
	if dir, kind := RepoDirFor(recorded, nil); dir != "/work/here" || kind != ScopeWorkDir {
		t.Fatalf("RepoDirFor(work_dir, nil) = (%q, %s), want (\"/work/here\", %s)", dir, kind, ScopeWorkDir)
	}
}
