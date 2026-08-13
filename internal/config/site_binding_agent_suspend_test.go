package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Regression coverage for ga-gc16k3: gc agent suspend/resume on an inline
// [[agent]] currently rewrites the whole of city.toml through a bare
// struct-marshal round-trip (writeCityAndRigSiteBindingsForEdit), which has
// no comment model at all -- every comment, and any TOML shape the struct
// doesn't preserve byte-for-byte, is silently dropped. These tests exercise
// WriteCityAgentSuspendedForEdit, which must instead toggle the `suspended`
// key directly in the on-disk bytes.

// TestWriteCityAgentSuspendedForEdit_PreservesCommentsAcrossSuspendResume
// covers AC1: suspending then resuming an inline agent must retain 100% of
// the original comment lines, verbatim, in order.
func TestWriteCityAgentSuspendedForEdit_PreservesCommentsAcrossSuspendResume(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte(`# City configuration for Acme Corp.
# Owned by the platform team; do not edit without review.

[workspace]
name = "acme-city"

# Primary orchestrator agent -- keep this one pinned to opus.
[[agent]]
name = "mayor"
provider = "claude"  # pinned for stability, see runbook

[[agent]]
name = "worker"
provider = "codex"
`)
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	setMayorSuspended(cfg, true)
	if err := WriteCityAgentSuspendedForEdit(fsys.OSFS{}, cityPath, cfg, "mayor", true); err != nil {
		t.Fatalf("WriteCityAgentSuspendedForEdit(suspend): %v", err)
	}

	cfg, err = Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("reload after suspend: %v", err)
	}
	setMayorSuspended(cfg, false)
	if err := WriteCityAgentSuspendedForEdit(fsys.OSFS{}, cityPath, cfg, "mayor", false); err != nil {
		t.Fatalf("WriteCityAgentSuspendedForEdit(resume): %v", err)
	}

	final, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	want := strings.Join(commentLines(original), "\n")
	got := strings.Join(commentLines(final), "\n")
	if want != got {
		t.Fatalf("comment lines not preserved across suspend+resume:\nwant:\n%s\ngot:\n%s\nfull result:\n%s", want, got, final)
	}
}

// TestWriteCityAgentSuspendedForEdit_DoesNotMaterializeAbsentTableOrReflowArray
// covers AC2: the write must not materialize a previously-absent table as an
// explicit zero value, and must not reflow a pre-existing single-line array.
func TestWriteCityAgentSuspendedForEdit_DoesNotMaterializeAbsentTableOrReflowArray(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte(`[workspace]
name = "acme-city"

[[agent]]
name = "mayor"
args = ["--flag-one", "--flag-two"]
`)
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	setMayorSuspended(cfg, true)
	if err := WriteCityAgentSuspendedForEdit(fsys.OSFS{}, cityPath, cfg, "mayor", true); err != nil {
		t.Fatalf("WriteCityAgentSuspendedForEdit: %v", err)
	}

	final, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(final), "[dolt]") {
		t.Fatalf("absent [dolt] table was materialized:\n%s", final)
	}
	if !strings.Contains(string(final), `args = ["--flag-one", "--flag-two"]`) {
		t.Fatalf("pre-existing single-line array was reflowed:\n%s", final)
	}
}

// TestWriteCityAgentSuspendedForEdit_DiffLimitedToSuspendedKey covers AC3's
// suspend direction: with comments stripped from both sides, the
// post-mutation city.toml must equal the pre-mutation city.toml except for
// the suspended key's line toggling in place (false -> true).
func TestWriteCityAgentSuspendedForEdit_DiffLimitedToSuspendedKey(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte(`# header comment
[workspace]
name = "acme-city"

[[agent]]
name = "mayor"
provider = "claude"
suspended = false
pre_start = ["echo starting", "echo ready"]

[[agent]]
name = "worker"
provider = "codex"
`)
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	setMayorSuspended(cfg, true)
	if err := WriteCityAgentSuspendedForEdit(fsys.OSFS{}, cityPath, cfg, "mayor", true); err != nil {
		t.Fatalf("WriteCityAgentSuspendedForEdit: %v", err)
	}

	final, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	before := strippedLines(original)
	after := strippedLines(final)
	if len(before) != len(after) {
		t.Fatalf("line count changed: before=%d after=%d\nbefore:\n%s\nafter:\n%s", len(before), len(after), original, final)
	}
	changed := 0
	for i := range before {
		if before[i] == after[i] {
			continue
		}
		changed++
		if !strings.Contains(after[i], "suspended") {
			t.Fatalf("unexpected line changed (not the suspended key): before %q, after %q", before[i], after[i])
		}
	}
	if changed == 0 {
		t.Fatalf("expected exactly one changed line (the suspended key), got none")
	}
}

// TestWriteCityAgentSuspendedForEdit_ResumeDeletesSuspendedKeyEntirely covers
// AC3's resume direction: Agent.Suspended is `toml:"suspended,omitempty"`, so
// resuming an agent that has an explicit suspended = true line must delete
// that line entirely -- not rewrite it to suspended = false -- leaving every
// other line (comments included) untouched.
func TestWriteCityAgentSuspendedForEdit_ResumeDeletesSuspendedKeyEntirely(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte(`# header comment
[workspace]
name = "acme-city"

[[agent]]
name = "mayor"
provider = "claude"
suspended = true
pre_start = ["echo starting", "echo ready"]

[[agent]]
name = "worker"
provider = "codex"
`)
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	setMayorSuspended(cfg, false)
	if err := WriteCityAgentSuspendedForEdit(fsys.OSFS{}, cityPath, cfg, "mayor", false); err != nil {
		t.Fatalf("WriteCityAgentSuspendedForEdit(resume): %v", err)
	}

	final, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	for _, line := range strippedLines(final) {
		if strings.Contains(line, "suspended") {
			t.Fatalf("resume left an explicit suspended line instead of deleting it: %q\nfull result:\n%s", line, final)
		}
	}

	var want []string
	for _, line := range strippedLines(original) {
		if strings.Contains(line, "suspended") {
			continue
		}
		want = append(want, line)
	}
	got := strippedLines(final)
	if len(want) != len(got) {
		t.Fatalf("unexpected line count after resume: want %d got %d\nwant:\n%s\ngot:\n%s", len(want), len(got), strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("line %d changed unexpectedly: want %q got %q", i, want[i], got[i])
		}
	}
}

// TestWriteCityAgentSuspendedForEdit_RefusesWhenAgentBlockNotFound covers
// AC4: when the target [[agent]] block cannot be unambiguously located, the
// write must refuse (wrapping ErrSurgicalAgentEditUnsupported) rather than
// silently doing nothing or falling back to a lossy rewrite, and city.toml
// must be left byte-for-byte unchanged.
func TestWriteCityAgentSuspendedForEdit_RefusesWhenAgentBlockNotFound(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "city.toml")
	original := []byte(`[workspace]
name = "acme-city"

[[agent]]
name = "mayor"
`)
	if err := os.WriteFile(cityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	err = WriteCityAgentSuspendedForEdit(fsys.OSFS{}, cityPath, cfg, "does-not-exist", true)
	if !errors.Is(err, ErrSurgicalAgentEditUnsupported) {
		t.Fatalf("WriteCityAgentSuspendedForEdit(unknown identity) error = %v, want wrapping ErrSurgicalAgentEditUnsupported", err)
	}

	final, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(final, original) {
		t.Fatalf("city.toml was modified despite refusing the edit:\nbefore:\n%s\nafter:\n%s", original, final)
	}
}

func setMayorSuspended(cfg *City, suspended bool) {
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "mayor" {
			cfg.Agents[i].Suspended = suspended
		}
	}
}

// commentLines extracts every trimmed comment-only line ("# ..."), in order.
func commentLines(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			out = append(out, trimmed)
		}
	}
	return out
}

// strippedLines returns every non-comment, non-blank line, in order, with
// any trailing inline comment removed.
func strippedLines(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if i := strings.Index(trimmed, "#"); i >= 0 {
			trimmed = strings.TrimSpace(trimmed[:i])
		}
		out = append(out, trimmed)
	}
	return out
}
