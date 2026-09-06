package storebinding

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testpolicy/resourcecensus"
)

// No consumer may obtain a StoreSet before activation publishes one. Three layers
// enforce that, and it is worth being exact about which one closes what.
//
//  1. The type system closes every route Go's own rules police. Assembly is
//     unexported and returns UnpublishedStoreSet, never StoreSet, so the only
//     function anywhere that yields a populated set is Publish, and Publish
//     demands an authority whose sole mint is unexported. No import, alias, or
//     composite literal reaches around that, and it holds without anyone
//     running a test.
//  2. The go:linkname ban is what closes the rest, because go:linkname is
//     outside the type system entirely: a bodyless declaration bound to another
//     package's symbol needs no import, honors no exportedness, and is never
//     type-checked. Measured on this code, one pull of the old exported
//     constructor yielded a usable StoreSet outright; one pull of the current
//     assembler yields only an unpublishable candidate, but a second pull of
//     the unexported mint publishes it. Nothing expressible in Go stops that —
//     an unforgeable token would be read out of the package by the same
//     mechanism — so the mechanism itself is banned. That ban is a closure
//     rather than a fourth blocklist entry because a directive is lexical: the
//     compiler matches raw comment text, which admits no escape sequence,
//     alias, or import name to hide behind, so there is no second spelling to
//     chase.
//  3. These censuses keep the assembly, mint, and publish call sites where the
//     the design put them, and pin the type-system invariant of layer 1 so a later
//     slice cannot quietly reintroduce a second producer.
//
// The censuses are module-wide by subject — this package plus every non-test
// file that imports it, under any import name and any legal spelling of the
// import path — and every one of them fails closed: an empty subject set is a
// broken census, not a pass. Every evasion found in review is pinned in
// TestCensusSeesEveryKnownEvasion so none can regress silently.

const (
	censusBuilderFile     = "internal/storebinding/builder_storeset.go"
	censusPublicationFile = "internal/storebinding/builder_publication.go"
	censusStoreSetFile    = "internal/storebinding/storebinding.go"
	censusRegistryFile    = "internal/storebinding/provider.go"
	censusPlannerFile     = "internal/storebinding/plan_resolve.go"
	censusPackageDir      = "internal/storebinding"
	censusImportPath      = "github.com/gastownhall/gascity/internal/storebinding"
	censusMinimumFiles    = 500
)

// censusSanctionedActivationFiles lists the files allowed to mint publication
// authority or publish a StoreSet. the activation code must be added here
// explicitly; until then no non-test file may do either.
var censusSanctionedActivationFiles = map[string]bool{}

// censusSanctionedLinknameFiles and censusSanctionedUnsafeFiles are empty and
// are meant to stay empty. A go:linkname pull reaches an unexported symbol in
// another package with no import, no export, and no type check, so a single
// entry here voids every compile-time guarantee this package makes. Nothing in
// this module needs either; adding a file means arguing for it in review.
var (
	censusSanctionedLinknameFiles = map[string]bool{}
	censusSanctionedUnsafeFiles   = map[string]bool{}
)

// censusCompositionRootFiles are the cmd/gc provider-bundle files that carry
// the sole registry construction site.
var censusCompositionRootFiles = map[string]bool{
	"cmd/gc/storage_provider_bundle.go":         true,
	"cmd/gc/storage_provider_bundle_default.go": true,
	"cmd/gc/storage_provider_bundle_custom.go":  true,
}

// The identifier sets the censuses tally. They are declared once because the
// prefilters below have to provably cover them.
var (
	censusCandidateTypes     = map[string]bool{"UnpublishedStoreSet": true, "StoreSetPublicationAuthority": true}
	censusAssemblyNames      = map[string]bool{"newUnpublishedStoreSet": true}
	censusMintNames          = map[string]bool{"newStoreSetPublicationAuthority": true}
	censusRegistryTypes      = map[string]bool{"ProviderRegistry": true, "NewProviderRegistry": true}
	censusRegistryOperations = map[string]bool{"Lookup": true, "Freeze": true, "Register": true}
)

const (
	// censusPublishSelector is the publication call the census counts on any receiver.
	censusPublishSelector = "Publish"
	// censusStoreSetType is the published type. Populating one is the whole prize.
	censusStoreSetType = "StoreSet"
	// censusRetiredAssemblyName was the exported constructor that returned a
	// usable StoreSet to anyone who could name it. It is gone; a linkname pull
	// to it was the third distinct census evasion, and the reason assembly now
	// returns the candidate instead.
	censusRetiredAssemblyName = "NewStoreSet"
	// censusAssemblyName is the sole assembly path. It is unexported and returns
	// UnpublishedStoreSet.
	censusAssemblyName = "newUnpublishedStoreSet"
	// censusLinknameDirective is the banned mechanism, matched exactly as the
	// compiler matches it.
	censusLinknameDirective = "//go:linkname"
	// censusUnsafeImportPath is the import a linkname pull requires.
	censusUnsafeImportPath = "unsafe"
)

// censusPublicationTokens and censusRegistryTokens are the raw byte sequences a
// source must contain before it can name anything the matching census tallies.
// The prefilters key on identifier bytes rather than on the import path: Go
// identifiers admit no escapes, so a censused name always appears literally,
// while an import literal may spell the same path as a raw string or with
// escape sequences and share almost no bytes with the canonical form. Keying a
// prefilter on the path would skip such a file before it was ever parsed.
var (
	censusPublicationTokens = []string{"StoreSet", censusPublishSelector, censusImportPath}
	censusRegistryTokens    = []string{"ProviderRegistry", "Lookup", "Freeze", "Register", censusImportPath}
)

// censusEscapedImportPath spells the census import path with its final byte as
// a hex escape. It is a legal import literal denoting exactly the same package,
// and it shares no suffix with the canonical spelling, so any census that
// compares raw literal bytes is blind to it.
var censusEscapedImportPath = fmt.Sprintf(`%s\x%02x`, censusImportPath[:len(censusImportPath)-1], censusImportPath[len(censusImportPath)-1])

// TestStoreSetPublicationSites pins every site that can assemble or publish a
// StoreSet. The subject set is module-wide: every non-test file in this package
// plus every non-test file that imports it, because those are the two ways Go
// code can name the assembler. A qualified cross-package call, an aliased one,
// and a method value all count exactly like an in-package call, so the cmd/gc
// composition root cannot assemble a pre-witness StoreSet.
func TestStoreSetPublicationSites(t *testing.T) {
	report := censusPublicationSurvey(t, censusSources(t))

	allowedCandidateFiles := map[string]bool{censusBuilderFile: true, censusPublicationFile: true}
	for file := range censusSanctionedActivationFiles {
		allowedCandidateFiles[file] = true
	}
	for file := range report.candidates {
		if !allowedCandidateFiles[file] {
			t.Errorf("%s names the unpublished candidate or its publication authority; both stay inside the sanctioned builder files", file)
		}
	}
	for file := range report.assembly {
		if file != censusBuilderFile && !censusSanctionedActivationFiles[file] {
			t.Errorf("%s names %s; only the frozen builder may assemble a StoreSet candidate", file, censusAssemblyName)
		}
	}
	for file := range report.mints {
		if !censusSanctionedActivationFiles[file] {
			t.Errorf("%s mints publication authority; only sanctioned activation code may", file)
		}
	}
	for file := range report.publishes {
		if !censusSanctionedActivationFiles[file] {
			t.Errorf("%s publishes a StoreSet; only sanctioned activation code may", file)
		}
	}

	if report.subjects == 0 {
		t.Fatal("census found no file in or importing the storage package; the subject set of this census is empty")
	}
	if report.importers == 0 {
		t.Fatal("census found no importing consumer of the storage package; it is blind to cross-package assembly")
	}
	if report.assembly[censusBuilderFile] == 0 {
		t.Fatalf("census found no StoreSet assembly in %s; the subject of this census disappeared", censusBuilderFile)
	}
	if !report.scanned[censusStoreSetFile] {
		t.Fatalf("census never scanned %s; it cannot see a constructor reappearing beside the StoreSet type", censusStoreSetFile)
	}
	if report.candidates[censusBuilderFile] == 0 || report.candidates[censusPublicationFile] == 0 {
		t.Fatalf("census found no unpublished-candidate sites in the builder files: %v", report.candidates)
	}
	if len(report.mints) != len(censusSanctionedActivationFiles) {
		t.Fatalf("publication authority mint sites = %v, want exactly the sanctioned set %v", report.mints, censusSanctionedActivationFiles)
	}
	if len(report.publishes) != len(censusSanctionedActivationFiles) {
		t.Fatalf("publish call sites = %v, want exactly the sanctioned set %v", report.publishes, censusSanctionedActivationFiles)
	}
	for file := range censusSanctionedActivationFiles {
		if !report.scanned[file] {
			t.Fatalf("sanctioned activation file %s was never scanned; the sanctioned set names a file this census cannot see", file)
		}
	}
	if declarations := censusDeclarations(t, "newStoreSetPublicationAuthority"); declarations != 1 {
		t.Fatalf("publication authority has %d mints, want exactly one unexported mint", declarations)
	}
	if declarations := censusDeclarations(t, censusAssemblyName); declarations != 1 {
		t.Fatalf("%s has %d declarations, want exactly one; the census anchor moved", censusAssemblyName, declarations)
	}
	if declarations := censusDeclarations(t, censusRetiredAssemblyName); declarations != 0 {
		t.Fatalf("%s is declared again (%d sites); an exported constructor hands a usable StoreSet to anyone who can name it, including a go:linkname pull", censusRetiredAssemblyName, declarations)
	}
}

// TestStoreSetHasOneProducer pins the type-system invariant the rest of this
// boundary rests on: exactly one function populates a StoreSet, it seals the
// result into the candidate, and Publish is the only thing that hands one back.
// Reaching assembly — from a future consumer, from the composition root, or
// from a go:linkname pull that ignores exportedness entirely — therefore yields
// a value whose only method demands publication authority. The invariant itself
// is enforced by the compiler; this test exists so a later slice cannot add a
// second producer and quietly restore the shape before the plan was frozen.
func TestStoreSetHasOneProducer(t *testing.T) {
	literals := map[string]int{}
	producers := map[string]bool{}
	packageFiles := 0
	for _, source := range censusSources(t) {
		if !bytes.Contains(source.src, []byte(censusStoreSetType)) {
			continue
		}
		file := censusParse(t, source)
		if populated := censusPopulatedStoreSetLiterals(file); populated > 0 {
			literals[source.rel] += populated
		}
		if filepath.Dir(source.rel) != censusPackageDir {
			continue
		}
		packageFiles++
		for _, function := range censusStoreSetResults(file) {
			producers[function] = true
		}
	}

	if packageFiles < 5 {
		t.Fatalf("census scanned %d non-test files in %s; the package walk is broken", packageFiles, censusPackageDir)
	}
	for file, populated := range literals {
		if file != censusPublicationFile {
			t.Errorf("%s populates %d StoreSet literal(s); only %s assembles one, and it seals it into the candidate", file, populated, censusPublicationFile)
		}
	}
	if literals[censusPublicationFile] != 1 {
		t.Fatalf("%s populates %d StoreSet literals, want exactly one; the single assembly point is the whole guarantee", censusPublicationFile, literals[censusPublicationFile])
	}
	want := map[string]bool{"UnpublishedStoreSet.Publish": true}
	if len(producers) == 0 {
		t.Fatal("census found no function returning a StoreSet; the subject set of this census is empty")
	}
	for function := range producers {
		if !want[function] {
			t.Errorf("%s returns a StoreSet; only Publish may, because only Publish validates publication authority", function)
		}
	}
	for function := range want {
		if !producers[function] {
			t.Errorf("%s no longer returns a StoreSet; the census lost its anchor", function)
		}
	}
}

// TestNoLinknameOrUnsafeEscape bans the two mechanisms that void every
// compile-time guarantee above. A go:linkname pull binds a bodyless local
// declaration to another package's symbol by name: it needs no import, honors
// no exportedness, and is not type-checked, so it reaches an unexported
// assembler exactly as easily as an exported one — and a cmd/gc file doing it
// links into the full binary while every identifier census stays green. Banning
// the directive closes that, and it closes it for good rather than for one
// spelling: the compiler recognizes the directive in raw comment text, and a
// comment admits no escape sequence, no alias, and no import name to hide
// behind. The blank unsafe import a pull requires is banned with it.
func TestNoLinknameOrUnsafeEscape(t *testing.T) {
	scanned := 0
	for _, source := range censusSources(t) {
		if !censusSanctionedLinknameFiles[source.rel] {
			for _, directive := range censusLinknameDirectives(source) {
				t.Errorf("%s carries %q; go:linkname reaches any symbol in any package regardless of exportedness, so it defeats every guarantee the storage boundary makes", source.rel, directive)
			}
		}
		// Every file is parsed, with no token prefilter: an import path is a
		// string literal and can be escaped past any byte comparison, so a
		// prefilter here would be exactly the blindness this census exists to
		// remove.
		file := censusParse(t, source)
		scanned++
		if censusSanctionedUnsafeFiles[source.rel] {
			continue
		}
		for _, name := range censusUnsafeImports(file) {
			if name == "_" {
				t.Errorf("%s blank-imports unsafe; the only reason to do that is to enable a compiler directive, and the one that matters here is banned", source.rel)
				continue
			}
			// A named unsafe import is ordinary syscall or memory-layout code
			// until it comes near the storage boundary.
			if censusRelevant(source, censusPublicationTokens) {
				t.Errorf("%s imports unsafe as %q and names the storage boundary; a boundary guarded by the type system cannot also be reachable through pointer casts", source.rel, name)
			}
		}
	}
	if scanned < censusMinimumFiles {
		t.Fatalf("census parsed %d non-test Go files, want at least %d; the mechanism ban is blind", scanned, censusMinimumFiles)
	}
}

// TestCensusSeesEveryKnownEvasion is the blindness guard. Every shape below
// once passed some earlier version of this census, and each is pinned so it
// cannot pass again: the first census matched only unqualified in-package
// calls, so every qualified call site — including the cmd/gc wiring the next
// slice writes — went unseen; the second compared import literals as raw bytes,
// so a backquoted or escaped spelling of the same path went unseen in turn; the
// third bypassed the source graph altogether with a go:linkname pull, which
// needs no import at all and which no identifier census can ever see. The
// linkname shapes are detected lexically and deliberately without reading the
// directive's target: banning the mechanism is what makes this a closure rather
// than a fourth entry on a blocklist.
func TestCensusSeesEveryKnownEvasion(t *testing.T) {
	const mutant = "cmd/gc/storage_provider_bundle.go"
	assembly := func(t *testing.T, source censusSource) int {
		return censusPublicationSurvey(t, []censusSource{source}).assembly[source.rel]
	}
	publishes := func(t *testing.T, source censusSource) int {
		return censusPublicationSurvey(t, []censusSource{source}).publishes[source.rel]
	}
	candidates := func(t *testing.T, source censusSource) int {
		return censusPublicationSurvey(t, []censusSource{source}).candidates[source.rel]
	}
	linknames := func(_ *testing.T, source censusSource) int {
		return len(censusLinknameDirectives(source))
	}
	unsafeImports := func(t *testing.T, source censusSource) int {
		return len(censusUnsafeImports(censusParse(t, source)))
	}
	populated := func(t *testing.T, source censusSource) int {
		return censusPopulatedStoreSetLiterals(censusParse(t, source))
	}

	shapes := []struct {
		name   string
		rel    string
		src    string
		detect func(*testing.T, censusSource) int
	}{
		{
			name: "qualified cross-package call",
			src: `package main
import "` + censusImportPath + `"
func wire() { _, _ = storebinding.newUnpublishedStoreSet(fronts, generation, identities) }`,
			detect: assembly,
		},
		{
			name: "aliased import",
			src: `package main
import sb "` + censusImportPath + `"
func wire() { _, _ = sb.newUnpublishedStoreSet(fronts, generation, identities) }`,
			detect: assembly,
		},
		{
			name: "raw-string import literal",
			src: "package main\n" +
				"import sb `" + censusImportPath + "`\n" +
				"func wire() { _, _ = sb.newUnpublishedStoreSet(fronts, generation, identities) }",
			detect: assembly,
		},
		{
			name: "escaped import literal",
			src: `package main
import sb "` + censusEscapedImportPath + `"
func wire() { _, _ = sb.newUnpublishedStoreSet(fronts, generation, identities) }`,
			detect: assembly,
		},
		{
			name: "escaped import literal publishing",
			src: `package main
import sb "` + censusEscapedImportPath + `"
func wire(candidate sb.UnpublishedStoreSet, authority sb.StoreSetPublicationAuthority) {
	_, _ = candidate.Publish(authority)
}`,
			detect: publishes,
		},
		{
			name: "method value",
			src: `package main
import "` + censusImportPath + `"
var assemble = storebinding.newUnpublishedStoreSet`,
			detect: assembly,
		},
		{
			name: "dot import",
			src: `package main
import . "` + censusImportPath + `"
func wire() { _, _ = newUnpublishedStoreSet(fronts, generation, identities) }`,
			detect: assembly,
		},
		{
			name: "qualified publish",
			src: `package main
import "` + censusImportPath + `"
func wire(candidate storebinding.UnpublishedStoreSet, authority storebinding.StoreSetPublicationAuthority) {
	_, _ = candidate.Publish(authority)
}`,
			detect: publishes,
		},
		{
			name: "candidate type named outside the builder",
			src: `package main
import "` + censusImportPath + `"
func hold(candidate storebinding.UnpublishedStoreSet) storebinding.UnpublishedStoreSet { return candidate }`,
			detect: candidates,
		},
		{
			name: "go:linkname pull of the assembler",
			src: `package main
import (
	_ "unsafe"

	"` + censusImportPath + `"
)

//go:linkname pullAssemble ` + censusImportPath + `.newUnpublishedStoreSet
func pullAssemble(fronts any, generation storebinding.Generation, identities any) (storebinding.UnpublishedStoreSet, error)`,
			detect: linknames,
		},
		{
			name: "go:linkname push out of the storage package",
			rel:  censusPackageDir + "/leak.go",
			src: `package storebinding
import _ "unsafe"

//go:linkname newUnpublishedStoreSet example.com/elsewhere.grabAssembler
`,
			detect: linknames,
		},
		{
			name: "go:linkname indented and tab-separated",
			src: "package main\n" +
				"import _ \"unsafe\"\n" +
				"\t//go:linkname\tpull\tsomewhere.symbol\n",
			detect: linknames,
		},
		{
			name: "go:linkname naming a target the census cannot read",
			src: "package main\n" +
				"import _ \"unsafe\"\n" +
				"//go:linkname pull \"not.a.symbol.reference\"\n",
			detect: linknames,
		},
		{
			name:   "blank unsafe import",
			src:    "package main\n\nimport _ \"unsafe\"\n",
			detect: unsafeImports,
		},
		{
			name:   "escaped unsafe import literal",
			src:    "package main\n\nimport _ \"unsaf\\x65\"\n",
			detect: unsafeImports,
		},
		{
			name: "populated StoreSet literal",
			rel:  censusPackageDir + "/leak.go",
			src: `package storebinding
func leak(work WorkTopology) StoreSet { return StoreSet{work: work} }`,
			detect: populated,
		},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			rel := shape.rel
			if rel == "" {
				rel = mutant
			}
			source := censusSource{rel: rel, src: []byte(shape.src)}
			if got := shape.detect(t, source); got == 0 {
				t.Fatalf("census did not see the %s in %s", shape.name, rel)
			}
		})
	}

	t.Run("the linkname ban does not depend on reading the directive", func(t *testing.T) {
		// The pull fixture above spells the canonical import path. If the ban
		// ever narrowed to directives naming this package, a pull routed
		// through an alias symbol would walk straight past it.
		source := censusSource{rel: mutant, src: []byte("package main\n//go:linkname pull nothing.to.do.with.storage\n")}
		if got := censusLinknameDirectives(source); len(got) == 0 {
			t.Fatal("census reads the directive target before banning it; a pull can name any symbol, including one that forwards")
		}
	})

	t.Run("ordinary comments are not directives", func(t *testing.T) {
		source := censusSource{rel: mutant, src: []byte("package main\n// go:linkname is banned here\n// see //go:linkname? no\n")}
		if got := censusLinknameDirectives(source); len(got) != 0 {
			t.Fatalf("census flagged %v; only a comment that begins with the directive is one", got)
		}
	})

	t.Run("import literals normalize before comparison", func(t *testing.T) {
		if strings.Contains(censusEscapedImportPath, censusImportPath) {
			t.Fatalf("the escaped fixture %q still spells the canonical path; it proves nothing", censusEscapedImportPath)
		}
		spellings := []string{
			strconv.Quote(censusImportPath),
			"`" + censusImportPath + "`",
			`"` + censusEscapedImportPath + `"`,
		}
		for _, literal := range spellings {
			path, ok := censusImportLiteral(literal)
			if !ok || path != censusImportPath {
				t.Errorf("censusImportLiteral(%s) = %q, %v; every legal spelling denotes the census path", literal, path, ok)
			}
			file := &ast.File{Imports: []*ast.ImportSpec{{Path: &ast.BasicLit{Kind: token.STRING, Value: literal}}}}
			if !censusImports(file) {
				t.Errorf("census does not see an import spelled %s; that file leaves the subject set entirely", literal)
			}
		}
		unrelated := &ast.File{Imports: []*ast.ImportSpec{{Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote("fmt")}}}}
		if censusImports(unrelated) {
			t.Error("census counts a file importing only fmt as an importer of the storage package")
		}
		unreadable := &ast.File{Imports: []*ast.ImportSpec{{Path: &ast.BasicLit{Kind: token.STRING, Value: `"` + censusImportPath}}}}
		if !censusImports(unreadable) {
			t.Error("census treats an import literal it cannot normalize as a non-import; an unreadable import must fail closed")
		}
	})

	t.Run("prefilters do not depend on the import path spelling", func(t *testing.T) {
		tallied := func(sets ...map[string]bool) []string {
			var names []string
			for _, set := range sets {
				for name := range set {
					names = append(names, name)
				}
			}
			return names
		}
		groups := []struct {
			census string
			tokens []string
			names  []string
		}{
			{
				census: "publication",
				tokens: censusPublicationTokens,
				names:  tallied(censusCandidateTypes, censusAssemblyNames, censusMintNames, map[string]bool{censusPublishSelector: true}),
			},
			{
				census: "registry",
				tokens: censusRegistryTokens,
				names:  tallied(censusRegistryTypes, censusRegistryOperations),
			},
		}
		for _, group := range groups {
			for _, name := range group.names {
				covered := false
				for _, token := range group.tokens {
					if token != censusImportPath && strings.Contains(name, token) {
						covered = true
					}
				}
				if !covered {
					t.Errorf("the %s census tallies %s but no identifier token of its prefilter appears in that name; a file naming %s while importing the storage package under a non-canonical literal is dropped before it is parsed", group.census, name, name)
				}
			}
		}

		evasive := []struct {
			name   string
			tokens []string
			src    string
		}{
			{
				name:   "publication",
				tokens: censusPublicationTokens,
				src: `package main
import sb "` + censusEscapedImportPath + `"
func wire() { _, _ = sb.newUnpublishedStoreSet(fronts, generation, identities) }`,
			},
			{
				name:   "registry",
				tokens: censusRegistryTokens,
				src: `package main
import sb "` + censusEscapedImportPath + `"
func wire(registry *sb.ProviderRegistry) { _, _ = registry.Lookup(name) }`,
			},
		}
		for _, source := range evasive {
			candidate := censusSource{rel: mutant, src: []byte(source.src)}
			if bytes.Contains(candidate.src, []byte(censusImportPath)) {
				t.Fatalf("the %s fixture still contains the canonical import path; it does not exercise the prefilter", source.name)
			}
			if !censusRelevant(candidate, source.tokens) {
				t.Errorf("the %s prefilter skips a file that imports the storage package under an escaped literal", source.name)
			}
		}
	})

	t.Run("composition root is not an allowed assembly site", func(t *testing.T) {
		for file := range censusCompositionRootFiles {
			if file == censusBuilderFile || censusSanctionedActivationFiles[file] {
				t.Errorf("%s is both a composition root and an allowed assembly site; the census would exempt the wiring it exists to police", file)
			}
		}
	})

	t.Run("mint inside the package", func(t *testing.T) {
		mint := censusPackageDir + "/activation.go"
		src := `package storebinding
func activate() { _, _ = newStoreSetPublicationAuthority(attempt, generation, digest, descriptors, true) }`
		report := censusPublicationSurvey(t, []censusSource{{rel: mint, src: []byte(src)}})
		if report.mints[mint] == 0 {
			t.Fatalf("census did not see the in-package authority mint; report = %+v", report)
		}
	})

	t.Run("declarations and near misses do not count", func(t *testing.T) {
		sources := []censusSource{
			{rel: censusPublicationFile, src: []byte("package storebinding\nfunc newUnpublishedStoreSet() (UnpublishedStoreSet, error) { return UnpublishedStoreSet{}, nil }\n")},
			{rel: censusPublicationFile, src: []byte("package storebinding\nfunc newStoreSetPublicationAuthority() (StoreSetPublicationAuthority, error) { return StoreSetPublicationAuthority{}, nil }\n")},
			{rel: "cmd/gc/other.go", src: []byte("package main\nfunc wire() { _ = other.newUnpublishedStoreSet() }\n")},
			{rel: censusBuilderFile, src: []byte("package storebinding\nfunc build() { _, _ = NewStoreSetBuilder(plan) }\n")},
		}
		report := censusPublicationSurvey(t, sources)
		if len(report.assembly) != 0 {
			t.Errorf("census counted a declaration or a non-importing near miss as assembly: %v", report.assembly)
		}
		if len(report.mints) != 0 {
			t.Errorf("census counted the authority declaration as a mint: %v", report.mints)
		}
	})

	t.Run("an empty StoreSet literal is not a producer", func(t *testing.T) {
		source := censusSource{rel: censusPublicationFile, src: []byte("package storebinding\nfunc fail() (StoreSet, error) { return StoreSet{}, errBoom }\n")}
		if got := censusPopulatedStoreSetLiterals(censusParse(t, source)); got != 0 {
			t.Fatalf("census counted %d populated literals in an error return; a zero StoreSet serves nothing", got)
		}
	})
}

// TestSingleCompositionRoot keeps provider-registry construction and resolution
// inside the registry, the planner, and the cmd/gc provider bundle.
func TestSingleCompositionRoot(t *testing.T) {
	allowedRegistryFiles := map[string]bool{censusRegistryFile: true, censusPlannerFile: true}
	for file := range censusCompositionRootFiles {
		allowedRegistryFiles[file] = true
	}
	allowedOperationFiles := map[string]bool{censusRegistryFile: true, censusPlannerFile: true}

	for file := range censusCompositionRootFiles {
		allowedOperationFiles[file] = true
	}

	registrySites := map[string]int{}
	operationSites := map[string]int{}
	packageFiles := 0

	for _, source := range censusSources(t) {
		inPackage := filepath.Dir(source.rel) == censusPackageDir
		if !inPackage && !censusRelevant(source, censusRegistryTokens) {
			continue
		}
		file := censusParse(t, source)
		if inPackage {
			packageFiles++
		}
		for name, uses := range censusReferences(file, censusRegistryTypes) {
			registrySites[source.rel] += uses
			if !allowedRegistryFiles[source.rel] {
				t.Errorf("%s names %s; the registry is constructed and resolved only at the composition root and planner", source.rel, name)
			}
		}
		// Registry operations are censused wherever they can be reached: inside
		// the package and in every file that imports it under any name.
		if !inPackage && !censusImports(file) {
			continue
		}
		for _, call := range censusCalls(file) {
			if call.selector == "" || !censusRegistryOperations[call.name] {
				continue
			}
			operationSites[source.rel]++
			if !allowedOperationFiles[source.rel] {
				t.Errorf("%s calls .%s(; provider registration, freezing, and lookup stay in the registry and the planner", source.rel, call.name)
			}
		}
	}

	if packageFiles < 5 {
		t.Fatalf("census scanned %d non-test files in %s; the package walk is broken", packageFiles, censusPackageDir)
	}
	if registrySites[censusRegistryFile] == 0 || registrySites[censusPlannerFile] == 0 {
		t.Fatalf("registry census found no registry use in the registry or planner: %v", registrySites)
	}
	if operationSites[censusPlannerFile] == 0 {
		t.Fatalf("the planner performs no registry lookup; %s is no longer the exact-lookup site", censusPlannerFile)
	}
	if declarations := censusDeclarations(t, "NewProviderRegistry"); declarations != 1 {
		t.Fatalf("registry has %d constructors, want exactly one", declarations)
	}
}

// TestNoRuntimeRegistryAccess proves ordinary operations cannot reach a
// provider registry after boot: no consumer of this package names one, and the
// published StoreSet exposes only its six typed accessors.
func TestNoRuntimeRegistryAccess(t *testing.T) {
	consumers := 0
	for _, source := range censusSources(t) {
		if !censusRelevant(source, censusRegistryTokens) {
			continue
		}
		file := censusParse(t, source)
		if !censusImports(file) {
			continue
		}
		consumers++
		// The composition root may name the registry it constructs; it may
		// never assemble a StoreSet, which TestStoreSetPublicationSites
		// censuses module-wide with no composition-root exemption.
		if censusCompositionRootFiles[source.rel] {
			continue
		}
		for name := range censusReferences(file, censusRegistryTypes) {
			t.Errorf("%s imports the storage package and names %s; consumers select a typed StoreSet field, never a registry", source.rel, name)
		}
	}
	if consumers == 0 {
		t.Fatal("census found no consumer of the storage package; the subject set of this census is empty")
	}

	storeSet := reflect.TypeOf(StoreSet{})
	want := map[string]bool{"Work": true, "Graph": true, "Sessions": true, "Messaging": true, "Orders": true, "Nudges": true}
	if storeSet.NumMethod() != len(want) {
		t.Fatalf("StoreSet has %d exported methods, want exactly the %d typed accessors", storeSet.NumMethod(), len(want))
	}
	forbidden := map[reflect.Type]string{
		reflect.TypeOf(&ProviderRegistry{}): "provider registry",
		reflect.TypeOf(BindingSpec{}):       "binding specification",
		reflect.TypeOf(Descriptor{}):        "descriptor",
	}
	for index := 0; index < storeSet.NumMethod(); index++ {
		method := storeSet.Method(index)
		if !want[method.Name] {
			t.Errorf("StoreSet exposes %s; only the six typed class accessors belong on the published set", method.Name)
		}
		for argument := 0; argument < method.Type.NumIn(); argument++ {
			if label, bad := forbidden[method.Type.In(argument)]; bad {
				t.Errorf("StoreSet.%s accepts a %s", method.Name, label)
			}
		}
		for result := 0; result < method.Type.NumOut(); result++ {
			if label, bad := forbidden[method.Type.Out(result)]; bad {
				t.Errorf("StoreSet.%s returns a %s", method.Name, label)
			}
		}
	}
}

// TestUnpublishedStoreSetUnreachable proves the candidate and its authority
// expose no field and cross no exported signature except Build and Publish.
func TestUnpublishedStoreSetUnreachable(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(UnpublishedStoreSet{}), reflect.TypeOf(StoreSetPublicationAuthority{})} {
		if typ.NumField() == 0 {
			t.Fatalf("%s has no fields; this census would pass vacuously", typ)
		}
		for index := 0; index < typ.NumField(); index++ {
			if field := typ.Field(index); field.IsExported() {
				t.Errorf("%s.%s is exported; the candidate and its authority carry no reachable state", typ, field.Name)
			}
		}
	}
	candidate := reflect.TypeOf(UnpublishedStoreSet{})
	if candidate.NumMethod() != 1 || candidate.Method(0).Name != "Publish" {
		t.Fatalf("UnpublishedStoreSet exposes %d methods, want only Publish", candidate.NumMethod())
	}
	if authority := reflect.TypeOf(StoreSetPublicationAuthority{}); authority.NumMethod() != 0 {
		t.Fatalf("StoreSetPublicationAuthority exposes %d methods, want none", authority.NumMethod())
	}

	names := map[string]bool{"UnpublishedStoreSet": true, "StoreSetPublicationAuthority": true}
	crossings := map[string]bool{}
	packageFiles := 0
	for _, source := range censusSources(t) {
		if filepath.Dir(source.rel) != censusPackageDir {
			continue
		}
		packageFiles++
		if !bytes.Contains(source.src, []byte("StoreSet")) {
			continue
		}
		for _, function := range censusExportedSignatures(censusParse(t, source), names) {
			crossings[function] = true
		}
	}
	want := map[string]bool{"Build": true, "Publish": true}
	if packageFiles < 5 {
		t.Fatalf("census scanned %d non-test files in %s; the package walk is broken", packageFiles, censusPackageDir)
	}
	if len(crossings) == 0 {
		t.Fatal("census found no exported signature carrying the candidate; the subject set of this census is empty")
	}
	for function := range crossings {
		if !want[function] {
			t.Errorf("exported %s carries the unpublished candidate or its authority; only Build and Publish may", function)
		}
	}
	for function := range want {
		if !crossings[function] {
			t.Errorf("exported %s no longer carries the unpublished candidate; the census lost its anchor", function)
		}
	}
}

// censusPublicationReport is the raw finding set of the publication census.
// Policy — which files are sanctioned — lives with the caller so the blindness
// guard can exercise the same detection over synthetic sources.
type censusPublicationReport struct {
	subjects   int
	importers  int
	scanned    map[string]bool
	candidates map[string]int
	assembly   map[string]int
	mints      map[string]int
	publishes  map[string]int
}

// censusPublicationSurvey classifies every source that can reach this package:
// files inside it and files that import it, whatever name they import it
// under. Detection is by identifier reference rather than by unqualified call,
// so a package-qualified call, an aliased one, and a method value are all
// counted.
func censusPublicationSurvey(t *testing.T, sources []censusSource) censusPublicationReport {
	t.Helper()
	report := censusPublicationReport{
		scanned:    map[string]bool{},
		candidates: map[string]int{},
		assembly:   map[string]int{},
		mints:      map[string]int{},
		publishes:  map[string]int{},
	}
	for _, source := range sources {
		if !censusRelevant(source, censusPublicationTokens) {
			continue
		}
		file := censusParse(t, source)
		for _, uses := range censusReferences(file, censusCandidateTypes) {
			report.candidates[source.rel] += uses
		}

		inPackage := filepath.Dir(source.rel) == censusPackageDir
		imports := !inPackage && censusImports(file)
		if !inPackage && !imports {
			continue
		}
		report.subjects++
		report.scanned[source.rel] = true
		if imports {
			report.importers++
		}
		for _, uses := range censusReferences(file, censusAssemblyNames) {
			report.assembly[source.rel] += uses
		}
		if inPackage {
			for _, uses := range censusReferences(file, censusMintNames) {
				report.mints[source.rel] += uses
			}
		}
		for _, call := range censusCalls(file) {
			if call.selector != "" && call.name == censusPublishSelector {
				report.publishes[source.rel]++
			}
		}
	}
	return report
}

type censusSource struct {
	rel string
	src []byte
}

type censusCall struct {
	name     string
	selector string
}

func censusModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the storage package")
		}
		dir = parent
	}
}

// censusSources reads every non-test Go file in the module. It fails when the
// walk returns implausibly few files, so a broken walk cannot silently turn
// every census into a pass.
func censusSources(t *testing.T) []censusSource {
	t.Helper()
	sources := censusSourcesFrom(t, censusModuleRoot(t))
	if len(sources) < censusMinimumFiles {
		t.Fatalf("census scanned %d non-test Go files, want at least %d; the module walk is broken", len(sources), censusMinimumFiles)
	}
	return sources
}

// censusSourcesFrom reads every non-test Go file under root. censusSources
// runs it against the module root; a synthetic root (a fixture git
// repository, say) lets a test exercise the walk itself directly. Only
// git-tracked files are read, so an untracked nested git worktree checked out
// under root — the common gitignored worktrees/<bead> pool-slot pattern —
// never contributes duplicate source: its files live in that worktree's own
// index, never this one's. testdata is excluded, matching the walk this
// replaced.
func censusSourcesFrom(t *testing.T, root string) []censusSource {
	t.Helper()
	tracked, err := resourcecensus.TrackedGoFiles(root)
	if err != nil {
		t.Fatalf("listing tracked Go files under %s: %v", root, err)
	}
	var sources []censusSource
	for _, rel := range tracked {
		if strings.HasSuffix(rel, "_test.go") || censusUnderTestdata(rel) {
			continue
		}
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		sources = append(sources, censusSource{rel: rel, src: source})
	}
	return sources
}

// censusUnderTestdata reports whether rel is under a testdata directory. git
// tracks testdata by convention, unlike node_modules, vendor, .claude, and
// .git, which this module never tracks — so testdata is the one directory the
// prior filesystem walk excluded that a tracked-files listing does not.
func censusUnderTestdata(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if segment == "testdata" {
			return true
		}
	}
	return false
}

func censusParse(t *testing.T, source censusSource) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), source.rel, source.src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", source.rel, err)
	}
	return file
}

// censusReferences counts identifier uses of the named symbols, skipping the
// positions where a name is declared. Selector expressions are covered:
// storebinding.NewStoreSet reaches this counter through its Sel identifier, so
// qualified and aliased cross-package uses count like unqualified ones, while
// the declaration of NewStoreSet itself does not.
func censusReferences(file *ast.File, names map[string]bool) map[string]int {
	declarations := map[*ast.Ident]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch declaration := node.(type) {
		case *ast.FuncDecl:
			declarations[declaration.Name] = true
		case *ast.TypeSpec:
			declarations[declaration.Name] = true
		case *ast.ValueSpec:
			for _, name := range declaration.Names {
				declarations[name] = true
			}
		}
		return true
	})
	uses := map[string]int{}
	ast.Inspect(file, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && names[identifier.Name] && !declarations[identifier] {
			uses[identifier.Name]++
		}
		return true
	})
	return uses
}

func censusCalls(file *ast.File) []censusCall {
	var calls []censusCall
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch function := call.Fun.(type) {
		case *ast.Ident:
			calls = append(calls, censusCall{name: function.Name})
		case *ast.SelectorExpr:
			receiver := "?"
			if identifier, ok := function.X.(*ast.Ident); ok {
				receiver = identifier.Name
			}
			calls = append(calls, censusCall{name: function.Sel.Name, selector: receiver})
		}
		return true
	})
	return calls
}

// censusRelevant reports whether a source contains the raw bytes of anything a
// census keyed on these tokens could tally. It is only a prefilter: everything
// it admits is still parsed and classified. The tokens must be identifier
// substrings, never import paths — see censusPublicationTokens.
func censusRelevant(source censusSource, tokens []string) bool {
	for _, token := range tokens {
		if bytes.Contains(source.src, []byte(token)) {
			return true
		}
	}
	return false
}

// censusImportLiteral normalizes one import literal to the path it denotes.
// An import path is a Go string literal in any legal spelling — interpreted,
// raw, or escaped — so it is unquoted rather than quote-trimmed: a backquoted
// `…/internal/storebinding` and an escaped "…/internal/storebindin\x67" both
// name this package, and neither survives trimming `"` off the ends. ok is
// false for a literal strconv cannot unquote; callers fail closed on that.
func censusImportLiteral(literal string) (string, bool) {
	path, err := strconv.Unquote(literal)
	if err != nil {
		return "", false
	}
	return path, true
}

// censusImports reports whether the file imports path under any name and any
// spelling. A literal that will not normalize counts as a match: an import the
// census cannot read must never classify its file out of the subject set,
// because that is exactly how a pre-witness assembly site would hide.
func censusImports(file *ast.File) bool {
	const path = censusImportPath
	for _, imported := range file.Imports {
		normalized, ok := censusImportLiteral(imported.Path.Value)
		if !ok || normalized == path {
			return true
		}
	}
	return false
}

// censusLinknameDirectives returns every go:linkname directive in a source. The
// scan is lexical because the directive is lexical: the compiler matches raw
// comment text, so unlike an identifier or an import path there is no second
// spelling to miss. Leading whitespace is tolerated so the census is a strict
// superset of what the compiler accepts, and the directive's target is never
// read — a pull may name any symbol, including one that forwards, so the ban is
// on the mechanism rather than on a list of destinations.
func censusLinknameDirectives(source censusSource) []string {
	var directives []string
	for _, line := range strings.Split(string(source.src), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, censusLinknameDirective) {
			directives = append(directives, trimmed)
		}
	}
	return directives
}

// censusUnsafeImports returns the names a file imports unsafe under, using "_"
// for a blank import and "" for the default name. Literals are normalized
// first, so an escaped spelling of the path is seen exactly like the plain one.
func censusUnsafeImports(file *ast.File) []string {
	var names []string
	for _, imported := range file.Imports {
		path, ok := censusImportLiteral(imported.Path.Value)
		if !ok || path != censusUnsafeImportPath {
			continue
		}
		name := ""
		if imported.Name != nil {
			name = imported.Name.Name
		}
		names = append(names, name)
	}
	return names
}

// censusPopulatedStoreSetLiterals counts composite literals of the published
// type that set at least one field. A zero StoreSet{} is what every error path
// returns and serves nothing; a populated one is the whole prize, and outside
// this package the unexported fields make writing one a compile error.
func censusPopulatedStoreSetLiterals(file *ast.File) int {
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok || len(literal.Elts) == 0 {
			return true
		}
		if censusTypeName(literal.Type) == censusStoreSetType {
			count++
		}
		return true
	})
	return count
}

// censusStoreSetResults returns the functions and methods whose results include
// the published type, qualified by receiver so the caller can name the one
// producer it expects.
func censusStoreSetResults(file *ast.File) []string {
	var functions []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Type.Results == nil {
			continue
		}
		returns := false
		for _, result := range function.Type.Results.List {
			if censusTypeName(result.Type) == censusStoreSetType {
				returns = true
			}
		}
		if !returns {
			continue
		}
		name := function.Name.Name
		if function.Recv != nil && len(function.Recv.List) == 1 {
			name = censusTypeName(function.Recv.List[0].Type) + "." + name
		}
		functions = append(functions, name)
	}
	return functions
}

// censusTypeName reduces a type expression to the identifier it names, seeing
// through a qualifier and a pointer so a consumer's storebinding.StoreSet reads
// the same as an in-package StoreSet.
func censusTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	case *ast.StarExpr:
		return censusTypeName(typed.X)
	}
	return ""
}

// censusExportedSignatures returns the exported functions and methods whose
// parameters or results carry one of the named types.
func censusExportedSignatures(file *ast.File, names map[string]bool) []string {
	var functions []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || !function.Name.IsExported() {
			continue
		}
		carries := false
		ast.Inspect(function.Type, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok && names[identifier.Name] {
				carries = true
			}
			return true
		})
		if carries {
			functions = append(functions, function.Name.Name)
		}
	}
	return functions
}

func censusDeclarations(t *testing.T, name string) int {
	t.Helper()
	declarations := 0
	for _, source := range censusSources(t) {
		if !bytes.Contains(source.src, []byte(name)) {
			continue
		}
		for _, declaration := range censusParse(t, source).Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name {
				declarations++
			}
		}
	}
	return declarations
}
