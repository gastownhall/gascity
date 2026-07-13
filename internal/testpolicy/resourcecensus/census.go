// Package resourcecensus checks Gas City's declared test-resource debt against
// syntax-aware observations from tracked Go test files.
package resourcecensus

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Resource is a syntax-observable test resource.
type Resource string

const (
	// ResourceSubprocess counts direct os/exec command construction.
	ResourceSubprocess Resource = "subprocess"
	// ResourceFixedSleep counts direct time.Sleep calls.
	ResourceFixedSleep Resource = "fixed_sleep"
)

var knownResources = map[Resource]struct{}{
	ResourceSubprocess: {},
	ResourceFixedSleep: {},
}

// Scope selects the source population counted by a ledger row.
type Scope string

const (
	// ScopeAll includes every tracked Go test file.
	ScopeAll Scope = "all"
	// ScopeUntagged excludes explicitly and implicitly constrained files.
	ScopeUntagged Scope = "untagged"
)

type baselineKey struct {
	scope    Scope
	resource Resource
}

// Ledger is the checked source-level test-resource inventory.
type Ledger struct {
	Version       int        `toml:"version"`
	AuditBaseline []Baseline `toml:"audit_baseline"`
	Debt          []Baseline `toml:"debt"`
}

// Baseline pins one source-census signal and its migration ownership.
type Baseline struct {
	Scope           Scope    `toml:"scope"`
	Resource        Resource `toml:"resource"`
	BaselineCalls   int      `toml:"baseline_calls"`
	BaselineFiles   int      `toml:"baseline_files"`
	ReportedCalls   int      `toml:"reported_calls"`
	ReportedFiles   int      `toml:"reported_files"`
	OwnerBead       string   `toml:"owner_bead"`
	Invariant       string   `toml:"invariant"`
	ResourceOwner   string   `toml:"resource_owner"`
	MigrationTarget string   `toml:"migration_target"`
	Expires         string   `toml:"expires"`
}

var bootstrapPolicy = Ledger{
	Version: 1,
	AuditBaseline: []Baseline{
		{
			Scope:           ScopeAll,
			Resource:        ResourceSubprocess,
			BaselineCalls:   490,
			BaselineFiles:   135,
			ReportedCalls:   495,
			ReportedFiles:   135,
			OwnerBead:       "ga-80po0c.2",
			Invariant:       "tracked test source totals remain visible as audit evidence",
			ResourceOwner:   "ga-80po0c.2 owns this point-in-time source census",
			MigrationTarget: "P0.4a",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeAll,
			Resource:        ResourceFixedSleep,
			BaselineCalls:   443,
			BaselineFiles:   156,
			ReportedCalls:   447,
			ReportedFiles:   157,
			OwnerBead:       "ga-80po0c.2",
			Invariant:       "tracked test source totals remain visible as audit evidence",
			ResourceOwner:   "ga-80po0c.2 owns this point-in-time source census",
			MigrationTarget: "P0.4a",
			Expires:         "2026-10-01",
		},
	},
	Debt: []Baseline{
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceSubprocess,
			BaselineCalls:   374,
			BaselineFiles:   97,
			ReportedCalls:   380,
			ReportedFiles:   98,
			OwnerBead:       "ga-80po0c.2",
			Invariant:       "untagged subprocess call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each process-owning test removes or replaces its source call site",
			MigrationTarget: "D1/D2/D5/D6/E6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceFixedSleep,
			BaselineCalls:   291,
			BaselineFiles:   113,
			ReportedCalls:   295,
			ReportedFiles:   114,
			OwnerBead:       "ga-80po0c.2",
			Invariant:       "untagged fixed-sleep call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each owning test replaces elapsed wall time with its lifecycle signal",
			MigrationTarget: "W1-W5",
			Expires:         "2026-10-01",
		},
	},
}

// Occurrence is one syntax-owned resource use.
type Occurrence struct {
	Path     string
	Tagged   bool
	Resource Resource
}

// Census is a deterministic collection of resource occurrences.
type Census struct {
	Occurrences []Occurrence
}

// Count is the call-site and unique-file count for a scope/resource pair.
type Count struct {
	Calls int
	Files int
}

// Count returns the observed count for scope and resource.
func (c Census) Count(scope Scope, resource Resource) Count {
	files := map[string]struct{}{}
	count := Count{}
	for _, occurrence := range c.Occurrences {
		if occurrence.Resource != resource || !scopeContains(scope, occurrence) {
			continue
		}
		count.Calls++
		files[occurrence.Path] = struct{}{}
	}
	count.Files = len(files)
	return count
}

func scopeContains(scope Scope, occurrence Occurrence) bool {
	switch scope {
	case ScopeAll:
		return true
	case ScopeUntagged:
		return !occurrence.Tagged
	default:
		return false
	}
}

// ScanRepository scans the repository's tracked Go test files.
func ScanRepository(root string) (Census, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*_test.go")
	out, err := cmd.Output()
	if err != nil {
		return Census{}, fmt.Errorf("listing tracked Go tests: %w", err)
	}
	parts := strings.Split(string(out), "\x00")
	files := make([]string, 0, len(parts))
	for _, name := range parts {
		if name != "" {
			files = append(files, filepath.ToSlash(name))
		}
	}
	return scanFiles(os.DirFS(root), files)
}

// ScanFS scans every *_test.go file in sourceFS. It is intended for hermetic
// policy fixtures; repository checks use ScanRepository so untracked files do
// not perturb the checked baseline.
func ScanFS(sourceFS fs.FS) (Census, error) {
	var files []string
	err := fs.WalkDir(sourceFS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(name, "_test.go") {
			files = append(files, filepath.ToSlash(name))
		}
		return nil
	})
	if err != nil {
		return Census{}, fmt.Errorf("walking test source: %w", err)
	}
	return scanFiles(sourceFS, files)
}

type parsedFile struct {
	name   string
	tagged bool
}

type bindingInfo struct {
	uses map[*ast.Ident]types.Object
}

type emptyPackageImporter struct {
	packages map[string]*types.Package
}

func newEmptyPackageImporter() *emptyPackageImporter {
	return &emptyPackageImporter{packages: make(map[string]*types.Package)}
}

func (importer *emptyPackageImporter) Import(importPath string) (*types.Package, error) {
	if imported, ok := importer.packages[importPath]; ok {
		return imported, nil
	}
	imported := types.NewPackage(importPath, path.Base(importPath))
	imported.MarkComplete()
	importer.packages[importPath] = imported
	return imported, nil
}

// These sets mirror internal/syslist.KnownOS and KnownArch in the repository's
// pinned Go toolchain. Go owns them as the past, present, and future names used
// for filename matching. Scanning remains hermetic, so a toolchain update must
// review these code-owned copies.
var knownGOOS = map[string]struct{}{
	"aix": {}, "android": {}, "darwin": {}, "dragonfly": {},
	"freebsd": {}, "hurd": {}, "illumos": {}, "ios": {}, "js": {}, "linux": {},
	"nacl":   {},
	"netbsd": {}, "openbsd": {}, "plan9": {}, "solaris": {},
	"wasip1": {}, "windows": {}, "zos": {},
}

var knownGOARCH = map[string]struct{}{
	"386": {}, "amd64": {}, "amd64p32": {},
	"arm": {}, "armbe": {}, "arm64": {}, "arm64be": {},
	"loong64": {},
	"mips":    {}, "mipsle": {}, "mips64": {}, "mips64le": {},
	"mips64p32": {}, "mips64p32le": {},
	"ppc": {}, "ppc64": {}, "ppc64le": {},
	"riscv": {}, "riscv64": {},
	"s390": {}, "s390x": {},
	"sparc": {}, "sparc64": {},
	"wasm": {},
}

func scanFiles(sourceFS fs.FS, names []string) (Census, error) {
	sort.Strings(names)
	census := Census{}
	importer := newEmptyPackageImporter()
	for index, name := range names {
		data, err := fs.ReadFile(sourceFS, name)
		if err != nil {
			return Census{}, fmt.Errorf("reading %s: %w", name, err)
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, name, data, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return Census{}, fmt.Errorf("parsing %s: %w", name, err)
		}
		tagged, err := parsedBuildConstraint(data)
		if err != nil {
			return Census{}, fmt.Errorf("parsing build constraint in %s: %w", name, err)
		}
		if err := validateImports(file); err != nil {
			return Census{}, fmt.Errorf("scanning imports in %s: %w", name, err)
		}
		source := parsedFile{
			name:   filepath.ToSlash(name),
			tagged: tagged || hasImplicitPlatformConstraint(name),
		}
		candidates := resourceCandidateCalls(file)
		if len(candidates) == 0 {
			continue
		}
		bindings := resolveBindings(fileSet, file, importer, fmt.Sprintf("resourcecensus.local/file%d", index))
		for _, call := range candidates {
			matched, err := isImportedCall(call, bindings, "os/exec", "Command", "CommandContext")
			if err != nil {
				return Census{}, fmt.Errorf("scanning resource calls in %s: %w", name, err)
			}
			if matched {
				census.add(source, ResourceSubprocess)
				continue
			}
			matched, err = isImportedCall(call, bindings, "time", "Sleep")
			if err != nil {
				return Census{}, fmt.Errorf("scanning resource calls in %s: %w", name, err)
			}
			if matched {
				census.add(source, ResourceFixedSleep)
			}
		}
	}

	sort.Slice(census.Occurrences, func(i, j int) bool {
		left, right := census.Occurrences[i], census.Occurrences[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Resource < right.Resource
	})
	return census, nil
}

func (c *Census) add(source parsedFile, resource Resource) {
	c.Occurrences = append(c.Occurrences, Occurrence{
		Path:     source.name,
		Tagged:   source.tagged,
		Resource: resource,
	})
}

func parsedBuildConstraint(content []byte) (bool, error) {
	// Match go/build: one UTF-8 BOM is permitted only at the start of a Go
	// source file and is removed before the leading build header is parsed.
	content = bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf})
	header, goBuild, err := leadingBuildHeader(content)
	if err != nil {
		return false, err
	}
	if goBuild != nil {
		if _, err := constraint.Parse(string(goBuild)); err != nil {
			return false, err
		}
		return true, nil
	}
	for len(header) > 0 {
		line := header
		if index := bytes.IndexByte(line, '\n'); index >= 0 {
			line, header = line[:index], header[index+1:]
		} else {
			header = nil
		}
		text := string(bytes.TrimSpace(line))
		if !constraint.IsPlusBuild(text) {
			continue
		}
		// go/build ignores malformed legacy constraints.
		if _, err := constraint.Parse(text); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// leadingBuildHeader mirrors the placement rules in go/build.parseFileHeader:
// modern constraints may appear before the package clause, while legacy
// constraints must precede the last separating blank in the leading // block.
func leadingBuildHeader(content []byte) (header, goBuild []byte, err error) {
	end := 0
	rest := content
	ended := false
	inBlock := false

Lines:
	for len(rest) > 0 {
		line := rest
		if index := bytes.IndexByte(line, '\n'); index >= 0 {
			line, rest = line[:index], rest[index+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			end = len(content) - len(rest)
			continue
		}
		if !bytes.HasPrefix(line, []byte("//")) {
			ended = true
		}
		if !inBlock && constraint.IsGoBuild(string(line)) {
			if goBuild != nil {
				return nil, nil, errors.New("multiple //go:build comments")
			}
			goBuild = line
		}

		for len(line) > 0 {
			if inBlock {
				if index := bytes.Index(line, []byte("*/")); index >= 0 {
					inBlock = false
					line = bytes.TrimSpace(line[index+2:])
					continue
				}
				continue Lines
			}
			switch {
			case bytes.HasPrefix(line, []byte("//")):
				continue Lines
			case bytes.HasPrefix(line, []byte("/*")):
				inBlock = true
				line = bytes.TrimSpace(line[2:])
			default:
				break Lines
			}
		}
	}
	return content[:end], goBuild, nil
}

func hasImplicitPlatformConstraint(name string) bool {
	base := path.Base(filepath.ToSlash(name))
	stem, _, _ := strings.Cut(base, ".")
	stem = strings.TrimSuffix(stem, "_test")
	parts := strings.Split(stem, "_")
	if len(parts) < 2 {
		return false
	}
	last := parts[len(parts)-1]
	if _, ok := knownGOOS[last]; ok {
		return true
	}
	_, ok := knownGOARCH[last]
	return ok
}

func validateImports(file *ast.File) error {
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("decoding import path %s: %w", spec.Path.Value, err)
		}
		if spec.Name != nil && spec.Name.Name == "_" {
			continue
		}
		if spec.Name != nil && spec.Name.Name == "." {
			if importPath == "os/exec" || importPath == "time" {
				return fmt.Errorf("targeted dot import %q cannot be counted safely", importPath)
			}
		}
	}
	return nil
}

func resourceCandidateCalls(file *ast.File) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "Command", "CommandContext", "Sleep":
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func resolveBindings(fileSet *token.FileSet, file *ast.File, importer types.Importer, packagePath string) bindingInfo {
	info := bindingInfo{uses: make(map[*ast.Ident]types.Object)}
	config := types.Config{
		Importer:                 importer,
		DisableUnusedImportCheck: true,
		IgnoreFuncBodies:         false,
		Error:                    func(error) {},
	}
	_, _ = config.Check(packagePath, fileSet, []*ast.File{file}, &types.Info{Uses: info.uses})
	return info
}

func isImportedCall(call *ast.CallExpr, bindings bindingInfo, importPath string, names ...string) (bool, error) {
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false, nil
	}
	identifier, ok := unparen(selector.X).(*ast.Ident)
	if !ok {
		return false, nil
	}
	matchedName := false
	for _, name := range names {
		if selector.Sel.Name == name {
			matchedName = true
			break
		}
	}
	if !matchedName {
		return false, nil
	}
	binding, ok := bindings.uses[identifier]
	if !ok || binding == nil {
		return false, fmt.Errorf("resource candidate qualifier %q has no lexical binding", identifier.Name)
	}
	packageName, ok := binding.(*types.PkgName)
	if !ok {
		return false, nil
	}
	imported := packageName.Imported()
	if imported == nil {
		return false, fmt.Errorf("resource candidate qualifier %q has unusable package binding for %q", identifier.Name, importPath)
	}
	return imported.Path() == importPath, nil
}

func unparen(expression ast.Expr) ast.Expr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = parenthesized.X
	}
}

// ParseLedger decodes a ledger and rejects undeclared fields.
func ParseLedger(data []byte) (Ledger, error) {
	var ledger Ledger
	metadata, err := toml.Decode(string(data), &ledger)
	if err != nil {
		return Ledger{}, fmt.Errorf("decode resource ledger: %w", err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		fields := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			fields = append(fields, key.String())
		}
		sort.Strings(fields)
		return Ledger{}, fmt.Errorf("unknown ledger field: %s", strings.Join(fields, ", "))
	}
	return ledger, nil
}

// LoadLedger loads a checked resource ledger from disk.
func LoadLedger(name string) (Ledger, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return Ledger{}, err
	}
	return ParseLedger(data)
}

// Validate checks schema ownership, expiration, and exact census baselines.
func Validate(ledger Ledger, census Census, now time.Time) error {
	return validateAgainstPolicy(bootstrapPolicy, ledger, census, now)
}

func validateAgainstPolicy(policy, ledger Ledger, census Census, now time.Time) error {
	if problems := validateManifestAgainstPolicy(policy, ledger, now); len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "\n"))
	}

	var problems []string
	for _, baseline := range ledger.AuditBaseline {
		prefix := fmt.Sprintf("audit baseline scope=%s resource=%s", baseline.Scope, baseline.Resource)
		problems = append(problems, validateBaseline(prefix, baseline, census)...)
	}
	for _, debt := range ledger.Debt {
		prefix := fmt.Sprintf("debt baseline scope=%s resource=%s", debt.Scope, debt.Resource)
		problems = append(problems, validateBaseline(prefix, debt, census)...)
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(strings.Join(problems, "\n"))
}

func validateManifestAgainstPolicy(policy, ledger Ledger, now time.Time) []string {
	var problems []string
	if policy.Version != 1 {
		problems = append(problems, fmt.Sprintf("bootstrap policy version = %d, want 1", policy.Version))
	}
	if ledger.Version != policy.Version {
		problems = append(problems, fmt.Sprintf("ledger version = %d, bootstrap policy requires %d", ledger.Version, policy.Version))
	}
	problems = append(problems, validateRowsAgainstPolicy("audit", policy.AuditBaseline, ledger.AuditBaseline, now)...)
	problems = append(problems, validateRowsAgainstPolicy("debt", policy.Debt, ledger.Debt, now)...)
	return problems
}

func validateRowsAgainstPolicy(kind string, policyRows, ledgerRows []Baseline, now time.Time) []string {
	var problems []string
	policyByKey := map[baselineKey]Baseline{}
	for _, row := range policyRows {
		key := baselineKey{row.Scope, row.Resource}
		prefix := fmt.Sprintf("bootstrap %s baseline scope=%s resource=%s", kind, row.Scope, row.Resource)
		if _, exists := policyByKey[key]; exists {
			problems = append(problems, fmt.Sprintf("duplicate bootstrap %s baseline: scope=%s resource=%s", kind, row.Scope, row.Resource))
		}
		policyByKey[key] = row
		problems = append(problems, validateBaselineDefinition(prefix, row, now)...)
	}

	seen := map[baselineKey]bool{}
	for _, row := range ledgerRows {
		key := baselineKey{row.Scope, row.Resource}
		prefix := fmt.Sprintf("%s baseline scope=%s resource=%s", kind, row.Scope, row.Resource)
		if seen[key] {
			problems = append(problems, fmt.Sprintf("duplicate %s baseline: scope=%s resource=%s", kind, row.Scope, row.Resource))
		}
		seen[key] = true
		problems = append(problems, validateBaselineDefinition(prefix, row, now)...)
		want, exists := policyByKey[key]
		if !exists {
			problems = append(problems, fmt.Sprintf("unexpected %s baseline: scope=%s resource=%s", kind, row.Scope, row.Resource))
			continue
		}
		problems = append(problems, comparePolicyFields(prefix, row, want)...)
	}
	for key := range policyByKey {
		if !seen[key] {
			problems = append(problems, fmt.Sprintf("missing required %s baseline: scope=%s resource=%s", kind, key.scope, key.resource))
		}
	}
	return problems
}

func comparePolicyFields(prefix string, got, want Baseline) []string {
	var problems []string
	for _, field := range []struct {
		name      string
		got, want int
	}{
		{"baseline_calls", got.BaselineCalls, want.BaselineCalls},
		{"baseline_files", got.BaselineFiles, want.BaselineFiles},
		{"reported_calls", got.ReportedCalls, want.ReportedCalls},
		{"reported_files", got.ReportedFiles, want.ReportedFiles},
	} {
		if field.got != field.want {
			problems = append(problems, fmt.Sprintf("%s: %s = %d, bootstrap policy requires %d", prefix, field.name, field.got, field.want))
		}
	}
	for _, field := range []struct {
		name      string
		got, want string
	}{
		{"owner_bead", got.OwnerBead, want.OwnerBead},
		{"invariant", got.Invariant, want.Invariant},
		{"resource_owner", got.ResourceOwner, want.ResourceOwner},
		{"migration_target", got.MigrationTarget, want.MigrationTarget},
		{"expires", got.Expires, want.Expires},
	} {
		if field.got != field.want {
			problems = append(problems, fmt.Sprintf("%s: %s = %q, bootstrap policy requires %q", prefix, field.name, field.got, field.want))
		}
	}
	return problems
}

func validateBaselineDefinition(prefix string, row Baseline, now time.Time) []string {
	var problems []string
	if !knownScope(row.Scope) {
		problems = append(problems, fmt.Sprintf("%s: unknown scope %q", prefix, row.Scope))
	}
	if _, ok := knownResources[row.Resource]; !ok {
		problems = append(problems, fmt.Sprintf("%s: unknown resource %q", prefix, row.Resource))
	}
	if row.BaselineCalls < 0 || row.BaselineFiles < 0 {
		problems = append(problems, prefix+": baselines must be non-negative")
	}
	if row.ReportedCalls < 0 || row.ReportedFiles < 0 {
		problems = append(problems, prefix+": historical census must be non-negative")
	}
	problems = append(problems, validateOwnership(prefix, row, now)...)
	return problems
}

func validateBaseline(prefix string, row Baseline, census Census) []string {
	if row.BaselineCalls < 0 || row.BaselineFiles < 0 {
		return []string{prefix + ": baselines must be non-negative"}
	}
	actual := census.Count(row.Scope, row.Resource)
	switch {
	case actual.Calls > row.BaselineCalls || actual.Files > row.BaselineFiles:
		return []string{fmt.Sprintf("source resource census grew: scope=%s resource=%s calls=%d (baseline %d), files=%d (baseline %d)", row.Scope, row.Resource, actual.Calls, row.BaselineCalls, actual.Files, row.BaselineFiles)}
	case actual.Calls < row.BaselineCalls || actual.Files < row.BaselineFiles:
		return []string{fmt.Sprintf("source resource census baseline is stale: scope=%s resource=%s calls=%d (baseline %d), files=%d (baseline %d); lower the checked baseline to bank the improvement", row.Scope, row.Resource, actual.Calls, row.BaselineCalls, actual.Files, row.BaselineFiles)}
	default:
		return nil
	}
}

func knownScope(scope Scope) bool {
	return scope == ScopeAll || scope == ScopeUntagged
}

func validateOwnership(prefix string, row Baseline, now time.Time) []string {
	var problems []string
	for name, value := range map[string]string{
		"owner_bead":       row.OwnerBead,
		"invariant":        row.Invariant,
		"resource_owner":   row.ResourceOwner,
		"migration_target": row.MigrationTarget,
	} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Sprintf("%s: %s is required", prefix, name))
		}
	}
	expiry, err := time.Parse("2006-01-02", row.Expires)
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s: expiry %q must use YYYY-MM-DD", prefix, row.Expires))
	} else if expiry.Before(day(now)) {
		problems = append(problems, fmt.Sprintf("%s: expired %s", prefix, row.Expires))
	}
	return problems
}

func day(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

// RenderMarkdown renders the exact checked TESTING.md inventory block.
func RenderMarkdown(ledger Ledger) string {
	type row struct {
		kind      string
		scope     string
		baseline  string
		owner     string
		invariant string
		migration string
		expiry    string
	}
	var rows []row
	appendRows := func(kind string, baselines []Baseline) {
		for _, baseline := range baselines {
			rows = append(rows, row{
				kind:      kind,
				scope:     renderedSourceScope(baseline.Scope),
				baseline:  renderedBaseline(baseline),
				owner:     baseline.OwnerBead,
				invariant: baseline.Invariant + "; " + baseline.ResourceOwner,
				migration: baseline.MigrationTarget,
				expiry:    baseline.Expires,
			})
		}
	}
	appendRows("Audit baseline", ledger.AuditBaseline)
	appendRows("Source debt ratchet", ledger.Debt)
	sort.Slice(rows, func(i, j int) bool {
		left := rows[i].kind + "\x00" + rows[i].scope + "\x00" + rows[i].baseline
		right := rows[j].kind + "\x00" + rows[j].scope + "\x00" + rows[j].baseline
		return left < right
	})

	var output strings.Builder
	output.WriteString(markdownBegin)
	output.WriteString("\n| Ledger kind | Source scope | Resource baseline | Tracking owner | Invariant / resource owner | Migration | Expiry |\n")
	output.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, row := range rows {
		fmt.Fprintf(&output, "| %s | %s | %s | %s | %s | %s | %s |\n",
			row.kind, row.scope, row.baseline, row.owner, row.invariant, row.migration, row.expiry)
	}
	output.WriteString(markdownEnd)
	return output.String()
}

func renderedSourceScope(scope Scope) string {
	switch scope {
	case ScopeAll:
		return "all tracked test source"
	case ScopeUntagged:
		return "all untagged test source"
	default:
		return string(scope)
	}
}

func renderedBaseline(row Baseline) string {
	result := fmt.Sprintf("%s: %d calls / %d files", row.Resource, row.BaselineCalls, row.BaselineFiles)
	if row.ReportedCalls != 0 && (row.ReportedCalls != row.BaselineCalls || row.ReportedFiles != row.BaselineFiles) {
		result += fmt.Sprintf(" (historical regex census: %d / %d)", row.ReportedCalls, row.ReportedFiles)
	}
	return result
}

const (
	markdownBegin = "<!-- BEGIN CHECKED TEST RESOURCE LEDGER -->"
	markdownEnd   = "<!-- END CHECKED TEST RESOURCE LEDGER -->"
)

// CheckedMarkdownBlock returns the single generated inventory block.
func CheckedMarkdownBlock(document string) (string, error) {
	if strings.Count(document, markdownBegin) != 1 || strings.Count(document, markdownEnd) != 1 {
		return "", errors.New("TESTING.md must contain exactly one checked test resource ledger marker pair")
	}
	start := strings.Index(document, markdownBegin)
	end := strings.Index(document, markdownEnd)
	if end < start {
		return "", errors.New("TESTING.md resource ledger end marker precedes begin marker")
	}
	end += len(markdownEnd)
	return document[start:end], nil
}
