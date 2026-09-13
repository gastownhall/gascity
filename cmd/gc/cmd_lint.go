package main

import (
	"bytes"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/promptmeta"
	"github.com/spf13/cobra"
)

type lintReport struct {
	SchemaVersion string           `json:"schema_version"`
	OK            bool             `json:"ok"`
	Passed        bool             `json:"passed"`
	ErrorCount    int              `json:"error_count"`
	Packs         []lintPackReport `json:"packs"`
}

type lintPackReport struct {
	Path        string           `json:"path"`
	Name        string           `json:"name,omitempty"`
	OK          bool             `json:"ok"`
	Diagnostics []lintDiagnostic `json:"diagnostics,omitempty"`
}

type lintDiagnostic struct {
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

type lintPromptTarget struct {
	templatePath string
	sourcePath   string
	agent        config.Agent
}

func newLintCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "lint <pack>",
		Short: "Validate a pack before merge",
		Long: strings.TrimSpace(`Validate a pack before merge.

gc lint <pack> validates the pack.toml file, reports non-fatal loader
warnings, and parses prompt templates with the same missing-key behavior used
by runtime prompt rendering. Use gc lint . to recursively find every pack.toml
below the current directory.`),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return exitForCode(doLint(args[0], jsonOut, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit structured JSON report")
	return cmd
}

func doLint(target string, jsonOut bool, stdout, stderr io.Writer) int {
	report := lintTarget(target)
	if jsonOut {
		if err := writeCLIJSONLine(stdout, report); err != nil {
			fmt.Fprintf(stderr, "gc lint: encode JSON: %v\n", err) //nolint:errcheck
			return 1
		}
	} else {
		writeLintHuman(stdout, stderr, report)
	}
	if report.Passed {
		return 0
	}
	return 1
}

func lintTarget(target string) lintReport {
	report := lintReport{
		SchemaVersion: "2",
		OK:            true,
		Passed:        true,
		Packs:         []lintPackReport{},
	}
	packDirs, diagnostics := lintPackDirs(target)
	if len(diagnostics) > 0 {
		report.Passed = false
		report.ErrorCount = len(diagnostics)
		report.Packs = append(report.Packs, lintPackReport{
			Path:        target,
			OK:          false,
			Diagnostics: diagnostics,
		})
		return report
	}
	for _, dir := range packDirs {
		packReport := lintPack(dir)
		report.Packs = append(report.Packs, packReport)
		report.ErrorCount += lintErrorCount(packReport.Diagnostics)
	}
	report.Passed = report.ErrorCount == 0
	return report
}

func lintPackDirs(target string) ([]string, []lintDiagnostic) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, []lintDiagnostic{newLintDiagnostic("", 0, "pack path is required")}
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return nil, []lintDiagnostic{newLintDiagnostic(target, 0, fmt.Sprintf("resolve path: %v", err))}
	}
	if target != "." {
		packPath := filepath.Join(absTarget, "pack.toml")
		if info, err := os.Stat(packPath); err != nil {
			return nil, []lintDiagnostic{newLintDiagnostic(packPath, 0, fmt.Sprintf("stat pack.toml: %v", err))}
		} else if info.IsDir() {
			return nil, []lintDiagnostic{newLintDiagnostic(packPath, 0, "pack.toml is a directory")}
		}
		return []string{absTarget}, nil
	}
	packDirs, err := findPackDirs(absTarget)
	if err != nil {
		return nil, []lintDiagnostic{newLintDiagnostic(absTarget, 0, err.Error())}
	}
	if len(packDirs) == 0 {
		return nil, []lintDiagnostic{newLintDiagnostic(absTarget, 0, "no pack.toml files found")}
	}
	return packDirs, nil
}

func findPackDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && lintSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "pack.toml" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return dirs, nil
}

func lintSkipDir(name string) bool {
	switch name {
	case ".git", ".gc", ".beads", "node_modules":
		return true
	default:
		return false
	}
}

func lintPack(packDir string) lintPackReport {
	out := lintPackReport{Path: packDir, OK: true}
	loaded, err := config.LoadPackForLint(fsys.OSFS{}, packDir)
	if err != nil {
		packPath := filepath.Join(packDir, "pack.toml")
		out.Diagnostics = append(out.Diagnostics, diagnosticFromError(packPath, err))
		out.OK = false
		return out
	}
	out.Name = loaded.Name
	for _, warning := range loaded.Warnings {
		out.Diagnostics = append(out.Diagnostics, diagnosticFromWarning(filepath.Join(packDir, "pack.toml"), warning))
	}
	out.Diagnostics = append(out.Diagnostics, lintNamedSessionPoolConflicts(filepath.Join(packDir, "pack.toml"), loaded)...)
	out.Diagnostics = append(out.Diagnostics, lintFormulaFiles(packDir)...)
	targets, diagnostics := collectLintPromptTargets(packDir, loaded)
	out.Diagnostics = append(out.Diagnostics, diagnostics...)
	packDirs, cityPackDirs := lintFragmentSearchDirs(packDir, loaded.PackDirs)
	for _, target := range targets {
		out.Diagnostics = append(out.Diagnostics, lintPrompt(packDir, packDirs, cityPackDirs, loaded.Providers, target)...)
	}
	out.Diagnostics = append(out.Diagnostics, lintClaudeOverlayHookShape(packDir)...)
	out.OK = lintErrorCount(out.Diagnostics) == 0
	return out
}

// lintFragmentSearchDirs splits the directories a pack's prompts may resolve
// fragments from into the pack's own recursively-included PackDirs and the
// extra dirs its hosting city composes in at the rig level (city.toml's
// per-rig "includes"). A pack can depend on fragments from a sibling pack
// purely through that city-level composition without declaring the sibling in
// its own pack.toml — the runtime resolves such fragments fine (resolveTemplate
// in template_resolve.go calls city.PackDirsForRig(rigName)), but a standalone
// pack load (config.LoadPackForLint) never reads city.toml, so lint reported
// those fragments "not found" even though they render correctly.
//
// Two rules keep that widening from making lint lie or leak:
//
//   - City dirs are scoped to the rigs that actually contain this pack. A
//     fragment that exists only in an unrelated rig's pack is NOT reachable at
//     runtime for this pack's agents, so unioning every rig (PackDirsForRig(""))
//     would turn a real missing-fragment error into a false pass. Only rigs
//     whose composed dirs include packDir contribute. A pack that belongs to no
//     rig — standalone, or city-root — has no rig to scope to and keeps the
//     rig-less fallback the runtime uses for scope=city agents:
//     PackDirsForRig("").
//   - City dirs contribute fragments, not diagnostics. gc lint <pack> must
//     report on <pack>; a malformed fragment in a neighboring pack is already
//     that neighbour's lint failure, so parse failures under these dirs are
//     recorded at warning severity and do not fail this pack.
//
// The city load is deliberately best-effort: it is advisory, so on
// config.ErrRepoCacheBusy (another gc process holds the machine-wide repo-cache
// lock through a network clone) lint degrades to the pack's own dirs rather
// than blocking on that lock. When no city is discoverable from packDir, or it
// fails to load, the city-only list is empty and lint behaves exactly as it
// does for a pack with no city at all.
func lintFragmentSearchDirs(packDir string, ownPackDirs []string) (own []string, cityOnly []string) {
	cityPath, err := findCity(packDir)
	if err != nil {
		return ownPackDirs, nil
	}
	cfg, err := loadCityConfigAdvisory(cityPath)
	if err != nil || cfg == nil {
		return ownPackDirs, nil
	}
	known := make(map[string]struct{}, len(ownPackDirs))
	for _, dir := range ownPackDirs {
		known[pathutil.NormalizePathForCompare(dir)] = struct{}{}
	}
	for _, dir := range lintCityComposedPackDirs(cfg, packDir) {
		key := pathutil.NormalizePathForCompare(dir)
		if _, seen := known[key]; seen {
			continue
		}
		known[key] = struct{}{}
		cityOnly = append(cityOnly, dir)
	}
	return ownPackDirs, cityOnly
}

// lintCityComposedPackDirs returns the pack dirs cfg composes in for every rig
// that contains packDir, unioned in sorted rig-name order for determinism.
// Paths are compared through pathutil.NormalizePathForCompare because findCity
// canonicalizes the city root the same way, so a symlinked or /private-aliased
// ancestor cannot cause a rig to miss its own pack. When no rig contains
// packDir this falls back to cfg.PackDirsForRig(""), the rig-less union.
func lintCityComposedPackDirs(cfg *config.City, packDir string) []string {
	target := pathutil.NormalizePathForCompare(packDir)
	rigNames := make([]string, 0, len(cfg.RigPackDirs))
	for name := range cfg.RigPackDirs {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)

	var dirs []string
	var matched bool
	for _, rigName := range rigNames {
		for _, dir := range cfg.RigPackDirs[rigName] {
			if pathutil.NormalizePathForCompare(dir) != target {
				continue
			}
			matched = true
			dirs = appendUniqueStrings(dirs, cfg.PackDirsForRig(rigName)...)
			break
		}
	}
	if !matched {
		return cfg.PackDirsForRig("")
	}
	return dirs
}

func lintNamedSessionPoolConflicts(packPath string, loaded *config.LintPackLoad) []lintDiagnostic {
	if loaded == nil || len(loaded.NamedSessions) == 0 || len(loaded.Agents) == 0 {
		return nil
	}
	agentsByName := make(map[string]config.Agent, len(loaded.Agents))
	for _, agentCfg := range loaded.Agents {
		agentsByName[agentCfg.QualifiedName()] = agentCfg
	}
	var diagnostics []lintDiagnostic
	for _, named := range loaded.NamedSessions {
		agentCfg, ok := agentsByName[named.TemplateQualifiedName()]
		if !ok || !agentHasPoolControls(agentCfg) {
			continue
		}
		diagnostics = append(diagnostics, newLintDiagnostic(packPath, 0,
			fmt.Sprintf("named_session %q targets pool-controlled agent %q; remove pool settings from named-session templates or remove [[named_session]] for pool agents",
				named.QualifiedName(), agentCfg.QualifiedName())))
	}
	return diagnostics
}

func agentHasPoolControls(agentCfg config.Agent) bool {
	if agentPoolExplicitlyDisabled(agentCfg) {
		return false
	}
	if agentIsNamedSessionSingleton(agentCfg) {
		return false
	}
	return agentCfg.MinActiveSessions != nil ||
		agentCfg.MaxActiveSessions != nil ||
		strings.TrimSpace(agentCfg.ScaleCheck) != "" ||
		strings.TrimSpace(agentCfg.Namepool) != "" ||
		len(agentCfg.NamepoolNames) > 0
}

// agentIsNamedSessionSingleton reports whether agentCfg is the named-session
// flavor of max_active_sessions=1 documented by
// (*config.Agent).SupportsInstanceExpansion: max=1 with no
// min_active_sessions, no scale_check, and no namepool is a singleton with a
// stable canonical identity, not a pool — a [[named_session]] targeting it is
// the supported configuration, not a conflict. Pool flavors of max=1 (an
// explicit MinActiveSessions or a ScaleCheck) keep pool semantics and stay
// flagged.
func agentIsNamedSessionSingleton(agentCfg config.Agent) bool {
	return agentCfg.MaxActiveSessions != nil && *agentCfg.MaxActiveSessions == 1 &&
		agentCfg.MinActiveSessions == nil &&
		strings.TrimSpace(agentCfg.ScaleCheck) == "" &&
		strings.TrimSpace(agentCfg.Namepool) == "" &&
		len(agentCfg.NamepoolNames) == 0
}

// agentPoolExplicitlyDisabled reports whether agentCfg uses the documented
// min_active_sessions=0 + max_active_sessions=0 form to intentionally
// disable pooling (TestValidateAgentsPoolMaxZeroIsValid in
// internal/config), as opposed to an actual pool configuration. That form
// genuinely suppresses pool spawns, so it is not a pool/named-session
// conflict.
func agentPoolExplicitlyDisabled(agentCfg config.Agent) bool {
	return agentCfg.MinActiveSessions != nil && *agentCfg.MinActiveSessions == 0 &&
		agentCfg.MaxActiveSessions != nil && *agentCfg.MaxActiveSessions == 0 &&
		strings.TrimSpace(agentCfg.ScaleCheck) == "" &&
		strings.TrimSpace(agentCfg.Namepool) == "" &&
		len(agentCfg.NamepoolNames) == 0
}

func collectLintPromptTargets(packDir string, loaded *config.LintPackLoad) ([]lintPromptTarget, []lintDiagnostic) {
	var targets []lintPromptTarget
	var diagnostics []lintDiagnostic
	seenRender := map[string]struct{}{}
	seenPath := map[string]struct{}{}
	for _, agentCfg := range loaded.Agents {
		if strings.TrimSpace(agentCfg.PromptTemplate) == "" {
			continue
		}
		sourcePath := promptTemplateSourcePath(packDir, agentCfg.PromptTemplate)
		key := sourcePath + "\x00" + agentCfg.QualifiedName()
		if _, ok := seenRender[key]; ok {
			continue
		}
		seenRender[key] = struct{}{}
		seenPath[sourcePath] = struct{}{}
		targets = append(targets, lintPromptTarget{
			templatePath: agentCfg.PromptTemplate,
			sourcePath:   sourcePath,
			agent:        agentCfg,
		})
	}

	err := filepath.WalkDir(packDir, func(path string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			diagnostics = append(diagnostics, diagnosticFromError(path, walkErr))
			return nil
		}
		if entry == nil {
			return nil
		}
		if entry.IsDir() {
			if path != packDir && lintSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isPromptTemplatePath(entry.Name()) {
			return nil
		}
		if _, ok := seenPath[path]; ok {
			return nil
		}
		seenPath[path] = struct{}{}
		rel, err := filepath.Rel(packDir, path)
		if err != nil {
			diagnostics = append(diagnostics, diagnosticFromError(path, err))
			return nil
		}
		name := lintAgentNameFromTemplate(path)
		targets = append(targets, lintPromptTarget{
			templatePath: rel,
			sourcePath:   path,
			agent: config.Agent{
				Name:           name,
				PromptTemplate: rel,
			},
		})
		return nil
	})
	if err != nil {
		diagnostics = append(diagnostics, diagnosticFromError(packDir, err))
	}

	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].sourcePath == targets[j].sourcePath {
			return targets[i].agent.QualifiedName() < targets[j].agent.QualifiedName()
		}
		return targets[i].sourcePath < targets[j].sourcePath
	})
	return targets, diagnostics
}

func lintAgentNameFromTemplate(path string) string {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, canonicalPromptTemplateSuffix)
	name = strings.TrimSuffix(name, legacyPromptTemplateSuffix)
	if strings.TrimSpace(name) == "" {
		return "lint-agent"
	}
	return name
}

// lintPrompt renders one prompt target and reports what breaks. packDirs are
// the pack's own fragment sources and report at error severity; cityPackDirs
// are composed in by the hosting city (see lintFragmentSearchDirs) and supply
// fragments at warning severity, so a neighboring pack's malformed fragment
// cannot fail this pack's lint. City dirs are parsed first so the pack's own
// definitions win on a name collision.
func lintPrompt(packDir string, packDirs, cityPackDirs []string, providers map[string]config.ProviderSpec, target lintPromptTarget) []lintDiagnostic {
	sourcePath := target.sourcePath
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return []lintDiagnostic{diagnosticFromError(sourcePath, err)}
	}

	var diagnostics []lintDiagnostic
	for _, finding := range bdflags.ScanUnknownFlags(data) {
		diagnostics = append(diagnostics, newLintDiagnostic(sourcePath, finding.Line,
			fmt.Sprintf("bd-unknown-flag: bd %s uses unrecognized flag %q", finding.Subcommand, finding.Flag)))
	}

	_, body := promptmeta.Parse(string(data))
	if !isPromptTemplatePath(target.templatePath) {
		return diagnostics
	}

	var tmpl *template.Template
	tmpl = template.New("prompt").
		Funcs(promptFuncMap("lint-city", "", nil, func() *template.Template { return tmpl })).
		Option("missingkey=zero")

	for _, dir := range cityPackDirs {
		diagnostics = append(diagnostics, lintLoadAdvisorySharedTemplates(tmpl, filepath.Join(dir, "prompts", "shared"))...)
		diagnostics = append(diagnostics, lintLoadAdvisorySharedTemplates(tmpl, filepath.Join(dir, "template-fragments"))...)
	}
	for _, dir := range packDirs {
		diagnostics = append(diagnostics, lintLoadSharedTemplates(tmpl, filepath.Join(dir, "prompts", "shared"))...)
		diagnostics = append(diagnostics, lintLoadSharedTemplates(tmpl, filepath.Join(dir, "template-fragments"))...)
	}
	if sourcePackRoot := promptSourcePackRoot(packDir, sourcePath); sourcePackRoot != "" {
		diagnostics = append(diagnostics, lintLoadSharedTemplates(tmpl, filepath.Join(sourcePackRoot, "prompts", "shared"))...)
		diagnostics = append(diagnostics, lintLoadSharedTemplates(tmpl, filepath.Join(sourcePackRoot, "template-fragments"))...)
	}
	diagnostics = append(diagnostics, lintLoadSharedTemplates(tmpl, filepath.Join(packDir, "prompts", "shared"))...)
	diagnostics = append(diagnostics, lintLoadSharedTemplates(tmpl, filepath.Join(packDir, "template-fragments"))...)
	diagnostics = append(diagnostics, lintLoadSharedTemplates(tmpl, filepath.Join(filepath.Dir(sourcePath), "shared"))...)
	diagnostics = append(diagnostics, lintLoadSharedTemplates(tmpl, filepath.Join(filepath.Dir(sourcePath), "template-fragments"))...)

	if _, err := tmpl.Parse(body); err != nil {
		return append(diagnostics, diagnosticFromError(sourcePath, err))
	}

	dataMap := buildTemplateData(lintPromptContext(packDir, target.agent, providers))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, dataMap); err != nil {
		diagnostics = append(diagnostics, diagnosticFromError(sourcePath, err))
	}
	for _, name := range effectivePromptFragments(nil, target.agent.InjectFragments, target.agent.AppendFragments, target.agent.InheritedAppendFragments, nil) {
		frag := tmpl.Lookup(name)
		if frag == nil {
			diagnostics = append(diagnostics, newLintDiagnostic(sourcePath, 0, fmt.Sprintf("inject_fragment %q: template not found", name)))
			continue
		}
		var fbuf bytes.Buffer
		if err := frag.Execute(&fbuf, dataMap); err != nil {
			diagnostics = append(diagnostics, diagnosticFromError(sourcePath, err))
		}
	}
	return diagnostics
}

// lintLoadSharedTemplates parses every shared template under dir into tmpl and
// reports failures at error severity. Use it for directories the linted pack
// owns.
func lintLoadSharedTemplates(tmpl *template.Template, dir string) []lintDiagnostic {
	return loadLintSharedTemplates(tmpl, dir, false)
}

// lintLoadAdvisorySharedTemplates parses every shared template under dir into
// tmpl but reports failures at warning severity, so the fragments are available
// to the linted pack without a directory it does not own deciding its exit
// code.
func lintLoadAdvisorySharedTemplates(tmpl *template.Template, dir string) []lintDiagnostic {
	return loadLintSharedTemplates(tmpl, dir, true)
}

func loadLintSharedTemplates(tmpl *template.Template, dir string, advisory bool) []lintDiagnostic {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	report := diagnosticFromError
	if advisory {
		report = func(path string, err error) lintDiagnostic {
			message := "unknown error"
			if err != nil {
				message = err.Error()
			}
			return diagnosticFromWarning(path, message)
		}
	}
	var diagnostics []lintDiagnostic
	for _, name := range sharedTemplateFileNames(entries) {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			diagnostics = append(diagnostics, report(path, err))
			continue
		}
		if _, err := tmpl.Parse(string(data)); err != nil {
			diagnostics = append(diagnostics, report(path, err))
		}
	}
	return diagnostics
}

func lintPromptContext(packDir string, agentCfg config.Agent, providers map[string]config.ProviderSpec) PromptContext {
	env := make(map[string]string, len(agentCfg.Env)+4)
	for key, value := range agentCfg.Env {
		env[key] = value
	}
	qualifiedName := agentCfg.QualifiedName()
	if qualifiedName == "" {
		qualifiedName = "lint-agent"
	}
	providerKey := agentCfg.Provider
	// The zero query topology is right and not a shortcut: `gc lint` renders a
	// PACK, which is not deployed to any city, so there is no storage
	// arrangement to read. It lints the command's SHAPE.
	return PromptContext{
		CityRoot:                packDir,
		AgentName:               qualifiedName,
		TemplateName:            lintFirstNonEmpty(agentCfg.Name, "lint-agent"),
		BindingName:             agentCfg.BindingName,
		BindingPrefix:           agentCfg.BindingPrefix(),
		RigName:                 lintFirstNonEmpty(agentCfg.Dir, "lint-rig"),
		RigRoot:                 filepath.Join(packDir, "rigs", lintFirstNonEmpty(agentCfg.Dir, "lint-rig")),
		WorkDir:                 packDir,
		ConfigDir:               packDir,
		IssuePrefix:             "lint",
		Branch:                  "feature/lint",
		DefaultBranch:           "main",
		AssignedInProgressQuery: agentCfg.EffectiveAssignedInProgressQueryFor(config.QueryTopology{}),
		AssignedReadyQuery:      agentCfg.EffectiveAssignedReadyQueryFor(config.QueryTopology{}),
		RoutedPoolQuery:         agentCfg.EffectiveRoutedPoolQueryFor(config.QueryTopology{}),
		WorkQuery:               agentCfg.EffectiveWorkQueryFor(config.QueryTopology{}),
		SlingQuery:              agentCfg.EffectiveSlingQuery(),
		ProviderKey:             providerKey,
		ProviderDisplayName:     providerDisplayNameFor(providerKey, providers),
		Env:                     env,
	}
}

func lintFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// lintFormulaFiles checks all formula TOML files in the packDir/formulas/
// directory for graph.v2 steps that use gc.output_json instead of drain.
func lintFormulaFiles(packDir string) []lintDiagnostic {
	formulaDir := filepath.Join(packDir, "formulas")
	entries, err := os.ReadDir(formulaDir)
	if err != nil {
		return nil // no formulas/ dir is normal
	}
	parser := formula.NewParser(formulaDir)
	var diagnostics []lintDiagnostic
	for _, entry := range entries {
		if entry.IsDir() || !formula.IsTOMLFilename(entry.Name()) {
			continue
		}
		filePath := filepath.Join(formulaDir, entry.Name())
		f, err := parser.ParseFile(filePath)
		if err != nil {
			continue // parse errors are not this check's responsibility
		}
		for _, msg := range formula.GraphV2OutputJSONWarnings(f) {
			diagnostics = append(diagnostics, diagnosticFromWarning(filePath, msg))
		}
	}
	return diagnostics
}

func diagnosticFromError(path string, err error) lintDiagnostic {
	message := "unknown error"
	if err != nil {
		message = strings.TrimSpace(err.Error())
	}
	return newLintDiagnostic(path, lineFromError(message), message)
}

func diagnosticFromWarning(path string, warning string) lintDiagnostic {
	message := strings.TrimSpace(warning)
	if idx := strings.Index(message, ": "); idx > 0 {
		candidate := message[:idx]
		if filepath.IsAbs(candidate) || strings.HasSuffix(candidate, "pack.toml") {
			path = candidate
			message = strings.TrimSpace(message[idx+2:])
		}
	} else if path != "" {
		message = strings.TrimPrefix(message, path+": ")
	}
	return lintDiagnostic{
		Severity: "warning",
		Path:     path,
		Line:     lineFromError(message),
		Message:  strings.TrimSpace(message),
	}
}

func newLintDiagnostic(path string, line int, message string) lintDiagnostic {
	return lintDiagnostic{
		Severity: "error",
		Path:     path,
		Line:     line,
		Message:  strings.TrimSpace(message),
	}
}

func lintErrorCount(diagnostics []lintDiagnostic) int {
	var count int
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			count++
		}
	}
	return count
}

func lineFromError(message string) int {
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bline\s+(\d+)\b`),
		regexp.MustCompile(`:(\d+):`),
	} {
		match := re.FindStringSubmatch(message)
		if len(match) != 2 {
			continue
		}
		line, err := strconv.Atoi(match[1])
		if err == nil {
			return line
		}
	}
	return 0
}

func writeLintHuman(stdout, stderr io.Writer, report lintReport) {
	for _, pack := range report.Packs {
		for _, diagnostic := range pack.Diagnostics {
			fmt.Fprintln(stderr, formatLintDiagnostic(diagnostic)) //nolint:errcheck
		}
	}
	if report.Passed {
		switch len(report.Packs) {
		case 1:
			fmt.Fprintf(stdout, "gc lint: %s: ok\n", report.Packs[0].Path) //nolint:errcheck
		default:
			fmt.Fprintf(stdout, "gc lint: %d pack(s) ok\n", len(report.Packs)) //nolint:errcheck
		}
		return
	}
}

func formatLintDiagnostic(diagnostic lintDiagnostic) string {
	path := diagnostic.Path
	if strings.TrimSpace(path) == "" {
		path = "gc lint"
	}
	message := diagnostic.Message
	if message == "" {
		message = "lint failed"
	}
	if diagnostic.Line > 0 {
		if diagnostic.Severity != "" && diagnostic.Severity != "error" {
			return fmt.Sprintf("%s:%d: %s: %s", path, diagnostic.Line, diagnostic.Severity, message)
		}
		return fmt.Sprintf("%s:%d: %s", path, diagnostic.Line, message)
	}
	if diagnostic.Severity != "" && diagnostic.Severity != "error" {
		return fmt.Sprintf("%s: %s: %s", path, diagnostic.Severity, message)
	}
	return fmt.Sprintf("%s: %s", path, message)
}

// lintClaudeOverlayHookShape walks a pack for .claude/settings.json overlay
// files and flags any top-level hook entry using the invalid bare shape.
// Claude Code requires the wrapped {"matcher": ..., "hooks": [...]} form; a bare
// {"type": "command", "command": ...} entry projects to a settings file that
// fails Claude's schema (and `claude doctor`).
func lintClaudeOverlayHookShape(packDir string) []lintDiagnostic {
	var diagnostics []lintDiagnostic
	_ = filepath.WalkDir(packDir, func(path string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if path != packDir && lintSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "settings.json" || filepath.Base(filepath.Dir(path)) != ".claude" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			diagnostics = append(diagnostics, diagnosticFromError(path, err))
			return nil
		}
		bare, err := overlay.FindBareHookEntries(data)
		if err != nil {
			diagnostics = append(diagnostics, diagnosticFromError(path, err))
			return nil
		}
		for _, b := range bare {
			diagnostics = append(diagnostics, newLintDiagnostic(path, 0, fmt.Sprintf(
				"hooks.%s[%d] is a bare hook entry; Claude settings require the wrapped form {\"matcher\": ..., \"hooks\": [...]}",
				b.Category, b.Index)))
		}
		return nil
	})
	return diagnostics
}
