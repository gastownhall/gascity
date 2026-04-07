package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
)

// PromptContext holds template data for prompt rendering.
type PromptContext struct {
	CityRoot      string
	AgentName     string // qualified: "rig/polecat-1" or "mayor"
	TemplateName  string // config name: "polecat" (pool template) or "mayor" (singleton)
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
// stderr and returns empty string (triggering default prompt fallback).
func renderPrompt(fs fsys.FS, cityPath, cityName, templatePath string, ctx PromptContext, sessionTemplate string, stderr io.Writer, packDirs []string, injectFragments []string, store beads.Store) string {
	if templatePath == "" {
		return ""
	}
	sourcePath := filepath.Join(cityPath, templatePath)
	data, err := fs.ReadFile(sourcePath)
	if err != nil {
		return ""
	}
	raw := string(data)

	tmpl := template.New("prompt").
		Funcs(promptFuncMap(cityName, sessionTemplate, store)).
		Option("missingkey=zero")

	// Load shared templates from pack dirs (lower priority).
	// Each pack directory may contain a prompts/shared/ subdirectory.
	for _, dir := range packDirs {
		sharedDir := filepath.Join(dir, "prompts", "shared")
		loadSharedTemplates(fs, tmpl, sharedDir, stderr)
	}

	// Load shared templates from sibling shared/ directory (highest priority —
	// wins on name collision with cross-pack templates).
	sharedDir := filepath.Join(filepath.Dir(sourcePath), "shared")
	loadSharedTemplates(fs, tmpl, sharedDir, stderr)

	// Parse main template last — its body becomes the "prompt" template.
	tmpl, err = tmpl.Parse(raw)
	if err != nil {
		fmt.Fprintf(stderr, "gc: prompt template %q: %v\n", templatePath, err) //nolint:errcheck // best-effort stderr
		return ""
	}

	td := buildTemplateData(ctx)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, td); err != nil {
		fmt.Fprintf(stderr, "gc: prompt template %q: %v\n", templatePath, err) //nolint:errcheck // best-effort stderr
		return ""
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

// loadSharedTemplates loads all .md.tmpl files from a shared directory
// into the given template. Later calls override earlier definitions of
// the same name (last-writer-wins).
func loadSharedTemplates(fs fsys.FS, tmpl *template.Template, dir string, stderr io.Writer) {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md.tmpl") {
			if sdata, err := fs.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				if _, err := tmpl.Parse(string(sdata)); err != nil {
					fmt.Fprintf(stderr, "gc: shared template %q: %v\n", e.Name(), err) //nolint:errcheck // best-effort stderr
				}
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
