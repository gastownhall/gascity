// Package migrate converts legacy city agent declarations into pack-first layout.
package migrate

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Options configures the migration run.
type Options struct {
	DryRun bool
}

// Report summarizes changed files and manual follow-up warnings.
type Report struct {
	Changes  []string
	Warnings []string
}

type packFile struct {
	Pack           config.PackMeta                `toml:"pack"`
	Imports        map[string]config.Import       `toml:"imports,omitempty"`
	NamedSessions  []config.NamedSession          `toml:"named_session,omitempty"`
	Services       []config.Service               `toml:"service,omitempty"`
	Providers      map[string]config.ProviderSpec `toml:"providers,omitempty"`
	Formulas       config.FormulasConfig          `toml:"formulas,omitempty"`
	Patches        config.Patches                 `toml:"patches,omitempty"`
	Doctor         []config.PackDoctorEntry       `toml:"doctor,omitempty"`
	Commands       []config.PackCommandEntry      `toml:"commands,omitempty"`
	Global         config.PackGlobal              `toml:"global,omitempty"`
	AgentDefaults  config.AgentDefaults           `toml:"agent_defaults,omitempty"`
	AgentsDefaults config.AgentDefaults           `toml:"agents,omitempty"`
	Defaults       packDefaults                   `toml:"defaults,omitempty"`
	Agents         []config.Agent                 `toml:"agent"`

	defaultRigImportOrder []string
}

type packDefaults struct {
	Rig packRigDefaults `toml:"rig,omitempty"`
}

type packRigDefaults struct {
	Imports map[string]config.Import `toml:"imports,omitempty"`
}

type agentFile struct {
	Description            string            `toml:"description,omitempty"`
	Dir                    string            `toml:"dir,omitempty"`
	WorkDir                string            `toml:"work_dir,omitempty"`
	Scope                  string            `toml:"scope,omitempty"`
	Suspended              bool              `toml:"suspended,omitempty"`
	PreStart               []string          `toml:"pre_start,omitempty"`
	Nudge                  string            `toml:"nudge,omitempty"`
	Session                string            `toml:"session,omitempty"`
	Provider               string            `toml:"provider,omitempty"`
	StartCommand           string            `toml:"start_command,omitempty"`
	Args                   []string          `toml:"args,omitempty"`
	PromptMode             string            `toml:"prompt_mode,omitempty"`
	PromptFlag             string            `toml:"prompt_flag,omitempty"`
	ReadyDelayMs           *int              `toml:"ready_delay_ms,omitempty"`
	ReadyPromptPrefix      string            `toml:"ready_prompt_prefix,omitempty"`
	ProcessNames           []string          `toml:"process_names,omitempty"`
	EmitsPermissionWarning *bool             `toml:"emits_permission_warning,omitempty"`
	Env                    map[string]string `toml:"env,omitempty"`
	OptionDefaults         map[string]string `toml:"option_defaults,omitempty"`
	MaxActiveSessions      *int              `toml:"max_active_sessions,omitempty"`
	MinActiveSessions      *int              `toml:"min_active_sessions,omitempty"`
	ScaleCheck             string            `toml:"scale_check,omitempty"`
	DrainTimeout           string            `toml:"drain_timeout,omitempty"`
	OnBoot                 string            `toml:"on_boot,omitempty"`
	OnDeath                string            `toml:"on_death,omitempty"`
	WorkQuery              string            `toml:"work_query,omitempty"`
	SlingQuery             string            `toml:"sling_query,omitempty"`
	IdleTimeout            string            `toml:"idle_timeout,omitempty"`
	SleepAfterIdle         string            `toml:"sleep_after_idle,omitempty"`
	InstallAgentHooks      []string          `toml:"install_agent_hooks,omitempty"`
	HooksInstalled         *bool             `toml:"hooks_installed,omitempty"`
	InjectAssignedSkills   *bool             `toml:"inject_assigned_skills,omitempty"`
	SessionSetup           []string          `toml:"session_setup,omitempty"`
	SessionSetupScript     string            `toml:"session_setup_script,omitempty"`
	SessionLive            []string          `toml:"session_live,omitempty"`
	DefaultSlingFormula    *string           `toml:"default_sling_formula,omitempty"`
	InjectFragments        []string          `toml:"inject_fragments,omitempty"`
	Attach                 *bool             `toml:"attach,omitempty"`
	DependsOn              []string          `toml:"depends_on,omitempty"`
	ResumeCommand          string            `toml:"resume_command,omitempty"`
	WakeMode               string            `toml:"wake_mode,omitempty"`
}

type usageCounts struct {
	prompts  map[string]int
	overlays map[string]int
	namepool map[string]int
}

type agentOrigin string

const (
	originCity agentOrigin = "city.toml"
	originPack agentOrigin = "pack.toml"
)

type agentEntry struct {
	Agent  config.Agent
	Origin agentOrigin
}

var (
	invalidBindingChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
	repeatedDash        = regexp.MustCompile(`-+`)
)

// Apply migrates a city directory to the pack-first agent layout.
func Apply(cityPath string, opts Options) (*Report, error) {
	report := &Report{}
	cityPath = filepath.Clean(cityPath)

	cityCfg, err := loadCityFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}

	packPath := filepath.Join(cityPath, "pack.toml")
	packCfg, _, err := loadPackFile(packPath)
	if err != nil {
		return nil, err
	}

	selectedAgents, fallbackNames := selectAgents(packCfg.Agents, cityCfg.Agents)
	if len(fallbackNames) > 0 {
		sort.Strings(fallbackNames)
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("dropped fallback field for agents: %s; review shadowing behavior manually",
				strings.Join(fallbackNames, ", ")))
	}

	usage := buildUsageCounts(cityPath, selectedAgents)
	if err := validateAgentAssets(cityPath, selectedAgents); err != nil {
		return nil, err
	}
	for _, entry := range selectedAgents {
		if err := migrateAgentAssets(cityPath, entry, usage, report, opts); err != nil {
			return nil, fmt.Errorf("migrate agent %q: %w", entry.Agent.Name, err)
		}
	}

	packChanged := false
	if len(packCfg.Agents) > 0 {
		packCfg.Agents = nil
		packChanged = true
	}
	if len(cityCfg.Agents) > 0 {
		cityCfg.Agents = nil
	}

	if len(selectedAgents) > 0 || len(cityCfg.Workspace.Includes) > 0 || len(cityCfg.Workspace.DefaultRigIncludes) > 0 {
		if ensurePackMeta(&packCfg, cityCfg, cityPath) {
			packChanged = true
		}
	}

	if len(cityCfg.Workspace.Includes) > 0 {
		if packCfg.Imports == nil {
			packCfg.Imports = make(map[string]config.Import)
		}
		if addImports(packCfg.Imports, cityCfg.Workspace.Includes, cityCfg.Packs) {
			packChanged = true
		}
		cityCfg.Workspace.Includes = nil
	}

	if len(cityCfg.Workspace.DefaultRigIncludes) > 0 {
		if packCfg.Defaults.Rig.Imports == nil {
			packCfg.Defaults.Rig.Imports = make(map[string]config.Import)
		}
		var changed bool
		packCfg.defaultRigImportOrder, changed = addOrderedImports(
			packCfg.Defaults.Rig.Imports,
			packCfg.defaultRigImportOrder,
			cityCfg.Workspace.DefaultRigIncludes,
			cityCfg.Packs,
		)
		if changed {
			packChanged = true
		}
		cityCfg.Workspace.DefaultRigIncludes = nil
	}

	cityContent, err := cityCfg.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal city.toml: %w", err)
	}
	if err := maybeWriteFile(filepath.Join(cityPath, "city.toml"), cityContent, "rewrite city.toml", report, opts.DryRun); err != nil {
		return nil, err
	}

	if packChanged {
		packContent, err := marshalPackFile(packCfg)
		if err != nil {
			return nil, fmt.Errorf("marshal pack.toml: %w", err)
		}
		if err := maybeWriteFile(packPath, packContent, "rewrite pack.toml", report, opts.DryRun); err != nil {
			return nil, err
		}
	}

	return report, nil
}

func loadCityFile(path string) (*config.City, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("migrate %q: %w", path, err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("migrate %q: %w", path, err)
	}
	if err := config.ResolveWorkspaceIdentity(fsys.OSFS{}, filepath.Dir(path), cfg); err != nil {
		return nil, fmt.Errorf("migrate %q: resolve workspace identity: %w", path, err)
	}
	return cfg, nil
}

func loadPackFile(path string) (packFile, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return packFile{}, false, nil
		}
		return packFile{}, false, fmt.Errorf("migrate %q: %w", path, err)
	}
	var cfg packFile
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return packFile{}, true, fmt.Errorf("migrate %q: %w", path, err)
	}
	if warnings := config.CheckUndecodedKeys(md, path); len(warnings) > 0 {
		return packFile{}, true, fmt.Errorf("migrate %q: %s", path, strings.Join(warnings, "; "))
	}
	cfg.defaultRigImportOrder = packDefaultRigImportOrder(md)
	return cfg, true, nil
}

func packDefaultRigImportOrder(md toml.MetaData) []string {
	var order []string
	seen := make(map[string]bool)
	for _, key := range md.Keys() {
		if len(key) != 4 ||
			key[0] != "defaults" ||
			key[1] != "rig" ||
			key[2] != "imports" {
			continue
		}
		if seen[key[3]] {
			continue
		}
		seen[key[3]] = true
		order = append(order, key[3])
	}
	return order
}

func selectAgents(packAgents, cityAgents []config.Agent) ([]agentEntry, []string) {
	selected := make(map[string]agentEntry)
	seenNames := make(map[string]bool)
	var names []string
	var fallbackNames []string

	add := func(origin agentOrigin, agents []config.Agent, override bool) {
		for _, agent := range agents {
			if agent.Fallback {
				fallbackNames = append(fallbackNames, agent.Name)
			}
			if !seenNames[agent.Name] {
				seenNames[agent.Name] = true
				names = append(names, agent.Name)
			}
			if override {
				selected[agent.Name] = agentEntry{Agent: agent, Origin: origin}
				continue
			}
			if _, exists := selected[agent.Name]; !exists {
				selected[agent.Name] = agentEntry{Agent: agent, Origin: origin}
			}
		}
	}

	add(originPack, packAgents, false)
	add(originCity, cityAgents, true)

	sort.Strings(names)
	result := make([]agentEntry, 0, len(names))
	for _, name := range names {
		result = append(result, selected[name])
	}
	return result, dedupeStrings(fallbackNames)
}

func buildUsageCounts(cityPath string, agents []agentEntry) usageCounts {
	out := usageCounts{
		prompts:  make(map[string]int),
		overlays: make(map[string]int),
		namepool: make(map[string]int),
	}
	for _, entry := range agents {
		agent := entry.Agent
		if agent.PromptTemplate != "" {
			out.prompts[resolvePath(cityPath, agent.PromptTemplate)]++
		}
		if agent.OverlayDir != "" {
			out.overlays[resolvePath(cityPath, agent.OverlayDir)]++
		}
		if agent.Namepool != "" {
			out.namepool[resolvePath(cityPath, agent.Namepool)]++
		}
	}
	return out
}

func validateAgentAssets(cityPath string, agents []agentEntry) error {
	for _, entry := range agents {
		agent := entry.Agent
		if agent.PromptTemplate != "" {
			src := resolvePath(cityPath, agent.PromptTemplate)
			if _, err := os.ReadFile(src); err != nil {
				return fmt.Errorf("migrate agent %q: prompt_template %q: %w", agent.Name, agent.PromptTemplate, err)
			}
		}
		if agent.OverlayDir != "" {
			src := resolvePath(cityPath, agent.OverlayDir)
			info, err := os.Stat(src)
			if err != nil {
				return fmt.Errorf("migrate agent %q: overlay_dir %q: %w", agent.Name, agent.OverlayDir, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("migrate agent %q: overlay_dir %q: %q is not a directory", agent.Name, agent.OverlayDir, src)
			}
		}
		if agent.Namepool != "" {
			src := resolvePath(cityPath, agent.Namepool)
			if _, err := os.ReadFile(src); err != nil {
				return fmt.Errorf("migrate agent %q: namepool %q: %w", agent.Name, agent.Namepool, err)
			}
		}
	}
	return nil
}

func migrateAgentAssets(cityPath string, entry agentEntry, usage usageCounts, report *Report, opts Options) error {
	agent := entry.Agent
	agentDir := filepath.Join(cityPath, "agents", agent.Name)
	if err := ensureDir(agentDir, report, opts.DryRun); err != nil {
		return err
	}

	if agent.PromptTemplate != "" {
		src := resolvePath(cityPath, agent.PromptTemplate)
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("prompt_template %q: %w", agent.PromptTemplate, err)
		}
		destName := "prompt.md"
		if bytes.Contains(data, []byte("{{")) {
			destName = "prompt.template.md"
		}
		dest := filepath.Join(agentDir, destName)
		removeSrc := usage.prompts[src] <= 1
		if err := stageFileMove(src, dest, data, removeSrc, cityPath, report, opts.DryRun); err != nil {
			return err
		}
	}

	if agent.OverlayDir != "" {
		src := resolvePath(cityPath, agent.OverlayDir)
		dest := filepath.Join(agentDir, "overlay")
		removeSrc := usage.overlays[src] <= 1
		if err := stageDirMove(src, dest, removeSrc, cityPath, report, opts.DryRun); err != nil {
			return fmt.Errorf("overlay_dir %q: %w", agent.OverlayDir, err)
		}
	}

	if agent.Namepool != "" {
		src := resolvePath(cityPath, agent.Namepool)
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("namepool %q: %w", agent.Namepool, err)
		}
		dest := filepath.Join(agentDir, "namepool.txt")
		removeSrc := usage.namepool[src] <= 1
		if err := stageFileMove(src, dest, data, removeSrc, cityPath, report, opts.DryRun); err != nil {
			return err
		}
	}

	cfg := agentConfigFromAgent(agent)
	if !isZeroAgentConfig(cfg) {
		data, err := marshalAgentFile(cfg)
		if err != nil {
			return fmt.Errorf("agent.toml: %w", err)
		}
		if err := maybeWriteFile(filepath.Join(agentDir, "agent.toml"), data,
			fmt.Sprintf("write agents/%s/agent.toml", agent.Name), report, opts.DryRun); err != nil {
			return err
		}
	}

	return nil
}

func ensurePackMeta(packCfg *packFile, cityCfg *config.City, cityPath string) bool {
	changed := false
	if packCfg.Pack.Name == "" {
		packCfg.Pack.Name = strings.TrimSpace(config.EffectiveCityName(cityCfg, filepath.Base(cityPath)))
		changed = true
	}
	if packCfg.Pack.Schema == 0 {
		packCfg.Pack.Schema = 1
		changed = true
	}
	return changed
}

func addImports(target map[string]config.Import, includes []string, packs map[string]config.PackSource) bool {
	changed := false
	for _, include := range includes {
		source := importSourceFor(include, packs)
		if _, exists := existingDefaultImportBindingForSource(target, source); exists {
			continue
		}
		binding := uniqueBinding(target, deriveBindingName(include, source, packs))
		target[binding] = config.Import{Source: source}
		changed = true
	}
	return changed
}

func addOrderedImports(target map[string]config.Import, order []string, includes []string, packs map[string]config.PackSource) ([]string, bool) {
	changed := false
	for _, include := range includes {
		source := importSourceFor(include, packs)
		binding, exists := existingDefaultImportBindingForSource(target, source)
		if !exists {
			binding = uniqueBinding(target, deriveBindingName(include, source, packs))
			target[binding] = config.Import{Source: source}
			changed = true
		}
		if !stringSliceContains(order, binding) {
			order = append(order, binding)
			changed = true
		}
	}
	return order, changed
}

func existingDefaultImportBindingForSource(target map[string]config.Import, source string) (string, bool) {
	for binding, imp := range target {
		if imp.Source == source && importMatchesLegacyDefault(imp) {
			return binding, true
		}
	}
	return "", false
}

func importMatchesLegacyDefault(imp config.Import) bool {
	if strings.TrimSpace(imp.Version) != "" || imp.Export {
		return false
	}
	if imp.Transitive != nil && !*imp.Transitive {
		return false
	}
	shadow := strings.TrimSpace(imp.Shadow)
	return shadow == "" || shadow == "warn"
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func importSourceFor(include string, packs map[string]config.PackSource) string {
	if spec, ok := packs[include]; ok {
		source := spec.Source
		if spec.Path != "" {
			source += "//" + strings.TrimPrefix(spec.Path, "/")
		}
		if spec.Ref != "" {
			source += "#" + spec.Ref
		}
		return source
	}
	if looksLikeLocalPath(include) && !strings.HasPrefix(include, "./") && !strings.HasPrefix(include, "../") && !filepath.IsAbs(include) {
		return "./" + include
	}
	return include
}

func deriveBindingName(include, source string, packs map[string]config.PackSource) string {
	if _, ok := packs[include]; ok {
		return sanitizeBindingName(include)
	}
	base := source
	if idx := strings.Index(base, "#"); idx >= 0 {
		base = base[:idx]
	}
	if idx := strings.LastIndex(base, "//"); idx >= 0 && idx > strings.Index(base, "://")+2 {
		base = base[idx+2:]
	}
	base = strings.TrimSuffix(base, "/")
	base = strings.TrimSuffix(base, ".git")
	base = pathBase(base)
	return sanitizeBindingName(base)
}

func uniqueBinding(target map[string]config.Import, base string) string {
	if base == "" {
		base = "import"
	}
	if _, exists := target[base]; !exists {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, exists := target[candidate]; !exists {
			return candidate
		}
	}
}

func sanitizeBindingName(value string) string {
	value = invalidBindingChars.ReplaceAllString(value, "-")
	value = repeatedDash.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "import"
	}
	return value
}

func resolvePath(root, ref string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return filepath.Clean(filepath.Join(root, ref))
}

func looksLikeLocalPath(value string) bool {
	if strings.Contains(value, "://") || strings.HasPrefix(value, "git@") {
		return false
	}
	if strings.HasPrefix(value, "github.com/") {
		return false
	}
	return true
}

func pathBase(value string) string {
	value = strings.TrimRight(value, "/")
	if idx := strings.LastIndex(value, "/"); idx >= 0 {
		return value[idx+1:]
	}
	return value
}

func ensureDir(path string, report *Report, dryRun bool) error {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil
	}
	report.Changes = append(report.Changes, fmt.Sprintf("create %s", relativeOrSame(path)))
	if dryRun {
		return nil
	}
	return os.MkdirAll(path, 0o755)
}

func stageFileMove(src, dest string, data []byte, removeSrc bool, stopDir string, report *Report, dryRun bool) error {
	if err := maybeWriteFile(dest, data, fmt.Sprintf("write %s", relativeOrSame(dest)), report, dryRun); err != nil {
		return err
	}
	if removeSrc && filepath.Clean(src) != filepath.Clean(dest) {
		if err := maybeRemoveFile(src, stopDir, report, dryRun); err != nil {
			return err
		}
	}
	return nil
}

func stageDirMove(src, dest string, removeSrc bool, stopDir string, report *Report, dryRun bool) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", src)
	}
	if err := copyDir(src, dest, report, dryRun); err != nil {
		return err
	}
	if removeSrc && filepath.Clean(src) != filepath.Clean(dest) {
		if err := maybeRemoveDir(src, stopDir, report, dryRun); err != nil {
			return err
		}
	}
	return nil
}

func copyDir(src, dest string, report *Report, dryRun bool) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			if rel == "." {
				return ensureDir(dest, report, dryRun)
			}
			return ensureDir(target, report, dryRun)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return maybeWriteFile(target, data, fmt.Sprintf("write %s", relativeOrSame(target)), report, dryRun)
	})
}

func maybeWriteFile(path string, data []byte, change string, report *Report, dryRun bool) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	report.Changes = append(report.Changes, change)
	if dryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func maybeRemoveFile(path, stopDir string, report *Report, dryRun bool) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	report.Changes = append(report.Changes, fmt.Sprintf("remove %s", relativeOrSame(path)))
	if dryRun {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	pruneEmptyParents(filepath.Dir(path), stopDir)
	return nil
}

func maybeRemoveDir(path, stopDir string, report *Report, dryRun bool) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	report.Changes = append(report.Changes, fmt.Sprintf("remove %s", relativeOrSame(path)))
	if dryRun {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	pruneEmptyParents(filepath.Dir(path), stopDir)
	return nil
}

func pruneEmptyParents(dir, stopDir string) {
	stopDir = filepath.Clean(stopDir)
	for dir != "." && dir != string(filepath.Separator) && filepath.Clean(dir) != stopDir {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func marshalPackFile(cfg packFile) ([]byte, error) {
	defaultRigImports := cfg.Defaults.Rig.Imports
	cfg.Defaults.Rig.Imports = nil

	data, err := encodeTOML(cfg)
	if err != nil {
		return nil, err
	}
	if len(defaultRigImports) == 0 {
		return data, nil
	}

	var buf bytes.Buffer
	buf.Write(data)
	if buf.Len() > 0 && !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		buf.WriteByte('\n')
	}
	if buf.Len() > 0 && !bytes.HasSuffix(buf.Bytes(), []byte("\n\n")) {
		buf.WriteByte('\n')
	}

	names := orderedPackDefaultRigImportNames(defaultRigImports, cfg.defaultRigImportOrder)
	for i, name := range names {
		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "[defaults.rig.imports.%s]\n", tomlKey(name))
		importData, err := encodeTOML(defaultRigImports[name])
		if err != nil {
			return nil, err
		}
		buf.Write(importData)
		if !bytes.HasSuffix(importData, []byte("\n")) {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), nil
}

func orderedPackDefaultRigImportNames(imports map[string]config.Import, order []string) []string {
	names := make([]string, 0, len(imports))
	seen := make(map[string]bool, len(imports))
	for _, name := range order {
		if _, ok := imports[name]; !ok || seen[name] {
			continue
		}
		names = append(names, name)
		seen[name] = true
	}

	var remaining []string
	for name := range imports {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(names, remaining...)
}

func tomlKey(key string) string {
	if key != "" {
		bare := true
		for _, r := range key {
			if (r >= 'A' && r <= 'Z') ||
				(r >= 'a' && r <= 'z') ||
				(r >= '0' && r <= '9') ||
				r == '_' ||
				r == '-' {
				continue
			}
			bare = false
			break
		}
		if bare {
			return key
		}
	}
	return strconv.Quote(key)
}

func marshalAgentFile(cfg agentFile) ([]byte, error) {
	return encodeTOML(cfg)
}

func encodeTOML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func agentConfigFromAgent(agent config.Agent) agentFile {
	return agentFile{
		Description:            agent.Description,
		Dir:                    agent.Dir,
		WorkDir:                agent.WorkDir,
		Scope:                  agent.Scope,
		Suspended:              agent.Suspended,
		PreStart:               agent.PreStart,
		Nudge:                  agent.Nudge,
		Session:                agent.Session,
		Provider:               agent.Provider,
		StartCommand:           agent.StartCommand,
		Args:                   agent.Args,
		PromptMode:             agent.PromptMode,
		PromptFlag:             agent.PromptFlag,
		ReadyDelayMs:           agent.ReadyDelayMs,
		ReadyPromptPrefix:      agent.ReadyPromptPrefix,
		ProcessNames:           agent.ProcessNames,
		EmitsPermissionWarning: agent.EmitsPermissionWarning,
		Env:                    agent.Env,
		OptionDefaults:         agent.OptionDefaults,
		MaxActiveSessions:      agent.MaxActiveSessions,
		MinActiveSessions:      agent.MinActiveSessions,
		ScaleCheck:             agent.ScaleCheck,
		DrainTimeout:           agent.DrainTimeout,
		OnBoot:                 agent.OnBoot,
		OnDeath:                agent.OnDeath,
		WorkQuery:              agent.WorkQuery,
		SlingQuery:             agent.SlingQuery,
		IdleTimeout:            agent.IdleTimeout,
		SleepAfterIdle:         agent.SleepAfterIdle,
		InstallAgentHooks:      agent.InstallAgentHooks,
		HooksInstalled:         agent.HooksInstalled,
		InjectAssignedSkills:   agent.InjectAssignedSkills,
		SessionSetup:           agent.SessionSetup,
		SessionSetupScript:     agent.SessionSetupScript,
		SessionLive:            agent.SessionLive,
		DefaultSlingFormula:    agent.DefaultSlingFormula,
		InjectFragments:        agent.InjectFragments,
		Attach:                 agent.Attach,
		DependsOn:              agent.DependsOn,
		ResumeCommand:          agent.ResumeCommand,
		WakeMode:               agent.WakeMode,
	}
}

func isZeroAgentConfig(cfg agentFile) bool {
	return cfg.Description == "" &&
		cfg.Dir == "" &&
		cfg.WorkDir == "" &&
		cfg.Scope == "" &&
		!cfg.Suspended &&
		len(cfg.PreStart) == 0 &&
		cfg.Nudge == "" &&
		cfg.Session == "" &&
		cfg.Provider == "" &&
		cfg.StartCommand == "" &&
		len(cfg.Args) == 0 &&
		cfg.PromptMode == "" &&
		cfg.PromptFlag == "" &&
		cfg.ReadyDelayMs == nil &&
		cfg.ReadyPromptPrefix == "" &&
		len(cfg.ProcessNames) == 0 &&
		cfg.EmitsPermissionWarning == nil &&
		len(cfg.Env) == 0 &&
		len(cfg.OptionDefaults) == 0 &&
		cfg.MaxActiveSessions == nil &&
		cfg.MinActiveSessions == nil &&
		cfg.ScaleCheck == "" &&
		cfg.DrainTimeout == "" &&
		cfg.OnBoot == "" &&
		cfg.OnDeath == "" &&
		cfg.WorkQuery == "" &&
		cfg.SlingQuery == "" &&
		cfg.IdleTimeout == "" &&
		cfg.SleepAfterIdle == "" &&
		len(cfg.InstallAgentHooks) == 0 &&
		cfg.HooksInstalled == nil &&
		cfg.InjectAssignedSkills == nil &&
		len(cfg.SessionSetup) == 0 &&
		cfg.SessionSetupScript == "" &&
		len(cfg.SessionLive) == 0 &&
		cfg.DefaultSlingFormula == nil &&
		len(cfg.InjectFragments) == 0 &&
		cfg.Attach == nil &&
		len(cfg.DependsOn) == 0 &&
		cfg.ResumeCommand == "" &&
		cfg.WakeMode == ""
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func relativeOrSame(path string) string {
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}
