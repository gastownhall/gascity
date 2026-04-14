package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func registerV2DeprecationChecks(d *doctor.Doctor) {
	d.Register(v2AgentFormatCheck{})
	d.Register(v2ImportFormatCheck{})
	d.Register(v2DefaultRigImportFormatCheck{})
	d.Register(v2ScriptsLayoutCheck{})
	d.Register(v2WorkspaceNameCheck{})
	d.Register(v2PromptTemplateSuffixCheck{})
}

type v2AgentFormatCheck struct{}

func (v2AgentFormatCheck) Name() string                     { return "v2-agent-format" }
func (v2AgentFormatCheck) CanFix() bool                     { return false }
func (v2AgentFormatCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2AgentFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	files := legacyAgentFiles(ctx.CityPath)
	if len(files) == 0 {
		return okCheck("v2-agent-format", "no legacy [[agent]] tables found")
	}
	return warnCheck("v2-agent-format",
		fmt.Sprintf("legacy [[agent]] tables found in %s", strings.Join(files, ", ")),
		v2MigrationHint(),
		files)
}

type v2ImportFormatCheck struct{}

func (v2ImportFormatCheck) Name() string                     { return "v2-import-format" }
func (v2ImportFormatCheck) CanFix() bool                     { return false }
func (v2ImportFormatCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2ImportFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cfg, ok := parseCityConfig(filepath.Join(ctx.CityPath, "city.toml"))
	if !ok || len(cfg.Workspace.Includes) == 0 {
		return okCheck("v2-import-format", "workspace.includes already migrated")
	}
	return warnCheck("v2-import-format",
		"workspace.includes is deprecated; this city must migrate to [imports] before Pack/City v2 can load it",
		v2MigrationHint(),
		cfg.Workspace.Includes)
}

type v2DefaultRigImportFormatCheck struct{}

func (v2DefaultRigImportFormatCheck) Name() string                     { return "v2-default-rig-import-format" }
func (v2DefaultRigImportFormatCheck) CanFix() bool                     { return false }
func (v2DefaultRigImportFormatCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2DefaultRigImportFormatCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cfg, ok := parseCityConfig(filepath.Join(ctx.CityPath, "city.toml"))
	if !ok || len(cfg.Workspace.DefaultRigIncludes) == 0 {
		return okCheck("v2-default-rig-import-format", "workspace.default_rig_includes already migrated")
	}
	return warnCheck("v2-default-rig-import-format",
		"workspace.default_rig_includes is deprecated; migrate to [rig_defaults] imports = [...]",
		v2MigrationHint(),
		cfg.Workspace.DefaultRigIncludes)
}

type v2ScriptsLayoutCheck struct{}

func (v2ScriptsLayoutCheck) Name() string                     { return "v2-scripts-layout" }
func (v2ScriptsLayoutCheck) CanFix() bool                     { return false }
func (v2ScriptsLayoutCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2ScriptsLayoutCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	path := filepath.Join(ctx.CityPath, "scripts")
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return okCheck("v2-scripts-layout", "no top-level scripts/ directory found")
	}
	return warnCheck("v2-scripts-layout",
		"top-level scripts/ is deprecated; move scripts to commands/ or assets/",
		"move entrypoint scripts next to commands/doctor entries or under assets/",
		[]string{"scripts/"})
}

type v2WorkspaceNameCheck struct{}

func (v2WorkspaceNameCheck) Name() string                     { return "v2-workspace-name" }
func (v2WorkspaceNameCheck) CanFix() bool                     { return false }
func (v2WorkspaceNameCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2WorkspaceNameCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	cfg, ok := parseCityConfig(filepath.Join(ctx.CityPath, "city.toml"))
	if !ok || strings.TrimSpace(cfg.Workspace.Name) == "" {
		return okCheck("v2-workspace-name", "workspace.name already absent")
	}
	return warnCheck("v2-workspace-name",
		"workspace.name will move to .gc/ in a future release",
		"review site-binding migration guidance before the hard cutover",
		[]string{cfg.Workspace.Name})
}

type v2PromptTemplateSuffixCheck struct{}

func (v2PromptTemplateSuffixCheck) Name() string                     { return "v2-prompt-template-suffix" }
func (v2PromptTemplateSuffixCheck) CanFix() bool                     { return false }
func (v2PromptTemplateSuffixCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (v2PromptTemplateSuffixCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	files := templatedMarkdownPrompts(ctx.CityPath)
	if len(files) == 0 {
		return okCheck("v2-prompt-template-suffix", "templated markdown prompts already use .template.md suffixes")
	}
	return warnCheck("v2-prompt-template-suffix",
		"templated markdown prompts should use .template.md",
		"rename each templated prompt file to *.template.md",
		files)
}

func okCheck(name, message string) *doctor.CheckResult {
	return &doctor.CheckResult{Name: name, Status: doctor.StatusOK, Message: message}
}

func warnCheck(name, message, hint string, details []string) *doctor.CheckResult {
	return &doctor.CheckResult{
		Name:    name,
		Status:  doctor.StatusWarning,
		Message: message,
		FixHint: hint,
		Details: details,
	}
}

func v2MigrationHint() string {
	return `run "gc doctor --fix" to rewrite safe mechanical cases, then rerun "gc doctor"`
}

func parseCityConfig(path string) (*config.City, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, false
	}
	return cfg, true
}

func legacyAgentFiles(cityPath string) []string {
	var files []string
	if cfg, ok := parseCityConfig(filepath.Join(cityPath, "city.toml")); ok && len(cfg.Agents) > 0 {
		files = append(files, "city.toml")
	}
	type rawPack struct {
		Agents []config.Agent `toml:"agent"`
	}
	packPath := filepath.Join(cityPath, "pack.toml")
	if data, err := os.ReadFile(packPath); err == nil {
		var pack rawPack
		if _, err := toml.Decode(string(data), &pack); err == nil && len(pack.Agents) > 0 {
			files = append(files, "pack.toml")
		}
	}
	return files
}

func templatedMarkdownPrompts(cityPath string) []string {
	candidates := make(map[string]bool)

	addPath := func(path string) {
		switch {
		case isCanonicalPromptTemplatePath(path):
			return
		case isLegacyPromptTemplatePath(path):
			candidates[path] = true
		case strings.HasSuffix(path, ".md"):
			candidates[path] = true
		}
	}

	if cfg, ok := parseCityConfig(filepath.Join(cityPath, "city.toml")); ok {
		for _, agent := range cfg.Agents {
			if agent.PromptTemplate != "" {
				addPath(resolvePromptPath(cityPath, agent.PromptTemplate))
			}
		}
	}

	type rawPack struct {
		Agents []config.Agent `toml:"agent"`
	}
	packPath := filepath.Join(cityPath, "pack.toml")
	if data, err := os.ReadFile(packPath); err == nil {
		var pack rawPack
		if _, err := toml.Decode(string(data), &pack); err == nil {
			for _, agent := range pack.Agents {
				if agent.PromptTemplate != "" {
					addPath(resolvePromptPath(cityPath, agent.PromptTemplate))
				}
			}
		}
	}

	for _, dir := range []string{filepath.Join(cityPath, "prompts"), filepath.Join(cityPath, "agents")} {
		if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if filepath.Base(path) == "prompt.md" ||
				filepath.Base(path) == "prompt.template.md" ||
				filepath.Base(path) == "prompt.md.tmpl" ||
				strings.HasPrefix(path, filepath.Join(cityPath, "prompts")+string(filepath.Separator)) {
				addPath(path)
			}
			return nil
		}); err != nil && !os.IsNotExist(err) {
			continue
		}
	}

	var files []string
	for path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "{{") {
			if rel, err := filepath.Rel(cityPath, path); err == nil {
				files = append(files, rel)
			} else {
				files = append(files, path)
			}
		}
	}
	sort.Strings(files)
	return files
}

func resolvePromptPath(cityPath, ref string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return filepath.Clean(filepath.Join(cityPath, ref))
}
