// Package docsync verifies that tutorial prose and testscript txtar files
// cover the same set of gc commands. Every `$ gc <verb>` in a tutorial
// markdown must have a corresponding `exec gc <verb>` in the txtar.
package docsync

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/docgen"
)

func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

var markdownLinkRE = regexp.MustCompile(`\[[^][]+\]\(([^)]+)\)`)

// docTreeDirs lists the top-level directories that are documentation trees
// and should be link-checked. Update this list when adding or removing doc
// directories. TestDocDirCoverage will fail if a new directory with markdown
// appears that is not accounted for here or in docTreeIgnored.
var docTreeDirs = []string{"contrib", "docs", "engdocs"}

// docTreeIgnored lists directories that contain markdown but are not
// documentation trees (e.g., embedded prompt templates, test fixtures,
// gitignored scratch space for local work).
var docTreeIgnored = []string{"cmd", "examples", "internal", "plans", "release-gates", "scripts", "test", "tmp"}

// knownBrokenLinks lists links to docs that do not exist yet. These are
// excluded from TestLocalMarkdownLinks failures but still logged. Remove
// entries as the missing docs are created.
// See: https://github.com/gastownhall/gascity/issues (file upstream)
var knownBrokenLinks = map[string]bool{
	"contrib/events-scripts/README.md -> ../../docs/k8s-guide.md":  true,
	"contrib/session-scripts/README.md -> ../../docs/k8s-guide.md": true,
}

func allDocsMarkdownFiles(root string) ([]string, error) {
	var files []string

	// Root-level markdown files.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext == ".md" || ext == ".mdx" {
			files = append(files, filepath.Join(root, e.Name()))
		}
	}

	// Walk every doc tree directory.
	for _, dir := range docTreeDirs {
		dirRoot := filepath.Join(root, dir)
		err := filepath.WalkDir(dirRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext == ".md" || ext == ".mdx" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sort.Strings(files)
	return files, nil
}

func publicSurfaceMarkdownFiles(root string) ([]string, error) {
	all, err := allDocsMarkdownFiles(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, path := range all {
		// Skip archive subdirectories in any doc tree.
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		isArchive := false
		for _, p := range parts {
			if p == "archive" {
				isArchive = true
				break
			}
		}
		if !isArchive {
			out = append(out, path)
		}
	}
	return out, nil
}

func extractMarkdownLinks(content string) []string {
	matches := markdownLinkRE.FindAllStringSubmatchIndex(content, -1)
	var links []string
	for _, m := range matches {
		start := m[0]
		if start > 0 && content[start-1] == '!' {
			continue
		}
		target := content[m[2]:m[3]]
		target = strings.TrimSpace(target)
		target = strings.Trim(target, "<>")
		if target == "" {
			continue
		}
		if idx := strings.Index(target, ` "`); idx >= 0 {
			target = target[:idx]
		}
		links = append(links, target)
	}
	return links
}

func isExternalLink(target string) bool {
	switch {
	case strings.HasPrefix(target, "http://"),
		strings.HasPrefix(target, "https://"),
		strings.HasPrefix(target, "mailto:"),
		strings.HasPrefix(target, "tel:"),
		strings.HasPrefix(target, "app://"),
		strings.HasPrefix(target, "plugin://"),
		strings.HasPrefix(target, "#"):
		return true
	default:
		return false
	}
}

// sourceTreeRoot returns the top-level doc directory that contains sourcePath,
// or "" if sourcePath is a root-level file. For example, if sourcePath is
// /repo/engdocs/architecture/foo.md and root is /repo, this returns "engdocs".
func sourceTreeRoot(root, sourcePath string) string {
	rel, err := filepath.Rel(root, sourcePath)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
	if len(parts) < 2 {
		return "" // root-level file
	}
	dir := filepath.Join(root, parts[0])
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return parts[0]
}

func resolveLocalLink(root, sourcePath, target string) string {
	if idx := strings.Index(target, "#"); idx >= 0 {
		target = target[:idx]
	}
	if target == "" {
		return ""
	}
	if strings.HasPrefix(target, "/") {
		// Absolute links resolve against docs/, the document root. This
		// matches the standard convention and Mintlify's behavior.
		// Absolute paths work from any tree, but from engdocs/ or
		// contrib/ they can be confusing since /foo resolves to
		// docs/foo, not a sibling in the same tree.
		target = strings.TrimPrefix(target, "/")
		target = filepath.FromSlash(target)
		return filepath.Clean(filepath.Join(root, "docs", target))
	}
	target = filepath.FromSlash(target)
	return filepath.Clean(filepath.Join(filepath.Dir(sourcePath), target))
}

func localLinkExists(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	switch ext := filepath.Ext(path); ext {
	case "":
		// Try .md then .mdx (Mintlify format), then index files.
		for _, try := range []string{
			path + ".md",
			path + ".mdx",
			filepath.Join(path, "index.md"),
			filepath.Join(path, "index.mdx"),
		} {
			if _, err := os.Stat(try); err == nil {
				return true
			}
		}
	case ".md":
		// Try .mdx variant.
		mdxPath := strings.TrimSuffix(path, ".md") + ".mdx"
		if _, err := os.Stat(mdxPath); err == nil {
			return true
		}
	}
	return false
}

func collectMintPages(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "pages" {
				if arr, ok := child.([]any); ok {
					for _, item := range arr {
						if s, ok := item.(string); ok {
							*out = append(*out, s)
						}
					}
				}
			}
			collectMintPages(child, out)
		}
	case []any:
		for _, child := range x {
			collectMintPages(child, out)
		}
	}
}

// gcVerbsFromMarkdown extracts unique gc subcommands from code blocks.
// Only matches unindented `$ gc ...` lines to skip agent conversations.
func gcVerbsFromMarkdown(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	verbs := make(map[string]bool)
	inCodeBlock := false
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if !inCodeBlock {
			continue
		}
		if strings.HasPrefix(line, "$ gc ") {
			verb := extractVerb(line[len("$ gc "):])
			if verb != "" {
				verbs[verb] = true
			}
		}
	}
	return verbs, scanner.Err()
}

// gcVerbsFromTxtar extracts unique gc subcommands from exec lines.
// Recognizes both active ("exec gc ...") and commented-out ("# exec gc ...")
// lines so that planned-but-unimplemented commands count as covered.
func gcVerbsFromTxtar(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	verbs := make(map[string]bool)
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		after, ok := strings.CutPrefix(line, "exec gc ")
		if !ok {
			after, ok = strings.CutPrefix(line, "# exec gc ")
			if !ok {
				continue
			}
		}
		verb := extractVerb(after)
		if verb != "" {
			verbs[verb] = true
		}
	}
	return verbs, scanner.Err()
}

// extractVerb pulls the subcommand (up to 2 lowercase words) from args.
// "rig add ~/foo" → "rig add", "bead show gc-1" → "bead show",
// "start $WORK/x" → "start".
func extractVerb(args string) string {
	words := strings.Fields(args)
	var parts []string
	for i, w := range words {
		if i >= 2 {
			break
		}
		if !isLowerAlpha(w) {
			break
		}
		parts = append(parts, w)
	}
	return strings.Join(parts, " ")
}

func isLowerAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

func TestTutorial01CommandSync(t *testing.T) {
	root := repoRoot()
	tutorial := filepath.Join(root, "docs", "tutorials", "01-cities-and-rigs.md")
	txtar := filepath.Join(root, "cmd", "gc", "testdata", "01-hello-gas-city.txtar")

	mdVerbs, err := gcVerbsFromMarkdown(tutorial)
	if err != nil {
		t.Fatalf("parsing tutorial: %v", err)
	}

	txtarVerbs, err := gcVerbsFromTxtar(txtar)
	if err != nil {
		t.Fatalf("parsing txtar: %v", err)
	}

	// Every tutorial command must have txtar coverage.
	var missing []string
	for verb := range mdVerbs {
		if !txtarVerbs[verb] {
			missing = append(missing, verb)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("gc commands in tutorial but not in txtar:")
		for _, v := range missing {
			t.Errorf("  gc %s", v)
		}
	}

	// Log txtar commands not in tutorial (info only — txtar may test more
	// than what's documented, which is fine).
	var extra []string
	for verb := range txtarVerbs {
		if !mdVerbs[verb] {
			extra = append(extra, verb)
		}
	}

	if len(extra) > 0 {
		sort.Strings(extra)
		t.Logf("gc commands in txtar but not in tutorial (ok — extra test coverage):")
		for _, v := range extra {
			t.Logf("  gc %s", v)
		}
	}
}

func TestSchemaFreshness(t *testing.T) {
	root := repoRoot()

	// Generate schemas in memory and compare against committed files.
	tests := []struct {
		name     string
		generate func() ([]byte, error)
		path     string
	}{
		{
			name: "city-schema.json",
			generate: func() ([]byte, error) {
				s, err := docgen.GenerateCitySchema()
				if err != nil {
					return nil, err
				}
				data, err := json.MarshalIndent(s, "", "  ")
				if err != nil {
					return nil, err
				}
				return append(data, '\n'), nil
			},
			path: filepath.Join(root, "docs", "schema", "city-schema.json"),
		},
		{
			name: "config.md",
			generate: func() ([]byte, error) {
				s, err := docgen.GenerateCitySchema()
				if err != nil {
					return nil, err
				}
				var buf bytes.Buffer
				if err := docgen.RenderMarkdown(&buf, s); err != nil {
					return nil, err
				}
				return buf.Bytes(), nil
			},
			path: filepath.Join(root, "docs", "reference", "config.md"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			generated, err := tt.generate()
			if err != nil {
				t.Fatalf("generating %s: %v", tt.name, err)
			}

			committed, err := os.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("reading %s: %v\nRun: go run ./cmd/genschema", tt.path, err)
			}

			if !bytes.Equal(generated, committed) {
				t.Errorf("%s is stale. Run: go run ./cmd/genschema", tt.name)
			}
		})
	}
}

// isMintlifySource returns true if path belongs to a doc tree that has a
// Mintlify config (docs.json). In Mintlify trees, extensionless root-relative
// links like /tutorials/01-beads are the expected convention. Other trees are
// GitHub-only and must use explicit .md extensions.
func isMintlifySource(root, path string) bool {
	tree := sourceTreeRoot(root, path)
	if tree == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(root, tree, "docs.json"))
	return err == nil
}

func TestLocalMarkdownLinks(t *testing.T) {
	root := repoRoot()
	files, err := allDocsMarkdownFiles(root)
	if err != nil {
		t.Fatalf("collecting markdown files: %v", err)
	}

	var broken []string
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		mintlify := isMintlifySource(root, path)
		for _, target := range extractMarkdownLinks(string(data)) {
			if isExternalLink(target) {
				continue
			}
			resolved := resolveLocalLink(root, path, target)
			if resolved == "" {
				continue
			}
			if mintlify {
				// Mintlify docs: extensionless links are OK (deployed
				// site uses route-based URLs without .md).
				if !localLinkExists(resolved) {
					relPath, _ := filepath.Rel(root, path)
					broken = append(broken, relPath+" -> "+target)
				}
			} else {
				// engdocs and root files: GitHub-only, require exact
				// file paths. No extensionless fallback.
				if _, err := os.Stat(resolved); err != nil {
					relPath, _ := filepath.Rel(root, path)
					broken = append(broken, relPath+" -> "+target)
				}
			}
		}
	}

	sort.Strings(broken)
	var unexpected []string
	for _, item := range broken {
		if !knownBrokenLinks[item] {
			unexpected = append(unexpected, item)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("broken local markdown links:")
		for _, item := range unexpected {
			t.Errorf("  %s", item)
		}
	}
	if len(broken) > 0 && len(unexpected) == 0 {
		t.Logf("%d known-broken links (see knownBrokenLinks allowlist)", len(broken))
	}
}

func TestMintNavigationPagesExist(t *testing.T) {
	root := repoRoot()
	configPath := filepath.Join(root, "docs", "docs.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading docs.json: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("parsing docs.json: %v", err)
	}

	var pages []string
	collectMintPages(decoded, &pages)
	sort.Strings(pages)

	var missing []string
	for _, page := range pages {
		path := filepath.Join(root, "docs", filepath.FromSlash(page))
		if filepath.Ext(path) == "" {
			path += ".md"
		}
		if _, err := os.Stat(path); err != nil {
			// Also try .mdx (Mintlify format).
			mdxPath := strings.TrimSuffix(path, ".md") + ".mdx"
			if _, err2 := os.Stat(mdxPath); err2 != nil {
				missing = append(missing, page)
			}
		}
	}

	if len(missing) > 0 {
		t.Errorf("docs.json references missing pages:")
		for _, page := range missing {
			t.Errorf("  %s", page)
		}
	}
}

func TestNoKnownStaleDocReferences(t *testing.T) {
	root := repoRoot()
	files, err := publicSurfaceMarkdownFiles(root)
	if err != nil {
		t.Fatalf("collecting public markdown files: %v", err)
	}

	banned := []string{
		"internal/session/session.go",
		"internal/session/fingerprint.go",
		"docs/primitive-test.md",
		"02-named-crew.md",
		"04-agent-team.md",
		"progression.md",
		"mail-roadmap.md",
		"agent.NewFake",
		"session.Fake",
		"agent.Fake",
		"internal/dolt",
	}

	var hits []string
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		text := string(data)
		relPath, _ := filepath.Rel(root, path)
		for _, pattern := range banned {
			if strings.Contains(text, pattern) {
				hits = append(hits, relPath+" -> "+pattern)
			}
		}
	}

	if len(hits) > 0 {
		sort.Strings(hits)
		t.Errorf("found stale doc references:")
		for _, hit := range hits {
			t.Errorf("  %s", hit)
		}
	}
}

// TestDocDirCoverage fails if a new top-level directory containing markdown
// files exists that is not listed in docTreeDirs or docTreeIgnored. This
// prevents silent gaps when new doc directories are added.
func TestDocDirCoverage(t *testing.T) {
	root := repoRoot()
	known := make(map[string]bool)
	for _, d := range docTreeDirs {
		known[d] = true
	}
	for _, d := range docTreeIgnored {
		known[d] = true
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading repo root: %v", err)
	}

	var uncovered []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" {
			continue
		}
		if known[name] {
			continue
		}
		// Check if this directory contains any markdown.
		dirPath := filepath.Join(root, name)
		hasMarkdown := false
		_ = filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil || hasMarkdown {
				return filepath.SkipDir
			}
			if !d.IsDir() {
				ext := filepath.Ext(path)
				if ext == ".md" || ext == ".mdx" {
					hasMarkdown = true
					return filepath.SkipAll
				}
			}
			return nil
		})
		if hasMarkdown {
			uncovered = append(uncovered, name)
		}
	}

	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		t.Errorf("directories with markdown not in docTreeDirs or docTreeIgnored " +
			"(add to the appropriate list in docsync_test.go):")
		for _, d := range uncovered {
			t.Errorf("  %s", d)
		}
	}
}
