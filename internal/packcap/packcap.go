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
//	PROVIDES  commands, agents and formulas the pack ships
//	MANDATES  claim and drain-ack commands its prose tells an agent to run
//	DEMANDS   reserved gc.* metadata keys and versioned schema ids it requires
//	USES      formula constructs it depends on
//	NORMS     normative lines in role prompts and template fragments
//	OPAQUE    a digest per prose file
//
// [Diff] classifies two manifests as BREAKING, ADDITIVE, UNCLASSIFIED or
// NONE. UNCLASSIFIED is never NONE: moved prose with an unchanged surface is
// reported, because a rewritten prompt can change an agent's judgment
// without changing anything computable.
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

// opaqueDigestLen is the number of hex digits of SHA-256 kept per prose file.
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
func Compute(dir string) (Manifest, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return Manifest{}, fmt.Errorf("reading pack directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return Manifest{}, fmt.Errorf("reading pack directory %q: not a directory", dir)
	}

	var b builder
	if err := b.provides(dir); err != nil {
		return Manifest{}, err
	}
	if err := b.walk(dir); err != nil {
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

// provides records the commands, agents and formulas the pack ships, using
// the same convention discovery the city loader uses.
func (b *builder) provides(dir string) error {
	osfs := fsys.OSFS{}

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

// walk extracts MANDATES, DEMANDS, USES, NORMS and OPAQUE from every
// Markdown and TOML file in the pack.
func (b *builder) walk(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %q: %w", path, err)
		}
		if d.IsDir() {
			if d.Name() == ".git" && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		isMarkdown := strings.HasSuffix(d.Name(), ".md")
		isTOML := strings.HasSuffix(d.Name(), ".toml")
		if !isMarkdown && !isTOML {
			return nil
		}
		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("relativizing %q: %w", path, err)
		}
		rel := filepath.ToSlash(relPath)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", rel, err)
		}
		b.prose(rel, string(data))
		if isTOML {
			if err := b.uses(rel, data); err != nil {
				return err
			}
		}
		if isMarkdown {
			sum := sha256.Sum256(data)
			b.add(Opaque, rel+" "+hex.EncodeToString(sum[:])[:opaqueDigestLen])
		}
		return nil
	})
}

// prose records the line-based properties of one file.
func (b *builder) prose(rel, text string) {
	judgment := isJudgmentSurface(rel)
	for _, line := range splitLines(text) {
		if m, ok := mandateOnLine(line); ok {
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
// declared formula compiler requirement.
func (b *builder) uses(rel string, data []byte) error {
	var doc map[string]any
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", rel, err)
	}
	if req, ok := doc["requires"].(map[string]any); ok {
		if _, ok := req["formula_compiler"]; ok {
			b.add(Uses, "requires.formula_compiler")
		}
	}
	b.stepUses(doc["steps"])
	b.stepUses(doc["template"])
	return nil
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
