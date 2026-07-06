package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
)

// gcShadowCheck warns when the `gc` command is shadowed by a shell alias or
// function in contexts that agent sessions inherit.
//
// The field failure this catches: oh-my-zsh's git plugin defines
// `alias gc='git commit --verbose'`. Claude Code agent shells source the
// operator's shell snapshots (~/.claude/shell-snapshots/snapshot-*.sh), so an
// agent running `gc hook --claim --json` actually executes
// `git commit --verbose hook --claim --json` (exit 129, "error: unknown
// option 'claim'"), reports "No hooked work", and idles until manually
// nudged. The town looks dispatch-broken; the cause is operator shell config.
//
// Pure observability (SeverityAdvisory): it reads snapshot and rc files only,
// never mutates anything, and never gates. Detection is deterministic file
// scanning — no interactive shell is spawned (an interactive `$SHELL -ic` can
// hang on prompts and is not testable).
type gcShadowCheck struct {
	// homeDir is the operator home directory scanned for shell snapshots and
	// rc files. Injectable for tests.
	homeDir string
}

func newGCShadowCheck() *gcShadowCheck {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return &gcShadowCheck{homeDir: home}
}

func (c *gcShadowCheck) Name() string                     { return "gc-shell-alias-shadow" }
func (c *gcShadowCheck) CanFix() bool                     { return false }
func (c *gcShadowCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *gcShadowCheck) WarmupEligible() bool             { return false }

// gcShadowLineRe matches an uncommented shell line that (re)defines `gc` as
// an alias or function: `alias gc=...`, `alias -- gc=...`, `gc () {`, or
// `function gc ...`. It deliberately does not match other names (gcb, gca).
var gcShadowLineRe = regexp.MustCompile(`^\s*(alias\s+(--\s+)?gc=|gc\s*\(\)|function\s+gc\b)`)

// scanFileForGCShadow reports whether any line of the file defines a `gc`
// alias or function. Unreadable files report false: this check must never
// warn on what it cannot see.
func scanFileForGCShadow(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if gcShadowLineRe.MatchString(line) {
			return true
		}
	}
	return false
}

// findGCShadowFiles returns the files under homeDir where `gc` is shadowed:
// Claude Code shell snapshots (the exact vector agent sessions inherit) plus
// the common interactive rc files that regenerate those snapshots.
func findGCShadowFiles(homeDir string) (snapshots, rcFiles []string) {
	if strings.TrimSpace(homeDir) == "" {
		return nil, nil
	}
	pattern := filepath.Join(homeDir, ".claude", "shell-snapshots", "*.sh")
	matches, _ := filepath.Glob(pattern)
	sort.Strings(matches)
	for _, path := range matches {
		if scanFileForGCShadow(path) {
			snapshots = append(snapshots, path)
		}
	}
	for _, rc := range []string{".zshrc", ".bashrc"} {
		path := filepath.Join(homeDir, rc)
		if scanFileForGCShadow(path) {
			rcFiles = append(rcFiles, path)
		}
	}
	return snapshots, rcFiles
}

func (c *gcShadowCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}

	snapshots, rcFiles := findGCShadowFiles(c.homeDir)
	if len(snapshots) == 0 && len(rcFiles) == 0 {
		res.Status = doctor.StatusOK
		res.Message = "gc is not shadowed by a shell alias or function in agent-inherited shell config"
		return res
	}

	res.Status = doctor.StatusWarning
	res.Message = fmt.Sprintf("gc is shadowed by a shell alias/function in %d file(s) agent sessions can inherit", len(snapshots)+len(rcFiles))
	res.FixHint = "remove the gc alias from your shell config (e.g. `unalias gc` after oh-my-zsh loads), then start a fresh Claude Code session so shell snapshots regenerate"
	res.Details = []string{
		"A shell alias or function named `gc` (commonly oh-my-zsh's git plugin: alias gc='git commit --verbose')",
		"shadows the gc binary in agent sessions: Claude Code agent shells source the operator's shell snapshots,",
		"so `gc hook --claim --json` becomes `git commit --verbose hook --claim --json` (exit 129). Agents then",
		"report \"No hooked work\" and idle — polecats/refinery look dispatch-broken when the cause is shell config.",
	}
	for _, path := range snapshots {
		res.Details = append(res.Details, "shadowed in agent-inherited snapshot: "+path)
	}
	for _, path := range rcFiles {
		res.Details = append(res.Details, "shadowed in shell rc file: "+path)
	}
	return res
}
