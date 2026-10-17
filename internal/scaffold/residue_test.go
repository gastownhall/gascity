package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/reviewquorum"
)

// headGitignore is the committed .gitignore used across these tests.
const headGitignore = "node_modules/\n.beads/proxieddb/\n"

// gasCityBlock is what gc appends to a worktree .gitignore. Its contents
// differ per worktree in the field, which is why the implementation parses
// the marker instead of matching fixed text.
const gasCityBlock = "\n# Gas City\n.mcp.json\n.gemini/settings.json\nopencode.json\n"

func parse(porcelain string) []reviewquorum.StatusEntry {
	return reviewquorum.ParseStatusPorcelain(porcelain)
}

func paths(entries []reviewquorum.StatusEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

func assertContains(t *testing.T, entries []reviewquorum.StatusEntry, want string) {
	t.Helper()
	for _, e := range entries {
		if e.Path == want {
			return
		}
	}
	t.Fatalf("residue %v is missing %q", paths(entries), want)
}

func trimTrailing(s string) string { return strings.TrimRight(s, "\n") }

func headFileFor(files map[string]string) HeadFileFunc {
	return func(rel string) ([]byte, error) {
		content, ok := files[rel]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(content), nil
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newScaffoldedWorktree builds a worktree in the exact shape gc leaves one:
// an appended .gitignore block, an adopted .mcp.json with its managed marker,
// and materialized skill symlinks named by an ownership manifest.
func newScaffoldedWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mcpContent := `{"mcpServers":{"bartertown":{"command":"/x/bartertown_mcp.py"}}}` + "\n"

	writeFile(t, filepath.Join(dir, ".gitignore"), headGitignore+gasCityBlock)
	writeFile(t, filepath.Join(dir, ".mcp.json"), mcpContent)

	marker, err := json.Marshal(map[string]string{
		"managed_by":     "gc",
		"provider":       "claude",
		"content_sha256": sha256Hex(mcpContent),
	})
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".gc", "mcp-managed", "claude.json"), string(marker))

	manifest, err := json.Marshal(map[string]any{
		"targets": map[string]string{
			"core.gc-mail": "/cache/packs/core/skills/gc-mail",
			"core.gc-work": "/cache/packs/core/skills/gc-work",
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeFile(t, filepath.Join(dir, ".claude", "skills", ownershipManifestFile), string(manifest))
	writeFile(t, filepath.Join(dir, ".claude", "skills", "core.gc-mail"), "symlink-stand-in")
	writeFile(t, filepath.Join(dir, ".claude", "skills", "core.gc-work"), "symlink-stand-in")

	return dir
}

// fullScaffoldPorcelain is the status a healthy, untouched gc worktree
// reports — the 100%-of-worktrees case from gas-wr8.
const fullScaffoldPorcelain = ` M .gitignore
 M .mcp.json
?? .claude/skills/.gc-skill-ownership.json
?? .claude/skills/core.gc-mail
?? .claude/skills/core.gc-work
`

func TestResidueEmptyForPureScaffolding(t *testing.T) {
	dir := newScaffoldedWorktree(t)
	head := headFileFor(map[string]string{".gitignore": headGitignore})

	if got := HasRealWork(dir, fullScaffoldPorcelain, head); got {
		residue := Residue(dir, parse(fullScaffoldPorcelain), head)
		t.Fatalf("pure gc scaffolding classified as real work; residue = %v", paths(residue))
	}
}

func TestResidueKeepsRealWork(t *testing.T) {
	dir := newScaffoldedWorktree(t)
	head := headFileFor(map[string]string{".gitignore": headGitignore})
	porcelain := fullScaffoldPorcelain + " M internal/git/git.go\n?? notes.md\n"

	residue := Residue(dir, parse(porcelain), head)
	if got := paths(residue); len(got) != 2 {
		t.Fatalf("residue = %v, want exactly the two non-gc paths", got)
	}
	assertContains(t, residue, "internal/git/git.go")
	assertContains(t, residue, "notes.md")
}

// A skill the agent placed by hand shares the sink directory with gc's own,
// but is absent from the manifest — it is work, and must survive.
func TestResidueKeepsUnmanifestedSinkEntry(t *testing.T) {
	dir := newScaffoldedWorktree(t)
	head := headFileFor(map[string]string{".gitignore": headGitignore})
	writeFile(t, filepath.Join(dir, ".claude", "skills", "my-own-skill"), "mine")
	porcelain := fullScaffoldPorcelain + "?? .claude/skills/my-own-skill\n"

	residue := Residue(dir, parse(porcelain), head)
	assertContains(t, residue, ".claude/skills/my-own-skill")
	if len(residue) != 1 {
		t.Fatalf("residue = %v, want only the hand-placed skill", paths(residue))
	}
}

// The destructive direction the gate exists to prevent: a real edit to the
// shared, tracked .gitignore must not be swallowed by the block subtraction.
func TestResidueKeepsGitignoreEditBesideGasCityBlock(t *testing.T) {
	dir := newScaffoldedWorktree(t)
	writeFile(t, filepath.Join(dir, ".gitignore"), headGitignore+"coverage.out\n"+gasCityBlock)
	head := headFileFor(map[string]string{".gitignore": headGitignore})

	residue := Residue(dir, parse(fullScaffoldPorcelain), head)
	assertContains(t, residue, ".gitignore")
}

func TestResidueKeepsEditedMCPConfig(t *testing.T) {
	dir := newScaffoldedWorktree(t)
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"hand-edited":{}}}`+"\n")
	head := headFileFor(map[string]string{".gitignore": headGitignore})

	residue := Residue(dir, parse(fullScaffoldPorcelain), head)
	assertContains(t, residue, ".mcp.json")
}

// A marker predating content hashing proves gc adopted the file but not that
// the bytes are still gc's, so it must fail closed.
func TestResidueKeepsMCPConfigWhenMarkerHasNoHash(t *testing.T) {
	dir := newScaffoldedWorktree(t)
	writeFile(t, filepath.Join(dir, ".gc", "mcp-managed", "claude.json"),
		`{"managed_by":"gc","provider":"claude"}`)
	head := headFileFor(map[string]string{".gitignore": headGitignore})

	residue := Residue(dir, parse(fullScaffoldPorcelain), head)
	assertContains(t, residue, ".mcp.json")
}

// gc only ever writes these paths, so a deleted one is a change gc did not
// make and must not be subtracted.
func TestResidueKeepsDeletedScaffolding(t *testing.T) {
	dir := newScaffoldedWorktree(t)
	head := headFileFor(map[string]string{".gitignore": headGitignore})

	residue := Residue(dir, parse(" D .mcp.json\n"), head)
	assertContains(t, residue, ".mcp.json")
}

// Ownership records are what make a path provable. Without the manifest and
// the managed marker, the skill entries and the MCP config are indistinguishable
// from agent work and must all survive — only .gitignore, whose proof is the
// parsed block rather than an on-disk record, is still subtracted.
func TestResidueWithoutOwnershipRecordsKeepsEverythingProvableOnlyByRecord(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), headGitignore+gasCityBlock)
	head := headFileFor(map[string]string{".gitignore": headGitignore})

	residue := Residue(dir, parse(fullScaffoldPorcelain), head)
	want := []string{
		".claude/skills/.gc-skill-ownership.json",
		".claude/skills/core.gc-mail",
		".claude/skills/core.gc-work",
		".mcp.json",
	}
	if got := paths(residue); len(got) != len(want) {
		t.Fatalf("residue = %v, want %v", got, want)
	}
	for _, w := range want {
		assertContains(t, residue, w)
	}
}

// A nil headFile disables only the .gitignore subtraction; the rest still work.
func TestResidueWithNilHeadFileKeepsGitignore(t *testing.T) {
	dir := newScaffoldedWorktree(t)

	residue := Residue(dir, parse(fullScaffoldPorcelain), nil)
	if got := paths(residue); len(got) != 1 || got[0] != ".gitignore" {
		t.Fatalf("residue = %v, want only .gitignore", got)
	}
}

func TestStripGasCityBlocksRemovesEveryBlock(t *testing.T) {
	content := headGitignore + gasCityBlock + "\n# Gas City\n.beads/*\n!.beads/identity.toml\n"
	if got := stripGasCityBlocks(content); trimTrailing(got) != trimTrailing(headGitignore) {
		t.Fatalf("stripGasCityBlocks = %q, want %q", got, headGitignore)
	}
}

// A user comment that merely starts with the marker text must not swallow the
// user's own following entries.
func TestStripGasCityBlocksStopsAtBlankLine(t *testing.T) {
	content := "# Gas City\n.mcp.json\n\nkeep-me/\n"
	want := "\nkeep-me/\n"
	if got := stripGasCityBlocks(content); got != want {
		t.Fatalf("stripGasCityBlocks = %q, want %q", got, want)
	}
}
