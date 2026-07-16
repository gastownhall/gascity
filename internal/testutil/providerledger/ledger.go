// Package providerledger owns the checked inventory that connects provider
// construction paths to their required contract dispositions.
package providerledger

import (
	"errors"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"
	"time"
)

const moduleImportPath = "github.com/gastownhall/gascity"

// SymbolRef identifies a Go declaration by import path and declared name.
type SymbolRef struct {
	ImportPath string
	Name       string
}

// Role classifies how an entry is used.
type Role string

const (
	// RoleProductionProvider marks a provider reachable from production wiring.
	RoleProductionProvider Role = "production_provider"
	// RoleReusableDouble marks a provider deliberately reused by tests.
	RoleReusableDouble Role = "reusable_double"
)

// Port identifies the interface contract implemented by a ledger entry.
type Port string

const (
	// PortRuntimeProvider is the runtime.Provider interface.
	PortRuntimeProvider Port = "runtime.Provider"
)

// ContractID identifies one executable conformance contract.
type ContractID string

const (
	// ContractRuntimeProvider is the full runtimetest provider contract.
	ContractRuntimeProvider ContractID = "runtime.Provider"
)

// Disposition records how one required contract is accounted for.
type Disposition string

const (
	// DispositionProved records an executable, source-checked contract proof.
	DispositionProved Disposition = "proved"
	// DispositionWaived records a temporary, owned contract gap.
	DispositionWaived Disposition = "waived"
	// DispositionNotApplicable records why a contract does not apply.
	DispositionNotApplicable Disposition = "not_applicable"
)

var runtimeProviderRunner = repoSymbol("internal/runtime/runtimetest", "RunProviderTests")

const (
	// RuntimeBuiltinCatalog names cmd/gc's static runtime provider registry.
	RuntimeBuiltinCatalog = "runtime.builtin"
	// runtimeDoubleBoundaryPath is the designated runtime.Provider double source.
	runtimeDoubleBoundaryPath = "internal/runtime/fake.go"
	// runtimeContractWaiverOwner owns the remaining production-runtime gaps.
	runtimeContractWaiverOwner = "ga-80po0c.3"

	// MarkdownStart begins the generated TESTING.md table.
	MarkdownStart = "<!-- BEGIN CHECKED RUNTIME PROVIDER LEDGER -->"
	// MarkdownEnd ends the generated TESTING.md table.
	MarkdownEnd = "<!-- END CHECKED RUNTIME PROVIDER LEDGER -->"

	maxWaiverHorizon = 90 * 24 * time.Hour
)

// CatalogRef binds a ledger entry to a discoverable production catalog key.
type CatalogRef struct {
	Name string
	Key  string
}

// SourceRef binds a composition outside a catalog to its production source.
type SourceRef struct {
	File     string
	Function string
	Reason   string
}

// ProofRef binds an exact runnable test to its contract runner. AllowedCalls
// lists pure setup calls permitted inside the inline provider factory; provider
// construction itself is always bound separately through ContractClaim.
type ProofRef struct {
	File         string
	Test         string
	Runner       SymbolRef
	AllowedCalls []SymbolRef
}

// Waiver is a temporary, owned exception to an applicable contract.
type Waiver struct {
	Owner   string
	Expires time.Time
	Reason  string
}

// ContractClaim accounts for one contract through exactly one disposition.
type ContractClaim struct {
	Constructor         SymbolRef
	Contract            ContractID
	Disposition         Disposition
	Proof               *ProofRef
	Waiver              *Waiver
	NotApplicableReason string
}

// Entry connects one provider construction path to its required contracts.
type Entry struct {
	ID             string
	Roles          []Role
	Port           Port
	Constructors   []SymbolRef
	DoubleType     *SymbolRef
	DoubleBoundary string

	// Production providers have exactly one catalog or source binding.
	Catalog *CatalogRef
	Source  *SourceRef

	Claims []ContractClaim
}

// Catalog returns fresh entries from the checked runtime-provider ledger.
func Catalog() []Entry {
	autoConstructor := repoSymbol("internal/runtime/auto", "New")
	return []Entry{
		reusableBuiltin(
			"fake", "exact:fake", repoSymbol("internal/runtime", "Fake"),
			provedRuntime(
				repoSymbol("internal/runtime", "NewFake"),
				"internal/runtime/fake_conformance_test.go",
				"TestFakeConformance",
				SymbolRef{ImportPath: "fmt", Name: "Sprintf"},
				SymbolRef{ImportPath: "sync/atomic", Name: "AddInt64"},
			),
		),
		reusableBuiltin(
			"fail", "exact:fail", repoSymbol("internal/runtime", "Fake"),
			notApplicableRuntime(
				repoSymbol("internal/runtime", "NewFailFake"),
				"intentional faulting double: a successful lifecycle cannot be exercised, so the successful-provider contract is not applicable",
			),
		),
		builtin(
			"subprocess", "exact:subprocess", nil,
			waivedRuntime(
				repoSymbol("internal/runtime/subprocess", "NewSeamBacked"),
				"NewSeamBacked selects a distinct reachable empty-cityPath branch with shared /tmp state; the WithDir proof does not exercise that composition",
			),
			provedRuntime(
				repoSymbol("internal/runtime/subprocess", "NewSeamBackedWithDir"),
				"internal/runtime/subprocess/seam_conformance_test.go",
				"TestSubprocessSeamConformance",
				SymbolRef{ImportPath: "fmt", Name: "Sprintf"},
				repoSymbol("internal/testutil", "ShortTempDir"),
				SymbolRef{ImportPath: "sync/atomic", Name: "AddInt64"},
			),
		),
		builtin(
			"acp", "exact:acp", nil,
			waivedRuntime(
				repoSymbol("internal/runtime/acp", "NewSeamBacked"),
				"full conformance covers the raw ACP provider, not the NewSeamBacked production composition",
			),
			waivedRuntime(
				repoSymbol("internal/runtime/acp", "NewSeamBackedWithDir"),
				"full conformance covers the raw ACP provider, not the NewSeamBackedWithDir production composition",
			),
		),
		builtin(
			"t3bridge", "exact:t3bridge", nil,
			waivedRuntime(
				repoSymbol("internal/runtime/t3bridge", "NewSeamBacked"),
				"the production T3 bridge composition has focused tests but no full shared runtime contract",
			),
		),
		builtin(
			"k8s", "exact:k8s", nil,
			waivedRuntime(
				repoSymbol("internal/runtime/k8s", "NewSeamBacked"),
				"the actual K8s production composition has no full shared runtime contract",
			),
		),
		builtin(
			"herdr", "exact:herdr", nil,
			waivedRuntime(
				repoSymbol("internal/runtime/herdr", "New"),
				"the existing full conformance run skips in short mode or when the herdr executable is absent",
			),
		),
		builtin(
			"hybrid", "exact:hybrid", nil,
			waivedRuntime(
				repoSymbol("cmd/gc", "newHybridProvider"),
				"cmd/gc.newHybridProvider is the selected registry construction boundary; its internal tmux, K8s, and hybrid constructors are not claimed here, and the wrapper has no full shared runtime contract",
			),
		),
		builtin(
			"exec", "prefix:exec:", nil,
			waivedRuntime(
				repoSymbol("internal/runtime/exec", "NewSeamBacked"),
				"full conformance covers the raw exec provider, not the production seam-backed prefix composition",
			),
			waivedRuntime(
				repoSymbol("internal/runtime/t3bridge", "NewSeamBacked"),
				"the legacy gc-session-t3 prefix branch selects the T3 bridge composition, which has no full shared runtime contract",
			),
		),
		builtin(
			"ssh", "prefix:ssh:", nil,
			waivedRuntime(
				repoSymbol("internal/runtime/ssh", "NewSeamBacked"),
				"the production SSH composition has no full shared runtime contract",
			),
		),
		builtin(
			"tmux", "exact:tmux", nil,
			waivedRuntime(
				repoSymbol("internal/runtime/tmux", "NewSeamBackedWithConfig"),
				"the existing full conformance run skips when the tmux executable is absent",
			),
		),
		{
			ID:           "runtime.composition.auto",
			Roles:        []Role{RoleProductionProvider},
			Port:         PortRuntimeProvider,
			Constructors: []SymbolRef{autoConstructor},
			Source: &SourceRef{
				File:     "cmd/gc/providers.go",
				Function: "resolveSessionTransportProvider",
				Reason:   "conditional transport composition is outside the runtime registry",
			},
			Claims: []ContractClaim{waivedRuntime(autoConstructor,
				"the production auto base/ACP composition has no full shared runtime contract",
			)},
		},
	}
}

func repoSymbol(packagePath, name string) SymbolRef {
	return SymbolRef{ImportPath: moduleImportPath + "/" + packagePath, Name: name}
}

func runtimeCatalogRef(key string) *CatalogRef {
	return &CatalogRef{Name: RuntimeBuiltinCatalog, Key: key}
}

func builtin(id, key string, extraRoles []Role, claims ...ContractClaim) Entry {
	constructors := make([]SymbolRef, 0, len(claims))
	for _, claim := range claims {
		constructors = append(constructors, claim.Constructor)
	}
	return Entry{
		ID:           "runtime.builtin." + id,
		Roles:        append([]Role{RoleProductionProvider}, extraRoles...),
		Port:         PortRuntimeProvider,
		Constructors: normalizeSymbolRefs(constructors),
		Catalog:      runtimeCatalogRef(key),
		Claims:       append([]ContractClaim(nil), claims...),
	}
}

func reusableBuiltin(id, key string, doubleType SymbolRef, claims ...ContractClaim) Entry {
	entry := builtin(id, key, []Role{RoleReusableDouble}, claims...)
	entry.DoubleType = &doubleType
	entry.DoubleBoundary = runtimeDoubleBoundaryPath
	return entry
}

func provedRuntime(constructor SymbolRef, file, test string, allowedCalls ...SymbolRef) ContractClaim {
	return ContractClaim{
		Constructor: constructor,
		Contract:    ContractRuntimeProvider,
		Disposition: DispositionProved,
		Proof: &ProofRef{
			File:         file,
			Test:         test,
			Runner:       runtimeProviderRunner,
			AllowedCalls: append([]SymbolRef(nil), allowedCalls...),
		},
	}
}

func waivedRuntime(constructor SymbolRef, reason string) ContractClaim {
	return ContractClaim{
		Constructor: constructor,
		Contract:    ContractRuntimeProvider,
		Disposition: DispositionWaived,
		Waiver: &Waiver{
			Owner:   runtimeContractWaiverOwner,
			Expires: time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC),
			Reason:  reason,
		},
	}
}

func notApplicableRuntime(constructor SymbolRef, reason string) ContractClaim {
	return ContractClaim{
		Constructor:         constructor,
		Contract:            ContractRuntimeProvider,
		Disposition:         DispositionNotApplicable,
		NotApplicableReason: reason,
	}
}

// Validate checks ledger structure and waiver policy at the supplied time.
func Validate(entries []Entry, now time.Time) error {
	var problems []string
	seenIDs := make(map[string]bool)
	seenCatalogKeys := make(map[string]string)
	seenSourceRefs := make(map[string]string)

	for _, entry := range entries {
		prefix := fmt.Sprintf("entry %q", entry.ID)
		if strings.TrimSpace(entry.ID) == "" {
			problems = append(problems, "entry ID is required")
		}
		if seenIDs[entry.ID] {
			problems = append(problems, prefix+" is duplicated")
		}
		seenIDs[entry.ID] = true
		if len(entry.Constructors) == 0 {
			problems = append(problems, prefix+" requires at least one constructor symbol")
		}
		seenConstructors := make(map[SymbolRef]bool)
		for _, constructor := range entry.Constructors {
			if err := validateSymbolRef(constructor); err != nil {
				problems = append(problems, fmt.Sprintf("%s constructor: %v", prefix, err))
			}
			if seenConstructors[constructor] {
				problems = append(problems, fmt.Sprintf("%s repeats constructor %s", prefix, renderSymbolRef(constructor)))
			}
			seenConstructors[constructor] = true
		}

		roles := make(map[Role]bool)
		for _, role := range entry.Roles {
			switch role {
			case RoleProductionProvider, RoleReusableDouble:
			default:
				problems = append(problems, fmt.Sprintf("%s has unknown role %q", prefix, role))
			}
			if roles[role] {
				problems = append(problems, fmt.Sprintf("%s repeats role %q", prefix, role))
			}
			roles[role] = true
		}
		if len(roles) == 0 {
			problems = append(problems, prefix+" requires at least one role")
		}
		switch {
		case roles[RoleReusableDouble]:
			if entry.DoubleType == nil {
				problems = append(problems, prefix+" reusable_double role requires a double type")
			} else if err := validateSymbolRef(*entry.DoubleType); err != nil {
				problems = append(problems, fmt.Sprintf("%s double type: %v", prefix, err))
			}
			boundary := pathpkg.Clean(strings.TrimSpace(entry.DoubleBoundary))
			if boundary == "." || strings.HasPrefix(boundary, "../") || strings.HasPrefix(boundary, "/") {
				problems = append(problems, prefix+" reusable_double role requires a repository-relative double boundary")
			}
		case entry.DoubleType != nil:
			problems = append(problems, prefix+" double type requires role reusable_double")
		case strings.TrimSpace(entry.DoubleBoundary) != "":
			problems = append(problems, prefix+" double boundary requires role reusable_double")
		}
		if (entry.Catalog != nil || entry.Source != nil) && !roles[RoleProductionProvider] {
			problems = append(problems, prefix+" discovery binding requires role production_provider")
		}
		if roles[RoleProductionProvider] {
			discoveryCount := 0
			if entry.Catalog != nil {
				discoveryCount++
			}
			if entry.Source != nil {
				discoveryCount++
			}
			if discoveryCount != 1 {
				problems = append(problems, prefix+" production provider requires exactly one catalog or source binding")
			}
		}
		if entry.Catalog != nil {
			catalogKey := entry.Catalog.Name + "/" + entry.Catalog.Key
			if strings.TrimSpace(entry.Catalog.Name) == "" || strings.TrimSpace(entry.Catalog.Key) == "" {
				problems = append(problems, prefix+" catalog name and key are required")
			} else if entry.Catalog.Name != RuntimeBuiltinCatalog {
				problems = append(problems, fmt.Sprintf("%s has unknown catalog %q", prefix, entry.Catalog.Name))
			} else if prior := seenCatalogKeys[catalogKey]; prior != "" {
				problems = append(problems, fmt.Sprintf("%s catalog key %s is also owned by %q", prefix, catalogKey, prior))
			} else {
				seenCatalogKeys[catalogKey] = entry.ID
			}
		}
		if entry.Source != nil {
			if strings.TrimSpace(entry.Source.File) == "" || strings.TrimSpace(entry.Source.Function) == "" || strings.TrimSpace(entry.Source.Reason) == "" {
				problems = append(problems, prefix+" source file, function, and reason are required")
			} else {
				sourceFile := pathpkg.Clean(strings.ReplaceAll(strings.TrimSpace(entry.Source.File), "\\", "/"))
				sourceKey := sourceFile + "#" + strings.TrimSpace(entry.Source.Function)
				if prior := seenSourceRefs[sourceKey]; prior != "" {
					problems = append(problems, fmt.Sprintf("%s source binding %s is also owned by %q", prefix, sourceKey, prior))
				} else {
					seenSourceRefs[sourceKey] = entry.ID
				}
			}
		}

		if entry.Port != PortRuntimeProvider {
			problems = append(problems, fmt.Sprintf("%s has unknown port %q", prefix, entry.Port))
		}
		type claimKey struct {
			constructor SymbolRef
			contract    ContractID
		}
		seenClaims := make(map[claimKey]bool)
		for _, claim := range entry.Claims {
			claimPrefix := fmt.Sprintf("%s constructor %s contract %s", prefix, renderSymbolRef(claim.Constructor), claim.Contract)
			if err := validateSymbolRef(claim.Constructor); err != nil {
				problems = append(problems, fmt.Sprintf("%s claim constructor: %v", prefix, err))
			} else if !seenConstructors[claim.Constructor] {
				problems = append(problems, fmt.Sprintf("%s constructor %s is not declared by the entry", prefix, renderSymbolRef(claim.Constructor)))
			}
			if claim.Contract != ContractRuntimeProvider {
				problems = append(problems, fmt.Sprintf("%s is not required by port %s", claimPrefix, entry.Port))
			}
			key := claimKey{constructor: claim.Constructor, contract: claim.Contract}
			if seenClaims[key] {
				problems = append(problems, claimPrefix+" is duplicated")
			}
			seenClaims[key] = true
			problems = append(problems, validateClaim(claimPrefix, claim, now)...)
		}
		for _, constructor := range entry.Constructors {
			if !seenClaims[claimKey{constructor: constructor, contract: ContractRuntimeProvider}] {
				problems = append(problems, fmt.Sprintf("%s constructor %s is missing required contract %s", prefix, renderSymbolRef(constructor), ContractRuntimeProvider))
			}
		}
	}

	return joinProblems(problems)
}

func hasRole(roles []Role, want Role) bool {
	for _, role := range roles {
		if role == want {
			return true
		}
	}
	return false
}

func validateClaim(prefix string, claim ContractClaim, now time.Time) []string {
	var problems []string
	payloads := 0
	if claim.Proof != nil {
		payloads++
	}
	if claim.Waiver != nil {
		payloads++
	}
	if strings.TrimSpace(claim.NotApplicableReason) != "" {
		payloads++
	}
	if payloads != 1 {
		problems = append(problems, prefix+" requires exactly one of proof, waiver, or not-applicable reason")
	}

	switch claim.Disposition {
	case DispositionProved:
		if claim.Proof == nil {
			problems = append(problems, prefix+" proved claim requires a proof")
		} else {
			if strings.TrimSpace(claim.Proof.File) == "" || strings.TrimSpace(claim.Proof.Test) == "" {
				problems = append(problems, prefix+" proof file and test are required")
			}
			if err := validateSymbolRef(claim.Proof.Runner); err != nil {
				problems = append(problems, fmt.Sprintf("%s proof runner: %v", prefix, err))
			} else if claim.Contract == ContractRuntimeProvider && claim.Proof.Runner != runtimeProviderRunner {
				problems = append(problems, fmt.Sprintf("%s proof runner is %s, want %s", prefix, renderSymbolRef(claim.Proof.Runner), renderSymbolRef(runtimeProviderRunner)))
			}
			seenAllowed := make(map[SymbolRef]bool)
			for _, allowed := range claim.Proof.AllowedCalls {
				if err := validateSymbolRef(allowed); err != nil {
					problems = append(problems, fmt.Sprintf("%s allowed proof call: %v", prefix, err))
				}
				if seenAllowed[allowed] {
					problems = append(problems, fmt.Sprintf("%s repeats allowed proof call %s", prefix, renderSymbolRef(allowed)))
				}
				seenAllowed[allowed] = true
			}
		}
	case DispositionWaived:
		if claim.Waiver == nil {
			problems = append(problems, prefix+" waived claim requires a waiver")
		}
	case DispositionNotApplicable:
		if strings.TrimSpace(claim.NotApplicableReason) == "" {
			problems = append(problems, prefix+" not-applicable claim requires a reason")
		}
	default:
		problems = append(problems, fmt.Sprintf("%s has unknown disposition %q", prefix, claim.Disposition))
	}

	if waiver := claim.Waiver; waiver != nil {
		if strings.TrimSpace(waiver.Owner) == "" {
			problems = append(problems, prefix+" waiver owner is required")
		}
		if strings.TrimSpace(waiver.Reason) == "" {
			problems = append(problems, prefix+" waiver reason is required")
		}
		if waiver.Expires.IsZero() {
			problems = append(problems, prefix+" waiver expiry is required")
		} else {
			if !waiver.Expires.After(now) {
				problems = append(problems, fmt.Sprintf("%s waiver owned by %s expired %s", prefix, waiver.Owner, waiver.Expires.Format("2006-01-02")))
			}
			if waiver.Expires.After(now.Add(maxWaiverHorizon)) {
				problems = append(problems, fmt.Sprintf("%s waiver owned by %s exceeds the %s horizon", prefix, waiver.Owner, maxWaiverHorizon))
			}
		}
	}
	return problems
}

func validateSymbolRef(ref SymbolRef) error {
	if strings.TrimSpace(ref.ImportPath) == "" || strings.TrimSpace(ref.Name) == "" {
		return errors.New("import path and name are required")
	}
	return nil
}

func normalizeSymbolRefs(refs []SymbolRef) []SymbolRef {
	unique := make(map[SymbolRef]bool)
	for _, ref := range refs {
		unique[ref] = true
	}
	refs = refs[:0]
	for ref := range unique {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ImportPath != refs[j].ImportPath {
			return refs[i].ImportPath < refs[j].ImportPath
		}
		return refs[i].Name < refs[j].Name
	})
	return refs
}

func equalSymbolRefs(left, right []SymbolRef) bool {
	left = normalizeSymbolRefs(append([]SymbolRef(nil), left...))
	right = normalizeSymbolRefs(append([]SymbolRef(nil), right...))
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func renderSymbolRef(ref SymbolRef) string {
	packagePath := strings.TrimPrefix(ref.ImportPath, moduleImportPath+"/")
	return packagePath + "." + ref.Name
}

func renderSymbolRefs(refs []SymbolRef) string {
	refs = normalizeSymbolRefs(append([]SymbolRef(nil), refs...))
	values := make([]string, len(refs))
	for i, ref := range refs {
		values[i] = renderSymbolRef(ref)
	}
	return strings.Join(values, ", ")
}

// RenderMarkdown renders the canonical marker-delimited ledger table.
func RenderMarkdown(entries []Entry) string {
	entries = append([]Entry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	var out strings.Builder
	out.WriteString(MarkdownStart)
	out.WriteString("\n")
	out.WriteString("This table is rendered from `internal/testutil/providerledger` and checked by `go test ./internal/testutil/providerledger`; edit the Go ledger, then use the expected block printed on drift.\n\n")
	out.WriteString("| Provider path | Roles | Reusable type | Port | Constructor | Discovery | Contract | Status |\n")
	out.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, entry := range entries {
		claims := append([]ContractClaim(nil), entry.Claims...)
		sort.Slice(claims, func(i, j int) bool {
			if claims[i].Constructor != claims[j].Constructor {
				return symbolRefLess(claims[i].Constructor, claims[j].Constructor)
			}
			return claims[i].Contract < claims[j].Contract
		})
		for _, claim := range claims {
			fmt.Fprintf(&out, "| `%s` | %s | %s | `%s` | `%s` | %s | `%s` | %s |\n",
				markdownCell(entry.ID),
				markdownCell(renderRoles(entry.Roles)),
				renderDoubleType(entry),
				markdownCell(string(entry.Port)),
				markdownCell(renderSymbolRef(claim.Constructor)),
				markdownCell(renderDiscovery(entry)),
				markdownCell(string(claim.Contract)),
				markdownCell(renderClaim(claim)),
			)
		}
	}
	out.WriteString(MarkdownEnd)
	return out.String()
}

func renderDoubleType(entry Entry) string {
	if entry.DoubleType == nil {
		return "—"
	}
	return "`" + markdownCell(renderSymbolRef(*entry.DoubleType)) + "`"
}

func symbolRefLess(left, right SymbolRef) bool {
	if left.ImportPath != right.ImportPath {
		return left.ImportPath < right.ImportPath
	}
	return left.Name < right.Name
}

func renderRoles(roles []Role) string {
	values := make([]string, len(roles))
	for i, role := range roles {
		values[i] = string(role)
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}

func renderDiscovery(entry Entry) string {
	var bindings []string
	if entry.Catalog != nil {
		bindings = append(bindings, entry.Catalog.Name+"/"+entry.Catalog.Key)
	}
	if entry.Source != nil {
		bindings = append(bindings, fmt.Sprintf("source: %s#%s — %s", entry.Source.File, entry.Source.Function, entry.Source.Reason))
	}
	if hasRole(entry.Roles, RoleReusableDouble) && strings.TrimSpace(entry.DoubleBoundary) != "" {
		bindings = append(bindings, "reusable: "+entry.DoubleBoundary)
	}
	if len(bindings) == 0 {
		return "invalid: no discovery binding"
	}
	return strings.Join(bindings, "; ")
}

func renderClaim(claim ContractClaim) string {
	switch claim.Disposition {
	case DispositionProved:
		if claim.Proof == nil {
			return "proved (invalid: no proof)"
		}
		return fmt.Sprintf("proved by %s#%s", claim.Proof.File, claim.Proof.Test)
	case DispositionWaived:
		if claim.Waiver == nil {
			return "waived (invalid: no waiver)"
		}
		return fmt.Sprintf("waived by %s through %s: %s", claim.Waiver.Owner, claim.Waiver.Expires.Format("2006-01-02"), claim.Waiver.Reason)
	case DispositionNotApplicable:
		return "not applicable: " + claim.NotApplicableReason
	default:
		return "invalid disposition: " + string(claim.Disposition)
	}
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	return strings.Join(strings.Fields(value), " ")
}

// CheckMarkdown checks the single generated TESTING.md ledger block.
func CheckMarkdown(document string, entries []Entry) error {
	if strings.Count(document, MarkdownStart) != 1 || strings.Count(document, MarkdownEnd) != 1 {
		return errors.New("TESTING.md must contain exactly one checked runtime provider ledger marker pair")
	}
	start := strings.Index(document, MarkdownStart)
	end := strings.Index(document[start:], MarkdownEnd)
	if end < 0 {
		return errors.New("TESTING.md checked runtime provider ledger markers are out of order")
	}
	end += start + len(MarkdownEnd)
	if got, want := document[start:end], RenderMarkdown(entries); got != want {
		return fmt.Errorf("TESTING.md checked runtime provider table does not match the provider ledger; replace the marker block with:\n%s", want)
	}
	return nil
}

func joinProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(strings.Join(problems, "\n"))
}
