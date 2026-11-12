package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestRigFederationGuardedFailsClosed(t *testing.T) {
	cases := []struct {
		federation string
		guarded    bool
	}{
		{"", false},
		{"  ", false},
		{"city", false},
		{" City ", false},
		{"CITY", false},
		{"rig", true},
		{"RIG", true},
		{" rig ", true},
		// Out-of-enum values never fail open.
		{"bogus", true},
		{"cty", true},
		{"none", true},
		{"off", true},
	}
	for _, tc := range cases {
		r := Rig{Name: "beta", Federation: tc.federation}
		if got := r.FederationGuarded(); got != tc.guarded {
			t.Errorf("Rig{Federation: %q}.FederationGuarded() = %v, want %v", tc.federation, got, tc.guarded)
		}
	}
}

func TestRigEffectiveFederationAndNamespace(t *testing.T) {
	cases := []struct {
		federation, namespace         string
		wantFederation, wantNamespace string
		wantExclusive                 bool
	}{
		{"", "", RigFederationCity, RigNamespaceShared, false},
		{"city", "shared", RigFederationCity, RigNamespaceShared, false},
		{" rig ", " exclusive ", RigFederationRig, RigNamespaceExclusive, true},
		{"rig", "", RigFederationRig, RigNamespaceShared, false},
	}
	for _, tc := range cases {
		r := Rig{Name: "beta", Federation: tc.federation, Namespace: tc.namespace}
		if got := r.EffectiveFederation(); got != tc.wantFederation {
			t.Errorf("EffectiveFederation(%q) = %q, want %q", tc.federation, got, tc.wantFederation)
		}
		if got := r.EffectiveNamespace(); got != tc.wantNamespace {
			t.Errorf("EffectiveNamespace(%q) = %q, want %q", tc.namespace, got, tc.wantNamespace)
		}
		if got := r.NamespaceExclusive(); got != tc.wantExclusive {
			t.Errorf("NamespaceExclusive(%q) = %v, want %v", tc.namespace, got, tc.wantExclusive)
		}
	}
}

func TestRigIdentityNamespace(t *testing.T) {
	cases := map[string]string{
		"beta":   "beta/",
		"my-rig": "my_rig/",
		"my_rig": "my_rig/",
		"Beta":   "Beta/",
		"":       "",
	}
	for name, want := range cases {
		r := Rig{Name: name}
		if got := r.IdentityNamespace(); got != want {
			t.Errorf("Rig{Name: %q}.IdentityNamespace() = %q, want %q", name, got, want)
		}
	}
}

func TestRigAdmitsLexically(t *testing.T) {
	guarded := Rig{Name: "beta", Federation: RigFederationRig}
	cases := []struct {
		identity string
		admitted bool
	}{
		{"beta/pk.worker", true},
		{"beta/pk.worker-2", true},
		{"beta--pk__worker", true},
		{"beta/core.control-dispatcher", true},
		{"  beta/pk.worker  ", true},
		{"beta/", false},
		{"beta--", false},
		{" beta/ ", false},
		{"beta/x", true},
		{"pk.worker", false},
		{"someone", false},
		{"alpha/pk.worker", false},
		{"Beta/pk.worker", false},
		{"BETA--pk__worker", false},
		{"beta", false},
		{"beta_pk.worker", false},
		{"beta__pk.worker", false},
		{"betamax/pk.worker", false},
		{"alpha/beta/pk.worker", false},
		{"gcs-17", false},
		{"xx-wisp-w7w7v0", false},
		{"", false},
		{"   ", false},
	}
	for _, tc := range cases {
		if got := guarded.AdmitsLexically(tc.identity); got != tc.admitted {
			t.Errorf("rig beta (federation = rig).AdmitsLexically(%q) = %v, want %v", tc.identity, got, tc.admitted)
		}
	}

	hyphenated := Rig{Name: "my-rig", Federation: RigFederationRig}
	for identity, want := range map[string]bool{
		"my-rig/pk.worker":    true,
		"my_rig/pk.worker":    true,
		"my.rig/pk.worker":    true,
		"my-rig--pk__worker":  true,
		"my--rig/pk.worker":   false,
		"myrig/pk.worker":     false,
		"my-rig-2/pk.worker":  false,
		"other/my-rig/worker": false,
	} {
		if got := hyphenated.AdmitsLexically(identity); got != want {
			t.Errorf("rig my-rig (federation = rig).AdmitsLexically(%q) = %v, want %v", identity, got, want)
		}
	}

	unguarded := Rig{Name: "beta"}
	for _, identity := range []string{"", "pk.worker", "alpha/pk.worker", "beta/pk.worker"} {
		if !unguarded.AdmitsLexically(identity) {
			t.Errorf("unguarded rig must admit %q", identity)
		}
	}

	nameless := Rig{Federation: RigFederationRig}
	for _, identity := range []string{"", "/", "/x", "x"} {
		if nameless.AdmitsLexically(identity) {
			t.Errorf("guarded rig without a name must not admit %q", identity)
		}
	}

	bogus := Rig{Name: "beta", Federation: "bogus"}
	if bogus.AdmitsLexically("pk.worker") {
		t.Error("an out-of-enum federation must fail closed and refuse identities outside the namespace")
	}
}

func TestValidateRigFederation(t *testing.T) {
	cases := []struct {
		name    string
		rigs    []Rig
		wantErr string
	}{
		{name: "unset", rigs: []Rig{{Name: "alpha"}, {Name: "beta"}}},
		{name: "explicit defaults", rigs: []Rig{{Name: "beta", Federation: "city", Namespace: "shared"}}},
		{name: "guarded", rigs: []Rig{{Name: "beta", Federation: "rig"}}},
		{name: "guarded exclusive", rigs: []Rig{{Name: "beta", Federation: "rig", Namespace: "exclusive"}}},
		{name: "guarded shared", rigs: []Rig{{Name: "beta", Federation: "rig", Namespace: "shared"}}},
		{name: "guarded hyphen and underscore", rigs: []Rig{{Name: "my-rig_2", Federation: "rig"}}},
		{name: "guarded triple hyphen folds to one segment", rigs: []Rig{{Name: "a---b", Federation: "rig"}}},
		{name: "guarded single-character name", rigs: []Rig{{Name: "b", Federation: "rig"}}},
		{name: "two distinct guarded rigs", rigs: []Rig{{Name: "alpha", Federation: "rig"}, {Name: "beta", Federation: "rig"}}},
		{name: "prefix that is not a namespace prefix", rigs: []Rig{{Name: "beta", Federation: "rig"}, {Name: "betamax"}}},
		{name: "unguarded twins are not this check's concern", rigs: []Rig{{Name: "my-rig"}, {Name: "my_rig"}}},
		{name: "unguarded multi-segment name", rigs: []Rig{{Name: "a--b"}}},

		{
			name:    "unknown federation",
			rigs:    []Rig{{Name: "beta", Federation: "bogus"}},
			wantErr: `rig "beta": federation "bogus" is not one of "city", "rig"`,
		},
		{
			name:    "federation is exact",
			rigs:    []Rig{{Name: "beta", Federation: "Rig"}},
			wantErr: `rig "beta": federation "Rig" is not one of "city", "rig"`,
		},
		{
			name:    "unknown namespace",
			rigs:    []Rig{{Name: "beta", Federation: "rig", Namespace: "private"}},
			wantErr: `rig "beta": namespace "private" is not one of "shared", "exclusive"`,
		},
		{
			name:    "exclusive without federation",
			rigs:    []Rig{{Name: "beta", Namespace: "exclusive"}},
			wantErr: `rig "beta": namespace = "exclusive" requires federation = "rig"`,
		},
		{
			name:    "exclusive with city federation",
			rigs:    []Rig{{Name: "beta", Federation: "city", Namespace: "exclusive"}},
			wantErr: `rig "beta": namespace = "exclusive" requires federation = "rig"`,
		},
		{
			name:    "double hyphen nests the namespace",
			rigs:    []Rig{{Name: "team--beta", Federation: "rig"}},
			wantErr: `rig "team--beta": federation = "rig" needs a one-segment rig name (letters, digits, '-', '_', starting and ending with a letter or digit, no "--"): the name is the identity namespace "team/beta/"`,
		},
		{
			name:    "dotted name",
			rigs:    []Rig{{Name: "team.beta", Federation: "rig"}},
			wantErr: `rig "team.beta": federation = "rig" needs a one-segment rig name`,
		},
		{
			name:    "slash in name",
			rigs:    []Rig{{Name: "team/beta", Federation: "rig"}},
			wantErr: `rig "team/beta": federation = "rig" needs a one-segment rig name`,
		},
		{
			name:    "leading separator",
			rigs:    []Rig{{Name: "-beta", Federation: "rig"}},
			wantErr: `rig "-beta": federation = "rig" needs a one-segment rig name`,
		},
		{
			name:    "trailing hyphen",
			rigs:    []Rig{{Name: "beta-", Federation: "rig"}},
			wantErr: `rig "beta-": federation = "rig" needs a one-segment rig name`,
		},
		{
			name:    "trailing underscore",
			rigs:    []Rig{{Name: "beta_", Federation: "rig"}},
			wantErr: `rig "beta_": federation = "rig" needs a one-segment rig name`,
		},
		{
			name:    "surrounding space",
			rigs:    []Rig{{Name: " beta", Federation: "rig"}},
			wantErr: `rig " beta": federation = "rig" needs a one-segment rig name`,
		},
		{
			name:    "empty name",
			rigs:    []Rig{{Federation: "rig"}},
			wantErr: `rig "": federation = "rig" needs a one-segment rig name`,
		},
		{
			name:    "separator twins",
			rigs:    []Rig{{Name: "my-rig", Federation: "rig"}, {Name: "my_rig"}},
			wantErr: `rig "my-rig": federation = "rig" namespace "my_rig/" collides with rig "my_rig": names that differ only in '.', '-', '_' or letter case are one namespace to the bead store`,
		},
		{
			name:    "case twins",
			rigs:    []Rig{{Name: "beta"}, {Name: "Beta", Federation: "rig"}},
			wantErr: `rig "Beta": federation = "rig" namespace "Beta/" collides with rig "beta"`,
		},
		{
			name:    "dotted twin",
			rigs:    []Rig{{Name: "my_rig", Federation: "rig"}, {Name: "my.rig"}},
			wantErr: `collides with rig "my.rig"`,
		},
		{
			name:    "twin that differs only in surrounding space",
			rigs:    []Rig{{Name: "beta", Federation: "rig"}, {Name: " beta"}},
			wantErr: `rig "beta": federation = "rig" namespace "beta/" collides with rig " beta"`,
		},
		{
			name:    "rig nested inside the namespace",
			rigs:    []Rig{{Name: "beta", Federation: "rig"}, {Name: "beta--x"}},
			wantErr: `rig "beta": federation = "rig" namespace "beta/" collides with rig "beta--x"`,
		},
		{
			name:    "both guarded twins",
			rigs:    []Rig{{Name: "my-rig", Federation: "rig"}, {Name: "MY_RIG", Federation: "rig"}},
			wantErr: `rig "my-rig": federation = "rig" namespace "my_rig/" collides with rig "MY_RIG"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRigFederation(tc.rigs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateRigFederation: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRigFederation: want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateRigFederation error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParseRefusesInvalidRigFederation(t *testing.T) {
	_, err := Parse([]byte(`
[workspace]
name = "test"

[[rigs]]
name = "beta"
path = "/srv/beta"
federation = "bogus"
`))
	if err == nil || !strings.Contains(err.Error(), `rig "beta": federation "bogus" is not one of "city", "rig"`) {
		t.Fatalf("Parse error = %v, want the federation enum error", err)
	}
}

func TestParseKeepsRigFederation(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test"

[[rigs]]
name = "beta"
path = "/srv/beta"
federation = "rig"
namespace = "exclusive"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Rigs[0]; got.Federation != "rig" || got.Namespace != "exclusive" || !got.FederationGuarded() {
		t.Fatalf("rig = %+v, want federation rig, namespace exclusive", got)
	}
}

func TestLoadWithIncludesRefusesInvalidRigFederation(t *testing.T) {
	cases := map[string]string{
		`federation = "bogus"`:    `rig "beta": federation "bogus" is not one of "city", "rig"`,
		`namespace = "exclusive"`: `rig "beta": namespace = "exclusive" requires federation = "rig"`,
	}
	for setting, wantErr := range cases {
		fs := fsys.NewFake()
		fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[[rigs]]
name = "beta"
path = "/srv/beta"
` + setting + "\n")
		_, _, err := LoadWithIncludes(fs, "/city/city.toml")
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("LoadWithIncludes with %s: error = %v, want it to contain %q", setting, err, wantErr)
		}
	}
}

// A fragment can contribute rigs the root layer's Parse never saw, so the
// composed root is validated again.
func TestLoadWithIncludesRefusesRigFederationCollisionAcrossFragments(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["rigs.toml"]

[workspace]
name = "test"

[[rigs]]
name = "my-rig"
path = "/srv/one"
federation = "rig"
`)
	fs.Files["/city/rigs.toml"] = []byte(`
[[rigs]]
name = "my_rig"
path = "/srv/two"
`)
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil || !strings.Contains(err.Error(), `collides with rig "my_rig"`) {
		t.Fatalf("LoadWithIncludes error = %v, want the namespace collision", err)
	}
}

func TestLoadWithIncludesKeepsRigFederation(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[[rigs]]
name = "beta"
path = "/srv/beta"
federation = "rig"
namespace = "exclusive"
`)
	cfg, prov, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].Federation != "rig" || cfg.Rigs[0].Namespace != "exclusive" {
		t.Fatalf("rigs = %+v, want beta with federation rig, namespace exclusive", cfg.Rigs)
	}
	for _, w := range prov.Warnings {
		if strings.Contains(w, "federation") || strings.Contains(w, "namespace") {
			t.Errorf("unexpected load warning about the federation keys: %q", w)
		}
	}
}

// The keys are deliberately not patchable, and a [[patches.rigs]] entry
// setting them is refused rather than silently dropped.
func TestRigFederationIsNotPatchable(t *testing.T) {
	for _, key := range []string{`federation = "city"`, `namespace = "shared"`, `Federation = "city"`, `NAMESPACE = "shared"`} {
		fs := fsys.NewFake()
		fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[[rigs]]
name = "beta"
path = "/srv/beta"
federation = "rig"

[[patches.rigs]]
name = "beta"
` + key + "\n")
		_, _, err := LoadWithIncludes(fs, "/city/city.toml")
		field := strings.Fields(key)[0]
		want := `[[patches.rigs]] cannot set "` + field + `"; set it on the rig's own [[rigs]] entry`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadWithIncludes with patches.rigs %s: error = %v, want it to contain %q", field, err, want)
		}
	}
}

func TestValidateRigFederationLayout(t *testing.T) {
	root := t.TempDir()
	city := filepath.Join(root, "city")
	alphaDir := filepath.Join(root, "alpha")
	betaDir := filepath.Join(root, "beta")
	for _, d := range []string{city, alphaDir, betaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	guardedBeta := func() Rig { return Rig{Name: "beta", Path: betaDir, Federation: RigFederationRig} }
	alpha := Rig{Name: "alpha", Path: alphaDir}

	cases := []struct {
		name    string
		cfg     *City
		wantErr string
	}{
		{name: "nil config", cfg: nil},
		{
			name: "guarded rig with its own directory and agents attached by name",
			cfg: func() *City {
				beta := guardedBeta()
				beta.DefaultSlingTarget = "beta/pk.worker"
				beta.DefaultSlingTargets = []string{"beta/pk.worker", "beta--pk__reviewer", "pk.worker"}
				return &City{
					Rigs: []Rig{alpha, beta},
					Agents: []Agent{
						{Name: "worker", BindingName: "pk", Dir: "beta"},
						{Name: "worker", BindingName: "pk", Dir: "alpha"},
						{Name: "worker", BindingName: "pk"},
						{Name: "beta-helper"},
					},
				}
			}(),
		},
		{
			name: "unguarded rigs may share a directory",
			cfg:  &City{Rigs: []Rig{{Name: "a", Path: alphaDir}, {Name: "b", Path: alphaDir}}},
		},
		{
			name:    "structural rules run first",
			cfg:     &City{Rigs: []Rig{{Name: "beta", Path: betaDir, Federation: "bogus"}}},
			wantErr: `rig "beta": federation "bogus" is not one of "city", "rig"`,
		},
		{
			name:    "guarded rig shares another rig's directory",
			cfg:     &City{Rigs: []Rig{guardedBeta(), {Name: "alpha", Path: betaDir}}},
			wantErr: `rig "beta": federation = "rig" needs its own store directory; path "` + betaDir + `" is also rig "alpha"'s`,
		},
		{
			name:    "guarded rig shares a directory through a relative path",
			cfg:     &City{Rigs: []Rig{guardedBeta(), {Name: "alpha", Path: "../beta/"}}},
			wantErr: `is also rig "alpha"'s`,
		},
		{
			name:    "guarded rig at the city root",
			cfg:     &City{Rigs: []Rig{{Name: "beta", Path: city, Federation: RigFederationRig}}},
			wantErr: `rig "beta": federation = "rig" needs its own store directory; path "` + city + `" is the city root`,
		},
		{
			name:    "guarded rig at the city root through a relative path",
			cfg:     &City{Rigs: []Rig{{Name: "beta", Path: ".", Federation: RigFederationRig}}},
			wantErr: `is the city root`,
		},
		{
			name: "agent attached by path",
			cfg: &City{
				Rigs:   []Rig{alpha, guardedBeta()},
				Agents: []Agent{{Name: "worker", BindingName: "pk", Dir: betaDir}},
			},
			wantErr: `agent "` + betaDir + `/pk.worker": attach it to rig "beta" by name (dir = "beta"): federation = "rig" admits only "beta/" identities`,
		},
		{
			name: "agent attached by relative path",
			cfg: &City{
				Rigs:   []Rig{alpha, guardedBeta()},
				Agents: []Agent{{Name: "worker", Dir: "../beta"}},
			},
			wantErr: `agent "../beta/worker": attach it to rig "beta" by name`,
		},
		{
			name: "city agent whose identity folds into the namespace",
			cfg: &City{
				Rigs:   []Rig{guardedBeta()},
				Agents: []Agent{{Name: "beta--worker"}},
			},
			wantErr: `agent "beta--worker" reads as "beta/worker" to the bead store, inside rig "beta"'s namespace "beta/" (federation = "rig"); rename it or attach it to the rig`,
		},
		{
			name: "agent in a non-rig dir that folds into the namespace",
			cfg: &City{
				Rigs:   []Rig{guardedBeta()},
				Agents: []Agent{{Name: "x", Dir: "beta--sub"}},
			},
			wantErr: `agent "beta--sub/x" reads as "beta/sub/x"`,
		},
		{
			name: "pool slot of an agent whose name ends in a hyphen",
			cfg: &City{
				Rigs:   []Rig{guardedBeta()},
				Agents: []Agent{{Name: "beta-"}},
			},
			wantErr: `agent "beta-": identity "beta--1" reads as "beta/1" to the bead store, inside rig "beta"'s namespace "beta/"`,
		},
		{
			name: "pool session base name of an agent in another rig",
			cfg: &City{
				Rigs:   []Rig{alpha, guardedBeta()},
				Agents: []Agent{{Name: "beta-", Dir: "alpha"}},
			},
			wantErr: `agent "alpha/beta-": identity "beta--1" reads as "beta/1" to the bead store, inside rig "beta"'s namespace "beta/"`,
		},
		{
			name: "namepool instance name that folds into the namespace",
			cfg: &City{
				Rigs:   []Rig{guardedBeta()},
				Agents: []Agent{{Name: "worker", NamepoolNames: []string{"ann", "beta--bob"}}},
			},
			wantErr: `agent "worker": identity "beta--bob" reads as "beta/bob"`,
		},
		{
			name: "namepool instance name of a rig agent is the rig's own",
			cfg: &City{
				Rigs:   []Rig{guardedBeta()},
				Agents: []Agent{{Name: "worker", Dir: "beta", NamepoolNames: []string{"beta--bob"}}},
			},
		},
		{
			name: "city named session that folds into the namespace",
			cfg: &City{
				Rigs:          []Rig{guardedBeta()},
				Agents:        []Agent{{Name: "lead"}},
				NamedSessions: []NamedSession{{Name: "beta--lead", Template: "lead"}},
			},
			wantErr: `named session "beta--lead" reads as "beta/lead" to the bead store, inside rig "beta"'s namespace "beta/"`,
		},
		{
			name: "rig named session is the rig's own",
			cfg: &City{
				Rigs:          []Rig{guardedBeta()},
				Agents:        []Agent{{Name: "lead", Dir: "beta"}},
				NamedSessions: []NamedSession{{Name: "lead", Template: "lead", Dir: "beta"}},
			},
		},
		{
			name: "agent name with surrounding space that folds into the namespace",
			cfg: &City{
				Rigs:   []Rig{guardedBeta()},
				Agents: []Agent{{Name: " beta--worker"}},
			},
			wantErr: `reads as "beta/worker"`,
		},
		{
			name: "qualified default sling target outside the namespace",
			cfg: func() *City {
				beta := guardedBeta()
				beta.DefaultSlingTarget = "alpha/pk.worker"
				return &City{Rigs: []Rig{alpha, beta}}
			}(),
			wantErr: `rig "beta": default_sling_target "alpha/pk.worker" is outside its namespace "beta/" (federation = "rig")`,
		},
		{
			name: "qualified default sling targets entry outside the namespace",
			cfg: func() *City {
				beta := guardedBeta()
				beta.DefaultSlingTargets = []string{"beta/pk.worker", "alpha--pk__worker"}
				return &City{Rigs: []Rig{alpha, beta}}
			}(),
			wantErr: `rig "beta": default_sling_target "alpha--pk__worker" is outside its namespace "beta/"`,
		},
		{
			name: "default sling target that is the bare namespace",
			cfg: func() *City {
				beta := guardedBeta()
				beta.DefaultSlingTarget = "beta--"
				return &City{Rigs: []Rig{beta}}
			}(),
			wantErr: `rig "beta": default_sling_target "beta--" names no agent inside its namespace "beta/" (federation = "rig")`,
		},
		{
			name: "unguarded rig may target anything",
			cfg: &City{Rigs: []Rig{alpha, {
				Name: "beta", Path: betaDir, DefaultSlingTarget: "alpha/pk.worker",
			}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRigFederationLayout(city, tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateRigFederationLayout: unexpected error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRigFederationLayout: want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateRigFederationLayout error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateRigFederationLayoutResolvesSymlinkedRigPaths(t *testing.T) {
	root := t.TempDir()
	betaDir := filepath.Join(root, "beta")
	if err := os.MkdirAll(betaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "beta-link")
	if err := os.Symlink(betaDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := &City{Rigs: []Rig{
		{Name: "beta", Path: betaDir, Federation: RigFederationRig},
		{Name: "alpha", Path: link},
	}}
	err := ValidateRigFederationLayout(filepath.Join(root, "city"), cfg)
	if err == nil || !strings.Contains(err.Error(), `is also rig "alpha"'s`) {
		t.Fatalf("ValidateRigFederationLayout error = %v, want the shared-directory error", err)
	}
}

func TestRigFederationWarnings(t *testing.T) {
	if got := RigFederationWarnings([]Rig{{Name: "alpha"}, {Name: "beta", Federation: "city"}}); len(got) != 0 {
		t.Fatalf("RigFederationWarnings for unguarded rigs = %v, want none", got)
	}
	got := RigFederationWarnings([]Rig{
		{Name: "alpha"},
		{Name: "beta", Federation: RigFederationRig, Namespace: RigNamespaceExclusive},
	})
	if len(got) != 1 {
		t.Fatalf("RigFederationWarnings = %v, want one warning", got)
	}
	for _, want := range []string{`rig "beta"`, `federation = "rig"`, "does not enforce", "NOT protected", "do not attach a bead store that another city also uses"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not contain %q", got[0], want)
		}
	}
}
