// Package packcap computes a pack's capability surface and classifies the
// difference between two versions of a pack.
//
// A pack's declared version does not carry compatibility: packs declare
// "0.1.0" across many commits, and 0.x permits breaking changes in any
// release. This package computes the surface instead, the way public-API
// diffing tools do for code, and compares two surfaces.
//
// A [Manifest] holds six properties. The first four are what a pack commits
// to. The last two cover judgment: the half that can be computed and the
// half that can only be detected.
//
//	PROVIDES  commands, agents, formulas and orders the pack ships
//	MANDATES  claim and drain-ack commands its prose tells an agent to run
//	DEMANDS   reserved gc.* metadata keys and versioned schema ids it requires
//	USES      formula constructs it depends on
//	NORMS     normative lines in role prompts and template fragments
//	OPAQUE    a digest per file
//
// [Diff] classifies two manifests as BREAKING, UNCLASSIFIED, ADDITIVE or
// NONE, in that order of precedence. UNCLASSIFIED is never NONE: a moved
// file that no computable change accounts for is reported, because a
// rewritten prompt or script can change behavior without changing anything
// computable.
package packcap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

// Property names one dimension of a pack's capability surface.
type Property string

// The six properties of a capability manifest.
const (
	Provides Property = "PROVIDES"
	Mandates Property = "MANDATES"
	Demands  Property = "DEMANDS"
	Uses     Property = "USES"
	Norms    Property = "NORMS"
	Opaque   Property = "OPAQUE"
)

// Properties lists the six properties in manifest order.
var Properties = []Property{Provides, Mandates, Demands, Uses, Norms, Opaque}

// opaqueDigestLen is the number of hex digits of SHA-256 kept per file.
const opaqueDigestLen = 12

// stepConstructs are the formula step keys recorded under USES.
var stepConstructs = []string{"check", "retry", "drain", "on_complete", "gate", "loop"}

// Entry is one manifest line: a property and its value.
type Entry struct {
	Property Property `json:"property"`
	Value    string   `json:"value"`
}

// Manifest is a pack's capability surface: entries sorted byte-wise by
// "PROPERTY\tvalue", without duplicates.
type Manifest struct {
	Entries []Entry
}

// Values returns the sorted values recorded for one property.
func (m Manifest) Values(p Property) []string {
	var out []string
	for _, e := range m.Entries {
		if e.Property == p {
			out = append(out, e.Value)
		}
	}
	return out
}

// WriteTSV writes the manifest as one "PROPERTY<TAB>value" line per entry.
func (m Manifest) WriteTSV(w io.Writer) error {
	for _, e := range m.Entries {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", e.Property, e.Value); err != nil {
			return err
		}
	}
	return nil
}

// Compute reads the pack directory dir and returns its manifest. The result
// depends only on the directory's contents, not on where it lives. Compute
// writes nothing.
//
// A symlinked dir is resolved first. Inside the pack, a symlink to a regular
// file that resolves within the pack is read through; any other symlink
// (dangling, to a directory, or leaving the pack) is never followed and is
// recorded under OPAQUE by its link target string.
func Compute(dir string) (Manifest, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Manifest{}, fmt.Errorf("reading pack directory %q: %w", dir, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return Manifest{}, fmt.Errorf("reading pack directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return Manifest{}, fmt.Errorf("reading pack directory %q: not a directory", dir)
	}

	var b builder
	if err := b.provides(root); err != nil {
		return Manifest{}, err
	}
	if err := b.walk(root); err != nil {
		return Manifest{}, err
	}
	return b.manifest(), nil
}

type builder struct {
	entries []Entry
}

func (b *builder) add(p Property, value string) {
	if value != "" {
		b.entries = append(b.entries, Entry{Property: p, Value: value})
	}
}

func (b *builder) manifest() Manifest {
	line := func(e Entry) string { return string(e.Property) + "\t" + e.Value }
	sort.Slice(b.entries, func(i, j int) bool { return line(b.entries[i]) < line(b.entries[j]) })
	out := make([]Entry, 0, len(b.entries))
	for i, e := range b.entries {
		if i > 0 && e == b.entries[i-1] {
			continue
		}
		out = append(out, e)
	}
	return Manifest{Entries: out}
}

// provides records the commands, agents, formulas and orders the pack
// ships, using the same convention discovery the city loader uses, plus the
// legacy inline [[agent]] and [[commands]] declarations in pack.toml.
func (b *builder) provides(dir string) error {
	osfs := fsys.OSFS{}

	if err := b.inlineProvides(dir); err != nil {
		return err
	}

	commands, err := config.DiscoverPackCommands(osfs, dir, "")
	if err != nil {
		return fmt.Errorf("discovering commands in %q: %w", dir, err)
	}
	for _, c := range commands {
		b.add(Provides, "command:"+strings.Join(c.Command, " "))
	}

	agents, err := config.DiscoverPackAgents(osfs, dir, "", nil)
	if err != nil {
		return fmt.Errorf("discovering agents in %q: %w", dir, err)
	}
	for _, a := range agents {
		b.add(Provides, "agent:"+a.Name)
	}
	// roles/agents/ is the older role-prompt layout; each subdirectory is
	// an agent.
	legacy, err := subdirNames(filepath.Join(dir, "roles", "agents"))
	if err != nil {
		return err
	}
	for _, name := range legacy {
		b.add(Provides, "agent:"+name)
	}

	formulas, err := os.ReadDir(filepath.Join(dir, "formulas"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading formulas in %q: %w", dir, err)
	}
	for _, f := range formulas {
		if f.IsDir() {
			continue
		}
		if name, ok := formula.TrimTOMLFilename(f.Name()); ok && name != "" {
			b.add(Provides, "formula:"+name)
		}
	}

	orderFiles, err := os.ReadDir(filepath.Join(dir, "orders"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading orders in %q: %w", dir, err)
	}
	for _, f := range orderFiles {
		if f.IsDir() {
			continue
		}
		if name, ok := orders.TrimFlatOrderFilename(f.Name()); ok && name != "" {
			b.add(Provides, "order:"+name)
		}
	}
	return nil
}

// inlineProvides records the agents and commands declared inline in
// pack.toml, decoded with the loader's own PackConfig type. A pack without
// pack.toml declares none.
func (b *builder) inlineProvides(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading pack.toml in %q: %w", dir, err)
	}
	var pc config.PackConfig
	if _, err := toml.Decode(string(data), &pc); err != nil {
		return fmt.Errorf("parsing pack.toml: %w", err)
	}
	for _, a := range pc.Agents {
		b.add(Provides, "agent:"+a.Name)
	}
	for _, c := range pc.Commands {
		b.add(Provides, "command:"+c.Name)
	}
	return nil
}

// subdirNames lists the visible subdirectories of dir. A missing dir has
// none.
func subdirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
			out = append(out, name)
		}
	}
	return out, nil
}

// walk records an OPAQUE digest for every file in the pack except git
// metadata, and extracts MANDATES, DEMANDS, NORMS and USES from prompt
// templates, Markdown and TOML files.
func (b *builder) walk(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %q: %w", path, err)
		}
		if d.Name() == ".git" && path != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativizing %q: %w", path, err)
		}
		rel := filepath.ToSlash(relPath)

		readPath := path
		switch {
		case d.Type().IsRegular():
		case d.Type()&fs.ModeSymlink != 0:
			target, ok := symlinkInside(root, path)
			if !ok {
				link, err := os.Readlink(path)
				if err != nil {
					return fmt.Errorf("reading %s: %w", rel, err)
				}
				b.add(Opaque, rel+" "+digest([]byte("symlink:"+link)))
				return nil
			}
			readPath = target
		default:
			return nil
		}

		data, err := os.ReadFile(readPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", rel, err)
		}
		b.add(Opaque, rel+" "+digest(data))
		isTOML := strings.HasSuffix(rel, ".toml")
		if isTOML || isPromptFile(rel) {
			b.prose(rel, string(data))
		}
		if isTOML {
			if err := b.uses(rel, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// symlinkInside resolves the symlink at path and reports whether it names a
// regular file inside root, returning the resolved path. A dangling link, a
// link to a directory and a link leaving root are not inside; the caller
// digests those by their target string instead of reading them.
func symlinkInside(root, path string) (string, bool) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return target, true
}

// promptSuffixes are the prompt and template-fragment file suffixes the
// loader reads: canonical .template.md, legacy .md.tmpl and plain .md.
var promptSuffixes = []string{".md", ".md.tmpl"}

// isPromptFile reports whether a pack-relative path names Markdown or a
// prompt template the loader reads.
func isPromptFile(rel string) bool {
	for _, s := range promptSuffixes {
		if strings.HasSuffix(rel, s) {
			return true
		}
	}
	return false
}

// digest returns the OPAQUE digest of a file's bytes.
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:opaqueDigestLen]
}

// prose records the line-based properties of one file.
func (b *builder) prose(rel, text string) {
	judgment := isJudgmentSurface(rel)
	for _, line := range splitLines(text) {
		for _, m := range mandatesOnLine(line) {
			b.add(Mandates, m)
		}
		for _, d := range demandsOnLine(line) {
			b.add(Demands, d)
		}
		if judgment {
			if n, ok := normOnLine(line); ok {
				b.add(Norms, rel+": "+n)
			}
		}
	}
}

// uses records the formula constructs a TOML file depends on: each step
// construct key on a step, template step or nested child step, and a
// declared formula compiler requirement with its file and value.
func (b *builder) uses(rel string, data []byte) error {
	var doc map[string]any
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", rel, err)
	}
	if req, ok := doc["requires"].(map[string]any); ok {
		if v, ok := req["formula_compiler"]; ok {
			b.add(Uses, rel+": "+formulaCompilerUse(v))
		}
	}
	b.stepUses(doc["steps"])
	b.stepUses(doc["template"])
	return nil
}

// formulaCompilerUse renders a formula_compiler requirement with its value.
// The caller prefixes the declaring file, so raising one formula's
// requirement is a removal plus an addition even when another formula keeps
// the old value.
func formulaCompilerUse(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("requires.formula_compiler = %q", s)
	}
	return fmt.Sprintf("requires.formula_compiler = %v", v)
}

func (b *builder) stepUses(v any) {
	switch steps := v.(type) {
	case []map[string]any:
		for _, s := range steps {
			b.stepUses(s)
		}
	case []any:
		for _, s := range steps {
			b.stepUses(s)
		}
	case map[string]any:
		for _, c := range stepConstructs {
			if _, ok := steps[c]; ok {
				b.add(Uses, "steps."+c)
			}
		}
		b.stepUses(steps["children"])
	}
}
