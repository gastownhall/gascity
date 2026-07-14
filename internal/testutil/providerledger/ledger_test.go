package providerledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateRejectsInvalidContractClaims(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	validWaiver := &Waiver{
		Owner:   "ga-80po0c.3",
		Expires: now.Add(30 * 24 * time.Hour),
		Reason:  "tracked legacy contract gap",
	}

	tests := []struct {
		name  string
		claim ContractClaim
		want  string
	}{
		{
			name: "waived contract has no waiver",
			claim: ContractClaim{
				Contract:    ContractRuntimeProvider,
				Disposition: DispositionWaived,
			},
			want: "waived claim requires a waiver",
		},
		{
			name: "waiver also has not-applicable reason",
			claim: ContractClaim{
				Contract:            ContractRuntimeProvider,
				Disposition:         DispositionWaived,
				Waiver:              validWaiver,
				NotApplicableReason: "faulting provider",
			},
			want: "exactly one of waiver or not-applicable reason",
		},
		{
			name: "waiver is expired",
			claim: ContractClaim{
				Contract:    ContractRuntimeProvider,
				Disposition: DispositionWaived,
				Waiver: &Waiver{
					Owner:   "ga-80po0c.3",
					Expires: now.Add(-time.Hour),
					Reason:  "expired gap",
				},
			},
			want: "waiver owned by ga-80po0c.3 expired",
		},
		{
			name: "waiver has no owner",
			claim: ContractClaim{
				Contract:    ContractRuntimeProvider,
				Disposition: DispositionWaived,
				Waiver: &Waiver{
					Expires: now.Add(30 * 24 * time.Hour),
					Reason:  "owner omitted",
				},
			},
			want: "waiver owner is required",
		},
		{
			name: "waiver exceeds bounded horizon",
			claim: ContractClaim{
				Contract:    ContractRuntimeProvider,
				Disposition: DispositionWaived,
				Waiver: &Waiver{
					Owner:   "ga-80po0c.3",
					Expires: now.Add(maxWaiverHorizon + time.Hour),
					Reason:  "parked gap",
				},
			},
			want: "waiver owned by ga-80po0c.3 exceeds",
		},
		{
			name: "not applicable has no reason",
			claim: ContractClaim{
				Contract:    ContractRuntimeProvider,
				Disposition: DispositionNotApplicable,
			},
			want: "not-applicable claim requires a reason",
		},
		{
			name: "not applicable also has waiver",
			claim: ContractClaim{
				Contract:            ContractRuntimeProvider,
				Disposition:         DispositionNotApplicable,
				Waiver:              validWaiver,
				NotApplicableReason: "faulting provider",
			},
			want: "exactly one of waiver or not-applicable reason",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := validRuntimeEntry("runtime.fixture", "exact:fixture", tt.claim)
			err := Validate([]Entry{entry}, now)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateRequiresEveryPortContract(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	entry := validRuntimeEntry("runtime.fixture", "exact:fixture", ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	})
	entry.Claims = nil

	err := Validate([]Entry{entry}, now)
	if err == nil || !strings.Contains(err.Error(), "missing required contract runtime.Provider") {
		t.Fatalf("Validate() error = %v, want missing required contract", err)
	}
}

func TestValidateRejectsUnknownCatalogName(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	entry := validRuntimeEntry("runtime.fixture", "exact:fixture", ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	})
	entry.Catalog.Name = "runtime.typo"

	err := Validate([]Entry{entry}, now)
	if err == nil || !strings.Contains(err.Error(), "unknown catalog") {
		t.Fatalf("Validate() error = %v, want unknown-catalog error", err)
	}
}

func TestValidateRequiresProductionRoleForDiscoveryBindings(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	claim := ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	}

	tests := []struct {
		name  string
		entry Entry
	}{
		{
			name: "catalog",
			entry: func() Entry {
				entry := validRuntimeEntry("runtime.catalog", "exact:fixture", claim)
				entry.Roles = []Role{RoleReusableDouble}
				return entry
			}(),
		},
		{
			name: "source",
			entry: func() Entry {
				entry := validRuntimeEntry("runtime.source", "exact:fixture", claim)
				entry.Roles = []Role{RoleReusableDouble}
				entry.Catalog = nil
				entry.Source = &SourceRef{File: "fixture.go", Function: "newFixture", Reason: "fixture"}
				return entry
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate([]Entry{tt.entry}, now)
			if err == nil || !strings.Contains(err.Error(), "discovery binding requires role production_provider") {
				t.Fatalf("Validate() error = %v, want production-role error", err)
			}
		})
	}
}

func TestValidateRequiresReusableDoubleTypeWithReusableRole(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)

	t.Run("role without type", func(t *testing.T) {
		entry := reusableRuntimeEntry("runtime.fake", "exact:fake", "Fake", "NewFake")
		entry.DoubleType = nil
		err := Validate([]Entry{entry}, now)
		if err == nil || !strings.Contains(err.Error(), "reusable_double role requires a double type") {
			t.Fatalf("Validate() error = %v, want missing-double-type error", err)
		}
	})

	t.Run("role without boundary", func(t *testing.T) {
		entry := reusableRuntimeEntry("runtime.fake", "exact:fake", "Fake", "NewFake")
		entry.DoubleBoundary = ""
		err := Validate([]Entry{entry}, now)
		if err == nil || !strings.Contains(err.Error(), "reusable_double role requires a repository-relative double boundary") {
			t.Fatalf("Validate() error = %v, want missing-double-boundary error", err)
		}
	})

	t.Run("type without role", func(t *testing.T) {
		entry := reusableRuntimeEntry("runtime.fake", "exact:fake", "Fake", "NewFake")
		entry.Roles = []Role{RoleProductionProvider}
		err := Validate([]Entry{entry}, now)
		if err == nil || !strings.Contains(err.Error(), "double type requires role reusable_double") {
			t.Fatalf("Validate() error = %v, want missing-reusable-role error", err)
		}
	})
}

func TestRenderMarkdownShowsReusableOnlyBoundary(t *testing.T) {
	entry := reusableRuntimeEntry("runtime.double.gated", "unused", "GatedFake", "NewGatedFake")
	entry.Roles = []Role{RoleReusableDouble}
	entry.Catalog = nil

	if err := Validate([]Entry{entry}, time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Validate(reusable-only entry): %v", err)
	}
	got := RenderMarkdown([]Entry{entry})
	if !strings.Contains(got, "reusable: internal/runtime/fake.go") || strings.Contains(got, "invalid: no discovery binding") {
		t.Fatalf("RenderMarkdown(reusable-only entry) = %q, want honest reusable boundary", got)
	}
}

func TestValidateRejectsDuplicateSourceBindings(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	claim := ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	}
	first := validRuntimeEntry("runtime.source.first", "unused:first", claim)
	first.Catalog = nil
	first.Source = &SourceRef{File: "cmd/gc/providers.go", Function: "newFixture", Reason: "fixture"}
	second := validRuntimeEntry("runtime.source.second", "unused:second", claim)
	second.Catalog = nil
	second.Source = &SourceRef{File: "cmd/gc/./providers.go", Function: "newFixture", Reason: "same normalized source"}

	err := Validate([]Entry{first, second}, now)
	if err == nil || !strings.Contains(err.Error(), "source binding cmd/gc/providers.go#newFixture is also owned") {
		t.Fatalf("Validate() error = %v, want duplicate-source error", err)
	}
}

func TestValidateRejectsContractNotRequiredByPort(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	entry := validRuntimeEntry("runtime.fixture", "exact:fixture", ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	})
	entry.Claims = append(entry.Claims, ContractClaim{
		Constructor:         entry.Constructors[0],
		Contract:            ContractID("runtime.Unknown"),
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	})

	err := Validate([]Entry{entry}, now)
	if err == nil || !strings.Contains(err.Error(), "contract runtime.Unknown is not required by port runtime.Provider") {
		t.Fatalf("Validate() error = %v, want inapplicable-contract error", err)
	}
}

func TestValidateRequiresExactlyOneClaimPerConstructorContract(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	constructorA := SymbolRef{ImportPath: "example.test/provider", Name: "NewA"}
	constructorB := SymbolRef{ImportPath: "example.test/provider", Name: "NewB"}
	claim := func(constructor SymbolRef) ContractClaim {
		return ContractClaim{
			Constructor:         constructor,
			Contract:            ContractRuntimeProvider,
			Disposition:         DispositionNotApplicable,
			NotApplicableReason: "fixture",
		}
	}
	entry := Entry{
		ID:           "runtime.fixture",
		Roles:        []Role{RoleProductionProvider},
		Port:         PortRuntimeProvider,
		Constructors: []SymbolRef{constructorA, constructorB},
		Source: &SourceRef{
			File:     "fixture.go",
			Function: "newFixture",
			Reason:   "fixture",
		},
		Claims: []ContractClaim{claim(constructorA)},
	}

	t.Run("missing pair", func(t *testing.T) {
		err := Validate([]Entry{entry}, now)
		if err == nil || !strings.Contains(err.Error(), "constructor example.test/provider.NewB is missing required contract runtime.Provider") {
			t.Fatalf("Validate() error = %v, want missing constructor-contract pair", err)
		}
	})

	t.Run("duplicate pair", func(t *testing.T) {
		duplicate := entry
		duplicate.Claims = []ContractClaim{claim(constructorA), claim(constructorA), claim(constructorB)}
		err := Validate([]Entry{duplicate}, now)
		if err == nil || !strings.Contains(err.Error(), "constructor example.test/provider.NewA contract runtime.Provider is duplicated") {
			t.Fatalf("Validate() error = %v, want duplicate constructor-contract pair", err)
		}
	})

	t.Run("undeclared constructor", func(t *testing.T) {
		undeclared := entry
		undeclared.Claims = []ContractClaim{claim(constructorA), claim(constructorB), claim(SymbolRef{ImportPath: "example.test/provider", Name: "NewC"})}
		err := Validate([]Entry{undeclared}, now)
		if err == nil || !strings.Contains(err.Error(), "constructor example.test/provider.NewC is not declared by the entry") {
			t.Fatalf("Validate() error = %v, want undeclared-constructor error", err)
		}
	})
}

func TestCatalogDefersExactConstructorContractsToOwnedFollowups(t *testing.T) {
	want := map[string]bool{
		"runtime.builtin.fake/internal/runtime.NewFake":                               true,
		"runtime.builtin.subprocess/internal/runtime/subprocess.NewSeamBacked":        true,
		"runtime.builtin.subprocess/internal/runtime/subprocess.NewSeamBackedWithDir": true,
	}
	got := make(map[string]bool)

	for _, entry := range Catalog() {
		for _, claim := range entry.Claims {
			if claim.Waiver == nil || claim.Waiver.Owner != "ga-80po0c.1.2" {
				continue
			}
			key := entry.ID + "/" + renderSymbolRef(claim.Constructor)
			got[key] = true
			if claim.Disposition != DispositionWaived {
				t.Errorf("%s disposition = %q, want %q", key, claim.Disposition, DispositionWaived)
			}
			if claim.Contract != ContractRuntimeProvider {
				t.Errorf("%s contract = %q, want %q", key, claim.Contract, ContractRuntimeProvider)
			}
		}
	}

	if len(got) != len(want) {
		t.Fatalf("ga-80po0c.1.2 waiver rows = %v, want %v", got, want)
	}
	for key := range want {
		if !got[key] {
			t.Errorf("ga-80po0c.1.2 waiver row %s is missing", key)
		}
	}
}

func TestDiscoverRuntimeProviderDoublesUsesDeclaredPortIdentity(t *testing.T) {
	dir := writeRuntimeDoubleFixture(t, map[string]string{
		"runtime.go": `package runtime
type Provider interface { Run() }
`,
		"fake.go": `package runtime
type Fake struct{}
func (*Fake) Run() {}

type FakeAlias = Fake
type OtherFake Fake
type helper struct{}

func NewFake() *Fake { return nil }
func NewAlias() *FakeAlias { return nil }
func NewOther() *OtherFake { return nil }
func NewValue() Fake { return Fake{} }
func NewPair() (*Fake, error) { return nil, nil }
func newPrivate() *Fake { return nil }
func (helper) NewMethod() *Fake { return nil }
func NewShadow[Fake any]() *Fake { return nil }
func caller() {
	type Fake struct{}
	_ = func() *Fake { return nil }
}

type GatedFake struct{ *Fake }
func NewGatedFake() (*GatedFake, error) { return nil, nil }
func NewGatedValue() GatedFake { return GatedFake{} }

type Support struct{}
func NewSupport() *Support { return nil }
`,
		"constructors.go": `package runtime
func NewExternalFake() *Fake { return nil }
`,
	})

	got, err := DiscoverRuntimeProviderDoubles(dir)
	if err != nil {
		t.Fatalf("DiscoverRuntimeProviderDoubles: %v", err)
	}
	want := []ReusableDouble{
		{
			Type: repoSymbol("internal/runtime", "Fake"),
			Constructors: []SymbolRef{
				repoSymbol("internal/runtime", "NewAlias"),
				repoSymbol("internal/runtime", "NewExternalFake"),
				repoSymbol("internal/runtime", "NewFake"),
				repoSymbol("internal/runtime", "NewPair"),
			},
		},
		{
			Type: repoSymbol("internal/runtime", "GatedFake"),
			Constructors: []SymbolRef{
				repoSymbol("internal/runtime", "NewGatedFake"),
				repoSymbol("internal/runtime", "NewGatedValue"),
			},
		},
	}
	if gotText, wantText := renderReusableDoubles(got), renderReusableDoubles(want); gotText != wantText {
		t.Fatalf("doubles = %s, want %s", gotText, wantText)
	}
}

func TestDiscoverRuntimeProviderDoublesFailsClosed(t *testing.T) {
	const provider = `package runtime
type Provider interface { Run() }
`
	const validDouble = `package runtime
type Fake struct{}
func (*Fake) Run() {}
func NewFake() *Fake { return nil }
`

	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "boundary file renamed",
			files: map[string]string{"runtime.go": provider, "doubles.go": validDouble},
			want:  "designated runtime double boundary fake.go is missing",
		},
		{
			name:  "boundary package changed",
			files: map[string]string{"runtime.go": provider, "fake.go": strings.Replace(validDouble, "package runtime", "package other", 1)},
			want:  "fake.go must declare package runtime",
		},
		{
			name:  "provider declaration missing",
			files: map[string]string{"runtime.go": "package runtime\n", "fake.go": validDouble},
			want:  "runtime.Provider must be exactly one declared interface",
		},
		{
			name:  "provider is not an interface",
			files: map[string]string{"runtime.go": "package runtime\ntype Provider struct{}\n", "fake.go": validDouble},
			want:  "runtime.Provider must be exactly one declared interface",
		},
		{
			name:  "no exported provider double",
			files: map[string]string{"runtime.go": provider, "fake.go": "package runtime\ntype Support struct{}\n"},
			want:  "fake.go declares no exported runtime.Provider double",
		},
		{
			name: "provider double has no constructor",
			files: map[string]string{
				"runtime.go": provider,
				"fake.go":    "package runtime\ntype Fake struct{}\nfunc (*Fake) Run() {}\n",
			},
			want: "runtime provider double internal/runtime.Fake has no exported receiverless constructor",
		},
		{
			name: "declaration type error",
			files: map[string]string{
				"runtime.go": provider,
				"fake.go":    validDouble + "\nvar broken MissingType\n",
			},
			want: "type-check runtime double boundary",
		},
		{
			name: "generic boundary type",
			files: map[string]string{
				"runtime.go": provider,
				"fake.go": `package runtime
type GenericFake[T any] struct{}
func (*GenericFake[T]) Run() {}
func NewGenericFake[T any]() *GenericFake[T] { return nil }
`,
			},
			want: "generic exported type GenericFake in fake.go cannot be classified as a reusable provider double",
		},
		{
			name: "exported alias exposes untracked provider type",
			files: map[string]string{
				"runtime.go": provider,
				"fake.go": validDouble + `
type hiddenFake struct{ *Fake }
type GatedFake = hiddenFake
func NewGatedFake() *GatedFake { return nil }
`,
			},
			want: "exported provider alias GatedFake in fake.go resolves to an untracked concrete type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRuntimeDoubleFixture(t, tt.files)
			_, err := DiscoverRuntimeProviderDoubles(dir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DiscoverRuntimeProviderDoubles() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCompareReusableDoublesChecksConstructorsBothDirections(t *testing.T) {
	entries := []Entry{
		reusableRuntimeEntry("runtime.fake", "exact:fake", "Fake", "NewFake"),
		reusableRuntimeEntry("runtime.removed", "exact:removed", "Fake", "NewRemovedFake"),
	}
	discovered := []ReusableDouble{
		{
			Type: repoSymbol("internal/runtime", "Fake"),
			Constructors: []SymbolRef{
				repoSymbol("internal/runtime", "NewFake"),
				repoSymbol("internal/runtime", "NewFailFake"),
			},
		},
		{
			Type:         repoSymbol("internal/runtime", "GatedFake"),
			Constructors: []SymbolRef{repoSymbol("internal/runtime", "NewGatedFake")},
		},
	}

	err := CompareReusableDoubles(entries, discovered)
	if err == nil {
		t.Fatal("CompareReusableDoubles() succeeded, want missing and stale errors")
	}
	for _, want := range []string{
		"internal/runtime.NewFailFake is missing from the ledger",
		"internal/runtime.NewGatedFake is missing from the ledger",
		"internal/runtime.NewRemovedFake is not discovered for type boundary internal/runtime/fake.go",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CompareReusableDoubles() error = %v, want containing %q", err, want)
		}
	}
}

func TestCompareReusableDoublesBindsConstructorToDeclaredType(t *testing.T) {
	entry := reusableRuntimeEntry("runtime.fake", "exact:fake", "Fake", "NewFake")
	discovered := []ReusableDouble{{
		Type:         repoSymbol("internal/runtime", "RenamedFake"),
		Constructors: []SymbolRef{repoSymbol("internal/runtime", "NewFake")},
	}}

	err := CompareReusableDoubles([]Entry{entry}, discovered)
	if err == nil || !strings.Contains(err.Error(), "internal/runtime.NewFake constructs internal/runtime.RenamedFake, ledger declares internal/runtime.Fake") {
		t.Fatalf("CompareReusableDoubles() error = %v, want declared-type drift", err)
	}
}

func TestCompareReusableDoublesRequiresReusableRole(t *testing.T) {
	entry := reusableRuntimeEntry("runtime.fake", "exact:fake", "Fake", "NewFake")
	entry.Roles = []Role{RoleProductionProvider}
	discovered := []ReusableDouble{{
		Type:         repoSymbol("internal/runtime", "Fake"),
		Constructors: []SymbolRef{repoSymbol("internal/runtime", "NewFake")},
	}}

	err := CompareReusableDoubles([]Entry{entry}, discovered)
	if err == nil || !strings.Contains(err.Error(), "internal/runtime.NewFake is missing from the ledger") {
		t.Fatalf("CompareReusableDoubles() error = %v, want reusable-role error", err)
	}
}

func TestCompareReusableDoublesRequiresDesignatedBoundary(t *testing.T) {
	entry := reusableRuntimeEntry("runtime.fake", "exact:fake", "Fake", "NewFake")
	entry.DoubleBoundary = "internal/runtime/other.go"
	discovered := []ReusableDouble{{
		Type:         repoSymbol("internal/runtime", "Fake"),
		Constructors: []SymbolRef{repoSymbol("internal/runtime", "NewFake")},
	}}

	err := CompareReusableDoubles([]Entry{entry}, discovered)
	if err == nil || !strings.Contains(err.Error(), `reusable double boundary is "internal/runtime/other.go", want "internal/runtime/fake.go"`) {
		t.Fatalf("CompareReusableDoubles() error = %v, want designated-boundary error", err)
	}
}

func TestCompareReusableDoublesRejectsDuplicateOwnership(t *testing.T) {
	entries := []Entry{
		reusableRuntimeEntry("runtime.fake.first", "exact:first", "Fake", "NewFake"),
		reusableRuntimeEntry("runtime.fake.second", "exact:second", "Fake", "NewFake"),
	}
	discovered := []ReusableDouble{{
		Type:         repoSymbol("internal/runtime", "Fake"),
		Constructors: []SymbolRef{repoSymbol("internal/runtime", "NewFake")},
	}}

	err := CompareReusableDoubles(entries, discovered)
	if err == nil || !strings.Contains(err.Error(), `internal/runtime.NewFake is owned by multiple ledger entries: "runtime.fake.first", "runtime.fake.second"`) {
		t.Fatalf("CompareReusableDoubles() error = %v, want duplicate ownership", err)
	}
}

func TestDiscoverRuntimeCatalogIsBoundedToBuildRuntimeRegistry(t *testing.T) {
	source := []byte(`package main
import (
	registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
	runtimealias "github.com/gastownhall/gascity/internal/runtime"
	execalias "github.com/gastownhall/gascity/internal/runtime/exec"
)
func buildRuntimeRegistry() {
	r := registryalias.New()
	fakeFactory := func() { return runtimealias.NewFake(), nil }
	must(r.Register("fake", fakeFactory))
	must(r.RegisterPrefix("exec:", func() { return execalias.NewSeamBacked("provider"), nil }))
	r.SetFallback(fakeFactory)
	return r
}

func runtimeRegistryForCity() {
	_ = r.Register("pack-runtime", func() { return runtimealias.NewFake(), nil })
}`)

	got, err := DiscoverRuntimeCatalog(source)
	if err != nil {
		t.Fatalf("DiscoverRuntimeCatalog: %v", err)
	}
	want := []RuntimeRegistration{
		{
			Key:          "exact:fake",
			Constructors: []SymbolRef{{ImportPath: moduleImportPath + "/internal/runtime", Name: "NewFake"}},
		},
		{
			Key:          "prefix:exec:",
			Constructors: []SymbolRef{{ImportPath: moduleImportPath + "/internal/runtime/exec", Name: "NewSeamBacked"}},
		},
	}
	if gotText, wantText := renderRegistrations(got), renderRegistrations(want); gotText != wantText {
		t.Fatalf("catalog = %v, want %v", got, want)
	}
}

func TestDiscoverRuntimeCatalogRequiresOneReceiverlessFunction(t *testing.T) {
	const imports = `import (
	registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
	runtimealias "github.com/gastownhall/gascity/internal/runtime"
)`
	const decoy = `
type decoy struct{}
func (decoy) buildRuntimeRegistry() {
	r := registryalias.New()
	factory := func() { return runtimealias.NewFailFake(), nil }
	must(r.Register("decoy", factory))
	r.SetFallback(factory)
	return r
}`
	const production = `
func buildRuntimeRegistry() {
	r := registryalias.New()
	factory := func() { return runtimealias.NewFake(), nil }
	must(r.Register("fake", factory))
	r.SetFallback(factory)
	return r
}`

	t.Run("receiver method is ignored", func(t *testing.T) {
		got, err := DiscoverRuntimeCatalog([]byte("package main\n" + imports + decoy + production))
		if err != nil {
			t.Fatalf("DiscoverRuntimeCatalog: %v", err)
		}
		if len(got) != 1 || got[0].Key != "exact:fake" {
			t.Fatalf("catalog = %v, want receiverless exact:fake function", got)
		}
	})

	t.Run("receiver method alone is rejected", func(t *testing.T) {
		_, err := DiscoverRuntimeCatalog([]byte("package main\n" + imports + decoy))
		if err == nil || !strings.Contains(err.Error(), "exactly one receiverless top-level function") {
			t.Fatalf("DiscoverRuntimeCatalog() error = %v, want receiverless-cardinality error", err)
		}
	})

	t.Run("duplicate receiverless functions are rejected", func(t *testing.T) {
		_, err := DiscoverRuntimeCatalog([]byte("package main\n" + imports + production + production))
		if err == nil || !strings.Contains(err.Error(), "exactly one receiverless top-level function") {
			t.Fatalf("DiscoverRuntimeCatalog() error = %v, want receiverless-cardinality error", err)
		}
	})
}

func TestDiscoverRuntimeCatalogRejectsNonLiteralKeys(t *testing.T) {
	source := []byte(`package main
import registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
func buildRuntimeRegistry() {
	r := registryalias.New()
	must(r.Register(providerName, func() { return newProvider(), nil }))
	return r
}`)

	_, err := DiscoverRuntimeCatalog(source)
	if err == nil || !strings.Contains(err.Error(), "literal string") {
		t.Fatalf("DiscoverRuntimeCatalog() error = %v, want literal-string error", err)
	}
}

func TestDiscoverRuntimeCatalogRejectsShadowedImportQualifier(t *testing.T) {
	source := []byte(`package main
import runtimealias "github.com/gastownhall/gascity/internal/runtime"
import registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
func buildRuntimeRegistry() {
	r := registryalias.New()
	must(r.Register("fake", func() {
		runtimealias := localProviderFactory{}
		return runtimealias.NewFake(), nil
	}))
	return r
}`)

	_, err := DiscoverRuntimeCatalog(source)
	if err == nil || !strings.Contains(err.Error(), "not an imported package") {
		t.Fatalf("DiscoverRuntimeCatalog() error = %v, want shadowed-import error", err)
	}
}

func TestDiscoverRuntimeCatalogRejectsUnledgeredFallbackConstructor(t *testing.T) {
	source := []byte(`package main
import (
	execalias "github.com/gastownhall/gascity/internal/runtime/exec"
	registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
	tmuxalias "github.com/gastownhall/gascity/internal/runtime/tmux"
)
func buildRuntimeRegistry() {
	r := registryalias.New()
	tmuxFactory := func() { return tmuxalias.NewSeamBackedWithConfig(), nil }
	fallbackFactory := func() { return execalias.NewSeamBacked("provider"), nil }
	must(r.Register("tmux", tmuxFactory))
	r.SetFallback(fallbackFactory)
	return r
}`)

	_, err := DiscoverRuntimeCatalog(source)
	if err == nil || !strings.Contains(err.Error(), "fallback constructor set") {
		t.Fatalf("DiscoverRuntimeCatalog() error = %v, want unledgered-fallback error", err)
	}
}

func TestDiscoverRuntimeCatalogRequiresExactlyOneFallback(t *testing.T) {
	const prefix = `package main
import (
	registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
	runtimealias "github.com/gastownhall/gascity/internal/runtime"
)
func buildRuntimeRegistry() {
	r := registryalias.New()
	factory := func() { return runtimealias.NewFake(), nil }
	must(r.Register("fake", factory))
`
	tests := []struct {
		name     string
		fallback string
	}{
		{name: "missing"},
		{name: "duplicate", fallback: "r.SetFallback(factory)\nr.SetFallback(factory)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := []byte(prefix + tt.fallback + "\nreturn r\n}")
			_, err := DiscoverRuntimeCatalog(source)
			if err == nil || !strings.Contains(err.Error(), "exactly one SetFallback") {
				t.Fatalf("DiscoverRuntimeCatalog() error = %v, want fallback-cardinality error", err)
			}
		})
	}
}

func TestDiscoverRuntimeCatalogRejectsSuccessfulNilProviderReturn(t *testing.T) {
	const prefix = `package main
import (
	registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
	runtimealias "github.com/gastownhall/gascity/internal/runtime"
)
func buildRuntimeRegistry() {
	r := registryalias.New()
	factory := func(enabled bool) (any, error) {
		if enabled { return runtimealias.NewFake(), nil }
		RETURN
	}
	must(r.Register("fake", factory))
	r.SetFallback(factory)
	return r
}`

	t.Run("nil provider with nil error", func(t *testing.T) {
		source := []byte(strings.Replace(prefix, "RETURN", "return nil, nil", 1))
		_, err := DiscoverRuntimeCatalog(source)
		if err == nil || !strings.Contains(err.Error(), "nil provider with nil error") {
			t.Fatalf("DiscoverRuntimeCatalog() error = %v, want successful-nil-provider error", err)
		}
	})

	t.Run("nil provider with unconstrained error is rejected", func(t *testing.T) {
		source := []byte(strings.Replace(prefix, "RETURN", "var err error; return nil, err", 1))
		_, err := DiscoverRuntimeCatalog(source)
		if err == nil || !strings.Contains(err.Error(), "proven non-nil error guard") {
			t.Fatalf("DiscoverRuntimeCatalog() error = %v, want unproven-error error", err)
		}
	})

	t.Run("nil provider with typed nil error is rejected", func(t *testing.T) {
		source := []byte(strings.Replace(prefix, "RETURN", "return nil, error(nil)", 1))
		_, err := DiscoverRuntimeCatalog(source)
		if err == nil || !strings.Contains(err.Error(), "proven non-nil error guard") {
			t.Fatalf("DiscoverRuntimeCatalog() error = %v, want typed-nil-error error", err)
		}
	})

	t.Run("nil provider under non-nil error guard remains valid", func(t *testing.T) {
		source := []byte(strings.Replace(prefix, "RETURN", "var err error; if err != nil { return nil, err }; return runtimealias.NewFake(), nil", 1))
		if _, err := DiscoverRuntimeCatalog(source); err != nil {
			t.Fatalf("DiscoverRuntimeCatalog(guarded nil provider): %v", err)
		}
	})

	t.Run("nil provider after guarded error reassignment is rejected", func(t *testing.T) {
		source := []byte(strings.Replace(prefix, "RETURN", "var err error; if err != nil { err = nil; return nil, err }; return runtimealias.NewFake(), nil", 1))
		_, err := DiscoverRuntimeCatalog(source)
		if err == nil || !strings.Contains(err.Error(), "proven non-nil error guard") {
			t.Fatalf("DiscoverRuntimeCatalog() error = %v, want reassigned-error rejection", err)
		}
	})

	t.Run("nil provider after mutating guard conjunct is rejected", func(t *testing.T) {
		source := []byte(strings.Replace(prefix, "RETURN", "var err error; if err != nil && func() bool { err = nil; return true }() { return nil, err }; return runtimealias.NewFake(), nil", 1))
		_, err := DiscoverRuntimeCatalog(source)
		if err == nil || !strings.Contains(err.Error(), "proven non-nil error guard") {
			t.Fatalf("DiscoverRuntimeCatalog() error = %v, want mutating-conjunct rejection", err)
		}
	})
}

func TestDiscoverRuntimeCatalogBindsRegistryAndFactoriesByObject(t *testing.T) {
	const imports = `import (
	registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
	runtimealias "github.com/gastownhall/gascity/internal/runtime"
)`

	t.Run("registry variable may be renamed", func(t *testing.T) {
		source := []byte("package main\n" + imports + `
func buildRuntimeRegistry() {
	catalog := registryalias.New()
	factory := func() { return runtimealias.NewFake(), nil }
	must(catalog.Register("fake", factory))
	catalog.SetFallback(factory)
	return catalog
}`)
		got, err := DiscoverRuntimeCatalog(source)
		if err != nil {
			t.Fatalf("DiscoverRuntimeCatalog: %v", err)
		}
		if len(got) != 1 || got[0].Key != "exact:fake" {
			t.Fatalf("catalog = %v, want exact:fake", got)
		}
	})

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "registry alias hides registration",
			body: `r := registryalias.New()
	alias := r
	must(alias.Register("hidden", func() { return runtimealias.NewFailFake(), nil }))
	must(r.Register("fake", func() { return runtimealias.NewFake(), nil }))
	return r`,
			want: "catalog mutation receiver is not the bound registry",
		},
		{
			name: "registry passed to helper",
			body: `r := registryalias.New()
	registerExtra(r)
	must(r.Register("fake", func() { return runtimealias.NewFake(), nil }))
	return r`,
			want: "registry binding escapes direct catalog operations",
		},
		{
			name: "shadowed registry receiver",
			body: `r := registryalias.New()
	{
		r := registryalias.New()
		must(r.Register("hidden", func() { return runtimealias.NewFailFake(), nil }))
	}
	must(r.Register("fake", func() { return runtimealias.NewFake(), nil }))
	return r`,
			want: "catalog mutation receiver is not the bound registry",
		},
		{
			name: "registry reassigned",
			body: `r := registryalias.New()
	r = registryalias.New()
	must(r.Register("fake", func() { return runtimealias.NewFake(), nil }))
	return r`,
			want: "runtime registry must be one direct local binding",
		},
		{
			name: "factory reassigned",
			body: `r := registryalias.New()
	factory := func() { return runtimealias.NewFake(), nil }
	factory = func() { return runtimealias.NewFailFake(), nil }
	must(r.Register("fake", factory))
	return r`,
			want: "factory binding escapes direct catalog use",
		},
		{
			name: "factory passed to helper",
			body: `r := registryalias.New()
	factory := func() { return runtimealias.NewFake(), nil }
	inspect(factory)
	must(r.Register("fake", factory))
	return r`,
			want: "factory binding escapes direct catalog use",
		},
		{
			name: "factory shadowed",
			body: `r := registryalias.New()
	factory := func() { return runtimealias.NewFake(), nil }
	{
		factory := func() { return runtimealias.NewFailFake(), nil }
		must(r.Register("fake", factory))
	}
	return r`,
			want: "must be a direct top-level operation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := []byte("package main\n" + imports + "\nfunc buildRuntimeRegistry() {\n" + tt.body + "\n}")
			_, err := DiscoverRuntimeCatalog(source)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("DiscoverRuntimeCatalog() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestDiscoverRuntimeCatalogRejectsBareAndNamedProviderReturns(t *testing.T) {
	const prefix = `package main
import (
	registryalias "github.com/gastownhall/gascity/internal/runtime/registry"
	runtimealias "github.com/gastownhall/gascity/internal/runtime"
)
func buildRuntimeRegistry() {
	r := registryalias.New()
`
	tests := []struct {
		name    string
		factory string
	}{
		{name: "bare", factory: `func() (provider any, err error) { provider = runtimealias.NewFake(); return }`},
		{name: "named variable", factory: `func() (any, error) { provider := runtimealias.NewFake(); return provider, nil }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := []byte(prefix + `must(r.Register("fake", ` + tt.factory + `))
	return r
}`)
			_, err := DiscoverRuntimeCatalog(source)
			if err == nil || !strings.Contains(err.Error(), "must directly call its constructor") {
				t.Fatalf("DiscoverRuntimeCatalog() error = %v, want direct-constructor return error", err)
			}
		})
	}
}

func TestCompareRuntimeCatalogRejectsConstructorDrift(t *testing.T) {
	claim := ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	}
	entry := validRuntimeEntry("runtime.fake", "exact:fake", claim)
	entry.Constructors = []SymbolRef{{ImportPath: moduleImportPath + "/internal/runtime", Name: "NewFake"}}
	discovered := []RuntimeRegistration{{
		Key:          "exact:fake",
		Constructors: []SymbolRef{{ImportPath: moduleImportPath + "/internal/runtime/exec", Name: "NewSeamBacked"}},
	}}

	err := CompareRuntimeCatalog([]Entry{entry}, discovered)
	if err == nil || !strings.Contains(err.Error(), "constructor set") {
		t.Fatalf("CompareRuntimeCatalog() error = %v, want constructor-set drift", err)
	}
}

func TestCompareRuntimeCatalogChecksBothDirections(t *testing.T) {
	claim := ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	}
	entries := []Entry{
		validRuntimeEntry("runtime.fake", "exact:fake", claim),
		validRuntimeEntry("runtime.stale", "exact:stale", claim),
	}

	err := CompareRuntimeCatalog(entries, []RuntimeRegistration{
		{
			Key:          "exact:fake",
			Constructors: append([]SymbolRef(nil), entries[0].Constructors...),
		},
		{
			Key:          "prefix:exec:",
			Constructors: []SymbolRef{{ImportPath: moduleImportPath + "/internal/runtime/exec", Name: "NewSeamBacked"}},
		},
	})
	if err == nil {
		t.Fatal("CompareRuntimeCatalog() succeeded, want missing and stale errors")
	}
	for _, want := range []string{"prefix:exec: is missing from the ledger", "exact:stale is not registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CompareRuntimeCatalog() error = %v, want containing %q", err, want)
		}
	}
}

func TestCompareRuntimeCatalogRejectsUnknownCatalogAndMissingProductionRole(t *testing.T) {
	claim := ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	}
	unknown := validRuntimeEntry("runtime.unknown", "exact:unknown", claim)
	unknown.Catalog.Name = "runtime.typo"
	nonProduction := validRuntimeEntry("runtime.non-production", "exact:fake", claim)
	nonProduction.Roles = []Role{RoleReusableDouble}

	err := CompareRuntimeCatalog([]Entry{unknown, nonProduction}, []RuntimeRegistration{{
		Key:          "exact:fake",
		Constructors: append([]SymbolRef(nil), nonProduction.Constructors...),
	}})
	if err == nil {
		t.Fatal("CompareRuntimeCatalog() succeeded, want discovery-classification errors")
	}
	for _, want := range []string{"unknown catalog", "requires role production_provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CompareRuntimeCatalog() error = %v, want containing %q", err, want)
		}
	}
}

func TestValidateSourceRefsBindsManualCompositionConstructor(t *testing.T) {
	root := t.TempDir()
	entry := Entry{
		ID:           "runtime.composition.auto",
		Roles:        []Role{RoleProductionProvider},
		Port:         PortRuntimeProvider,
		Constructors: []SymbolRef{{ImportPath: moduleImportPath + "/internal/runtime/auto", Name: "New"}},
		Source: &SourceRef{
			File:     "cmd/gc/providers.go",
			Function: "resolveSessionTransportProvider",
			Reason:   "conditional transport composition is outside the runtime registry",
		},
	}

	const imports = `import (
	autoalias "github.com/gastownhall/gascity/internal/runtime/auto"
	otheralias "github.com/gastownhall/gascity/internal/runtime/other"
)`
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "valid bound return",
			body: `if enabled {
		p := autoalias.New(base, acp)
		p.RouteACP("worker")
		return p, nil
	}
	return base, nil`,
		},
		{
			name: "deleted constructor",
			body: `return base, nil`,
			want: "requires exactly one constructor call",
		},
		{
			name: "replaced constructor",
			body: `return otheralias.New(base, acp), nil`,
			want: "requires exactly one constructor call",
		},
		{
			name: "extra constructor",
			body: `extra := autoalias.NewOther(base, acp)
		_ = extra
		return autoalias.New(base, acp), nil`,
			want: "requires exactly one constructor call",
		},
		{
			name: "dead constructor closure",
			body: `dead := func() any { return autoalias.New(base, acp) }
		_ = dead
		return base, nil`,
			want: "requires exactly one constructor call",
		},
		{
			name: "discarded constructor",
			body: `autoalias.New(base, acp)
		return base, nil`,
			want: "must directly return or bind its constructor result",
		},
		{
			name: "different provider returned",
			body: `p := autoalias.New(base, acp)
		return other, nil`,
			want: "result is not returned",
		},
		{
			name: "reachable conditional discard",
			body: `p := autoalias.New(base, acp)
		if enabled { return p, nil }
		return other, nil`,
			want: "unconditional direct return in the same lexical block",
		},
		{
			name: "conditional direct constructor return",
			body: `if enabled { return autoalias.New(base, acp), nil }
		return base, nil`,
		},
		{
			name: "wrapped constructor return",
			body: `return otheralias.Wrap(autoalias.New(base, acp)), nil`,
			want: "must directly return or bind its constructor result",
		},
		{
			name: "aliased constructor result",
			body: `p := autoalias.New(base, acp)
	alias := p
	_ = alias
	return p, nil`,
			want: "escapes its direct return path",
		},
		{
			name: "reassigned constructor result",
			body: `p := autoalias.New(base, acp)
	p = other
	return p, nil`,
			want: "escapes its direct return path",
		},
		{
			name: "captured constructor result",
			body: `p := autoalias.New(base, acp)
	use := func() { _ = p }
	_ = use
	return p, nil`,
			want: "escapes its direct return path",
		},
		{
			name: "goroutine method escape",
			body: `p := autoalias.New(base, acp)
	go p.RouteACP("worker")
	return p, nil`,
			want: "escapes its direct return path",
		},
		{
			name: "unrelated same-name shadow",
			body: `p := autoalias.New(base, acp)
	if enabled {
		p := other
		_ = p
	}
	return p, nil`,
		},
		{
			name: "shadowed constructor qualifier",
			body: `if enabled {
		autoalias := other
		return autoalias.New(base, acp), nil
	}
	return base, nil`,
			want: "requires exactly one constructor call",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(root, strings.ReplaceAll(tt.name, " ", "-"), "cmd/gc")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			source := "package main\n" + imports + "\nfunc resolveSessionTransportProvider() {\n" + tt.body + "\n}\n"
			if err := os.WriteFile(filepath.Join(dir, "providers.go"), []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			err := ValidateSourceRefs(filepath.Dir(filepath.Dir(dir)), []Entry{entry})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ValidateSourceRefs(valid): %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateSourceRefs() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCatalogMatchesProductionWiringAndDocumentation(t *testing.T) {
	root := repoRoot(t)
	entries := Catalog()
	if err := Validate(entries, time.Now().UTC()); err != nil {
		t.Fatalf("Validate(Catalog): %v", err)
	}

	runtimeSource, err := os.ReadFile(filepath.Join(root, "cmd/gc/runtime_registry.go"))
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := DiscoverRuntimeCatalog(runtimeSource)
	if err != nil {
		t.Fatalf("DiscoverRuntimeCatalog: %v", err)
	}
	if err := CompareRuntimeCatalog(entries, discovered); err != nil {
		t.Fatalf("CompareRuntimeCatalog: %v", err)
	}
	doubles, err := DiscoverRuntimeProviderDoubles(filepath.Join(root, "internal/runtime"))
	if err != nil {
		t.Fatalf("DiscoverRuntimeProviderDoubles: %v", err)
	}
	if err := CompareReusableDoubles(entries, doubles); err != nil {
		t.Fatalf("CompareReusableDoubles: %v", err)
	}
	if err := ValidateSourceRefs(root, entries); err != nil {
		t.Fatalf("ValidateSourceRefs: %v", err)
	}
	doc, err := os.ReadFile(filepath.Join(root, "TESTING.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckMarkdown(string(doc), entries); err != nil {
		t.Fatalf("CheckMarkdown: %v", err)
	}
}

func TestCatalogReturnsIndependentEntries(t *testing.T) {
	first := Catalog()
	first[0].Roles[0] = RoleReusableDouble
	first[0].Constructors[0].Name = "MutatedConstructor"
	first[0].DoubleType.Name = "MutatedDouble"
	first[0].Catalog.Name = "mutated.catalog"
	first[0].Claims[0].Contract = ContractID("mutated.contract")
	first[0].Claims[0].Waiver.Owner = "mutated-owner"
	first[len(first)-1].Source.Function = "mutatedSource"

	second := Catalog()
	if second[0].Roles[0] != RoleProductionProvider {
		t.Errorf("Catalog() role leaked mutation: %q", second[0].Roles[0])
	}
	if second[0].Constructors[0].Name != "NewFake" {
		t.Errorf("Catalog() constructor leaked mutation: %q", second[0].Constructors[0].Name)
	}
	if second[0].DoubleType == nil || second[0].DoubleType.Name != "Fake" {
		t.Errorf("Catalog() double type leaked mutation: %v", second[0].DoubleType)
	}
	if second[0].Catalog.Name != RuntimeBuiltinCatalog {
		t.Errorf("Catalog() catalog leaked mutation: %q", second[0].Catalog.Name)
	}
	if second[0].Claims[0].Contract != ContractRuntimeProvider {
		t.Errorf("Catalog() claim leaked mutation: %q", second[0].Claims[0].Contract)
	}
	if second[0].Claims[0].Waiver.Owner != "ga-80po0c.1.2" {
		t.Errorf("Catalog() waiver leaked mutation: %q", second[0].Claims[0].Waiver.Owner)
	}
	if second[len(second)-1].Source.Function != "resolveSessionTransportProvider" {
		t.Errorf("Catalog() source leaked mutation: %q", second[len(second)-1].Source.Function)
	}
}

func TestCheckMarkdownRejectsDrift(t *testing.T) {
	entries := []Entry{validRuntimeEntry("runtime.fake", "exact:fake", ContractClaim{
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	})}
	doc := MarkdownStart + "\nstale\n" + MarkdownEnd

	err := CheckMarkdown(doc, entries)
	if err == nil || !strings.Contains(err.Error(), "does not match the provider ledger") {
		t.Fatalf("CheckMarkdown() error = %v, want drift error", err)
	}
	if !strings.Contains(err.Error(), RenderMarkdown(entries)) {
		t.Fatalf("CheckMarkdown() error = %v, want actionable expected block", err)
	}
}

func validRuntimeEntry(id, key string, claim ContractClaim) Entry {
	if claim.Constructor == (SymbolRef{}) {
		claim.Constructor = SymbolRef{ImportPath: "example.test/provider", Name: "New"}
	}
	return Entry{
		ID:           id,
		Roles:        []Role{RoleProductionProvider},
		Port:         PortRuntimeProvider,
		Constructors: []SymbolRef{claim.Constructor},
		Catalog:      &CatalogRef{Name: RuntimeBuiltinCatalog, Key: key},
		Claims:       []ContractClaim{claim},
	}
}

func reusableRuntimeEntry(id, key, typeName, constructorName string) Entry {
	constructor := repoSymbol("internal/runtime", constructorName)
	entry := validRuntimeEntry(id, key, ContractClaim{
		Constructor:         constructor,
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: "fixture",
	})
	entry.Roles = append(entry.Roles, RoleReusableDouble)
	doubleType := repoSymbol("internal/runtime", typeName)
	entry.DoubleType = &doubleType
	entry.DoubleBoundary = runtimeDoubleBoundaryPath
	return entry
}

func writeRuntimeDoubleFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, source := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func renderReusableDoubles(doubles []ReusableDouble) string {
	rows := make([]string, 0, len(doubles))
	for _, double := range doubles {
		rows = append(rows, renderSymbolRef(double.Type)+"="+renderSymbolRefs(double.Constructors))
	}
	return strings.Join(rows, ";")
}

func renderRegistrations(registrations []RuntimeRegistration) string {
	var rows []string
	for _, registration := range registrations {
		var symbols []string
		for _, ref := range registration.Constructors {
			symbols = append(symbols, ref.ImportPath+"."+ref.Name)
		}
		rows = append(rows, registration.Key+"="+strings.Join(symbols, "+"))
	}
	return strings.Join(rows, ",")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root")
		}
		dir = parent
	}
}
