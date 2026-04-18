package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
)

const (
	canonicalPromptTemplateSuffix = ".template.md"
	legacyPromptTemplateSuffix    = ".md.tmpl"
)

// PromptContext holds template data for prompt rendering.
type PromptContext struct {
	CityRoot      string
	AgentName     string // qualified: "rig/polecat-1" or "mayor"
	TemplateName  string // config name: "polecat" (template) or "mayor" (named backing template)
	RigName       string
	RigRoot       string
	WorkDir       string
	IssuePrefix   string
	Branch        string
	DefaultBranch string            // e.g. "main" — from git symbolic-ref origin/HEAD
	WorkQuery     string            // command to find available work (from Agent.EffectiveWorkQuery)
	SlingQuery    string            // command template to route work to this agent (from Agent.EffectiveSlingQuery)
	Env           map[string]string // from Agent.Env — custom vars
}

// renderPrompt reads a prompt template file and renders it with the given
// context. cityName is used internally by template functions (e.g. session)
// but not exposed as a template variable. sessionTemplate is the custom
// session naming template (empty = default). packDirs are the ordered
// pack directories; each may contain prompts/shared/ subdirectories
// loaded as cross-pack shared templates (lower priority than the
// sibling shared/ dir). injectFragments are named templates to append to
// the output after rendering. Returns empty string if templatePath is empty
// or the file doesn't exist. On parse or execute error, logs a warning to
// stderr and returns the raw text (graceful fallback).
func renderPrompt(fs fsys.FS, cityPath, cityName, templatePath string, ctx PromptContext, sessionTemplate string, stderr io.Writer, packDirs []string, injectFragments []string, store beads.Store) string {
	if templatePath == "" {
		return ""
	}
	sourcePath := templatePath
	if !filepath.IsAbs(sourcePath) {
		sourcePath = filepath.Join(cityPath, templatePath)
	}
	data, err := fs.ReadFile(sourcePath)
	if err != nil {
		return ""
	}
	raw := string(data)

	// Canonical prompt templates use .template.md. Legacy .md.tmpl files
	// remain supported temporarily for compatibility; plain .md files are
	// returned as-is.
	if !isPromptTemplatePath(templatePath) {
		return raw
	}

	tmpl := template.New("prompt").
		Funcs(promptFuncMap(cityName, sessionTemplate, store)).
		Option("missingkey=zero")

	// Load shared templates from pack dirs (lower priority).
	// Each pack directory may contain prompts/shared/ and/or
	// template-fragments/ subdirectories.
	for _, dir := range packDirs {
		sharedDir := filepath.Join(dir, "prompts", "shared")
		loadSharedTemplates(fs, tmpl, sharedDir, stderr)
		// V2: template-fragments/ at pack level.
		fragDir := filepath.Join(dir, "template-fragments")
		loadSharedTemplates(fs, tmpl, fragDir, stderr)
	}

	// Load shared templates from sibling shared/ directory (highest priority —
	// wins on name collision with cross-pack templates).
	sharedDir := filepath.Join(filepath.Dir(sourcePath), "shared")
	loadSharedTemplates(fs, tmpl, sharedDir, stderr)

	// V2: per-agent template-fragments/ (if the prompt lives in agents/<name>/).
	// Load from agents/<name>/template-fragments/ so per-agent fragments
	// are available alongside pack-level ones.
	agentFragDir := filepath.Join(filepath.Dir(sourcePath), "template-fragments")
	loadSharedTemplates(fs, tmpl, agentFragDir, stderr)

	// Parse main template last — its body becomes the "prompt" template.
	tmpl, err = tmpl.Parse(raw)
	if err != nil {
		fmt.Fprintf(stderr, "gc: prompt template %q: %v\n", templatePath, err) //nolint:errcheck // best-effort stderr
		return raw
	}

	td := buildTemplateData(ctx)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, td); err != nil {
		fmt.Fprintf(stderr, "gc: prompt template %q: %v\n", templatePath, err) //nolint:errcheck // best-effort stderr
		return raw
	}

	// Append injected fragments.
	for _, name := range injectFragments {
		frag := tmpl.Lookup(name)
		if frag == nil {
			fmt.Fprintf(stderr, "gc: inject_fragment %q: template not found\n", name) //nolint:errcheck // best-effort stderr
			continue
		}
		var fbuf bytes.Buffer
		if err := frag.Execute(&fbuf, td); err != nil {
			fmt.Fprintf(stderr, "gc: inject_fragment %q: %v\n", name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		buf.WriteString("\n\n")
		buf.Write(fbuf.Bytes())
	}

	return buf.String()
}

func isCanonicalPromptTemplatePath(path string) bool {
	return strings.HasSuffix(path, canonicalPromptTemplateSuffix)
}

func isLegacyPromptTemplatePath(path string) bool {
	return strings.HasSuffix(path, legacyPromptTemplateSuffix)
}

func isPromptTemplatePath(path string) bool {
	return isCanonicalPromptTemplatePath(path) || isLegacyPromptTemplatePath(path)
}

func sharedTemplateFileNames(entries []os.DirEntry) []string {
	legacy := make([]string, 0, len(entries))
	canonical := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch name := e.Name(); {
		case isLegacyPromptTemplatePath(name):
			legacy = append(legacy, name)
		case isCanonicalPromptTemplatePath(name):
			canonical = append(canonical, name)
		}
	}
	sort.Strings(legacy)
	sort.Strings(canonical)
	return append(legacy, canonical...)
}

// loadSharedTemplates loads supported prompt-template files from a shared
// directory into the given template. Canonical .template.md files override
// legacy .md.tmpl files with the same definitions.
func loadSharedTemplates(fs fsys.FS, tmpl *template.Template, dir string, stderr io.Writer) {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, name := range sharedTemplateFileNames(entries) {
		if sdata, err := fs.ReadFile(filepath.Join(dir, name)); err == nil {
			if _, err := tmpl.Parse(string(sdata)); err != nil {
				fmt.Fprintf(stderr, "gc: shared template %q: %v\n", name, err) //nolint:errcheck // best-effort stderr
			}
		}
	}
}

// mergeFragmentLists combines global and per-agent fragment lists.
func mergeFragmentLists(global, perAgent []string) []string {
	if len(global) == 0 && len(perAgent) == 0 {
		return nil
	}
	merged := make([]string, 0, len(global)+len(perAgent))
	merged = append(merged, global...)
	merged = append(merged, perAgent...)
	return merged
}

// buildTemplateData merges Env (lower priority) with SDK fields (higher
// priority) into a single map for template execution.
func buildTemplateData(ctx PromptContext) map[string]string {
	m := make(map[string]string, len(ctx.Env)+8)
	for k, v := range ctx.Env {
		m[k] = v
	}
	// SDK fields override Env.
	m["CityRoot"] = ctx.CityRoot
	m["AgentName"] = ctx.AgentName
	m["TemplateName"] = ctx.TemplateName
	m["RigName"] = ctx.RigName
	m["RigRoot"] = ctx.RigRoot
	m["WorkDir"] = ctx.WorkDir
	m["IssuePrefix"] = ctx.IssuePrefix
	m["Branch"] = ctx.Branch
	m["DefaultBranch"] = ctx.DefaultBranch
	m["WorkQuery"] = ctx.WorkQuery
	m["SlingQuery"] = ctx.SlingQuery
	return m
}

// findRigPrefix returns the effective bead ID prefix for the named rig.
// Returns empty string if rigName is empty or not found.
func findRigPrefix(rigName string, rigs []config.Rig) string {
	for i := range rigs {
		if rigs[i].Name == rigName {
			return rigs[i].EffectivePrefix()
		}
	}
	return ""
}

// defaultBranchFor returns the default branch for the repo at dir.
// Returns "main" on any error (best-effort).
func defaultBranchFor(dir string) string {
	if dir == "" {
		return "main"
	}
	g := git.New(dir)
	branch, _ := g.DefaultBranch()
	return branch
}

// promptFuncMap returns template functions available in prompt templates.
// sessionTemplate is the custom session naming template (empty = default).
// store is used by the "session" function to look up bead-derived session
// names; nil falls back to legacy naming.
func promptFuncMap(cityName, sessionTemplate string, store beads.Store) template.FuncMap {
	return template.FuncMap{
		"cmd": func() string {
			return filepath.Base(os.Args[0])
		},
		"session": func(agentName string) string {
			return lookupSessionNameOrLegacy(store, cityName, agentName, sessionTemplate)
		},
		"basename": func(qualifiedName string) string {
			_, name := config.ParseQualifiedName(qualifiedName)
			return name
		},
	}
}
