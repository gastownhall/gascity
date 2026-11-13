package overlay

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // git blob ids are SHA-1; this only pins fixture bytes.
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The embedded released bodies are matched byte for byte, so each must be
// exactly the blob a gc release shipped at
// internal/bootstrap/packs/core/overlay/per-provider/kiro/AGENTS.md.
func TestReleasedKiroAgentsMDBodiesAreTheReleasedBlobs(t *testing.T) {
	want := map[string]string{
		"v1.2.0": "75afa650b7f899c9a0f34f977a0e9708f44da88c",
		"v1.3.0": "4b0148068c5eec5683538cdb7c44af73a61d1b58",
		"v1.4.0": "645346eeb1a4ee25bc59285f3f95cfa375a01023",
	}
	got := ReleasedKiroAgentsMDBodies()
	if len(got) != len(want) {
		t.Fatalf("released bodies = %d (%v), want %d", len(got), keys(got), len(want))
	}
	for version, blob := range want {
		body, ok := got[version]
		if !ok {
			t.Fatalf("missing released body %s; have %v", version, keys(got))
		}
		if id := gitBlobID(body); id != blob {
			t.Errorf("released body %s has git blob id %s, want %s", version, id, blob)
		}
		if !IsReleasedKiroAgentsMD(body) {
			t.Errorf("IsReleasedKiroAgentsMD(%s) = false, want true", version)
		}
		if !strings.Contains(string(body), "run `gc hook` to\ncheck for and claim new work") {
			t.Errorf("released body %s lacks the wrong work-pickup text this upgrade exists to remove", version)
		}
	}
}

func TestIsReleasedKiroAgentsMDRejectsNearMatches(t *testing.T) {
	for version, body := range ReleasedKiroAgentsMDBodies() {
		for name, variant := range nearReleasedVariants(body) {
			if IsReleasedKiroAgentsMD(variant) {
				t.Errorf("IsReleasedKiroAgentsMD(%s %s) = true, want false", version, name)
			}
		}
	}
	if IsReleasedKiroAgentsMD(nil) || IsReleasedKiroAgentsMD([]byte("project instructions\n")) {
		t.Fatal("IsReleasedKiroAgentsMD matched content that is not a released body")
	}
}

func TestCopyDirForProviders_KiroReplacesReleasedFallback(t *testing.T) {
	for version, body := range ReleasedKiroAgentsMDBodies() {
		t.Run(version, func(t *testing.T) {
			src := t.TempDir()
			dst := t.TempDir()
			current := []byte("current fallback: run gc hook --claim\n")
			mustMkdirAll(t, filepath.Join(src, "per-provider", "kiro"))
			mustWriteFile(t, filepath.Join(src, "per-provider", "kiro", "AGENTS.md"), current, 0o644)
			mustWriteFile(t, filepath.Join(dst, "AGENTS.md"), body, 0o644)

			var stderr bytes.Buffer
			if err := CopyDirForProviders(src, dst, []string{"kiro"}, &stderr); err != nil {
				t.Fatalf("CopyDirForProviders: %v", err)
			}
			if got := readFile(t, filepath.Join(dst, "AGENTS.md")); !bytes.Equal(got, current) {
				t.Fatalf("AGENTS.md = %q, want the released %s body replaced by the current fallback", got, version)
			}
			if strings.Contains(stderr.String(), "preserving existing") {
				t.Fatalf("stderr = %q, want no preservation warning for a replaced released body", stderr.String())
			}
			assertOnlyFiles(t, dst, "AGENTS.md")

			// The next stage keeps the now-current file.
			if err := CopyDirForProviders(src, dst, []string{"kiro"}, io.Discard); err != nil {
				t.Fatalf("second CopyDirForProviders: %v", err)
			}
			if got := readFile(t, filepath.Join(dst, "AGENTS.md")); !bytes.Equal(got, current) {
				t.Fatalf("AGENTS.md after second stage = %q, want current fallback", got)
			}
			assertOnlyFiles(t, dst, "AGENTS.md")
		})
	}
}

func TestCopyDirForProviders_KiroKeepsEditedReleasedFallback(t *testing.T) {
	for version, body := range ReleasedKiroAgentsMDBodies() {
		for name, variant := range nearReleasedVariants(body) {
			t.Run(version+"/"+name, func(t *testing.T) {
				src := t.TempDir()
				dst := t.TempDir()
				mustMkdirAll(t, filepath.Join(src, "per-provider", "kiro"))
				mustWriteFile(t, filepath.Join(src, "per-provider", "kiro", "AGENTS.md"), []byte("current fallback\n"), 0o644)
				mustWriteFile(t, filepath.Join(dst, "AGENTS.md"), variant, 0o644)

				var stderr bytes.Buffer
				if err := CopyDirForProviders(src, dst, []string{"kiro"}, &stderr); err != nil {
					t.Fatalf("CopyDirForProviders: %v", err)
				}
				if got := readFile(t, filepath.Join(dst, "AGENTS.md")); !bytes.Equal(got, variant) {
					t.Fatalf("AGENTS.md = %q, want the %s body kept", got, name)
				}
				if !strings.Contains(stderr.String(), "preserving existing") {
					t.Fatalf("stderr = %q, want preservation warning", stderr.String())
				}
			})
		}
	}
}

// The released-body rule belongs to Kiro's AGENTS.md alone: a released body
// sitting at any other Kiro overlay path is staged over exactly as before.
func TestCopyDirForProviders_KiroReleasedBodyRuleIsAgentsMDOnly(t *testing.T) {
	body := ReleasedKiroAgentsMDBodies()["v1.4.0"]
	src := t.TempDir()
	dst := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "per-provider", "kiro", ".kiro", "agents"))
	mustWriteFile(t, filepath.Join(src, "per-provider", "kiro", "AGENTS.md"), []byte("current fallback\n"), 0o644)
	mustWriteFile(t, filepath.Join(src, "per-provider", "kiro", ".kiro", "agents", "gascity.json"), []byte(`{"name":"gascity"}`), 0o644)
	// A released body sitting at another Kiro overlay path is staged over as
	// before: the exception applies to AGENTS.md alone.
	mustMkdirAll(t, filepath.Join(dst, ".kiro", "agents"))
	mustWriteFile(t, filepath.Join(dst, ".kiro", "agents", "gascity.json"), body, 0o644)

	if err := CopyDirForProviders(src, dst, []string{"kiro"}, io.Discard); err != nil {
		t.Fatalf("CopyDirForProviders: %v", err)
	}
	if got := string(readFile(t, filepath.Join(dst, ".kiro", "agents", "gascity.json"))); got != `{"name":"gascity"}` {
		t.Fatalf("gascity.json = %q, want overlay content", got)
	}
}

// An AGENTS.md staging cannot read is kept even when it holds a released body:
// it cannot be compared, so it is preserved as before. The file is write-only
// (stat succeeds, read fails, a write would succeed), so an overwrite would be
// observable.
func TestCopyDirForProviders_KiroKeepsUnreadableAgentsMD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relies on unix permission bits")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses unix permission checks; cannot simulate stat-ok/read-fail")
	}
	body := ReleasedKiroAgentsMDBodies()["v1.4.0"]
	src := t.TempDir()
	dst := t.TempDir()
	mustMkdirAll(t, filepath.Join(src, "per-provider", "kiro"))
	mustWriteFile(t, filepath.Join(src, "per-provider", "kiro", "AGENTS.md"), []byte("current fallback\n"), 0o644)
	target := filepath.Join(dst, "AGENTS.md")
	mustWriteFile(t, target, body, 0o644)
	if err := os.Chmod(target, 0o200); err != nil {
		t.Fatalf("chmod %s: %v", target, err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o644) })

	var stderr bytes.Buffer
	if err := CopyDirForProviders(src, dst, []string{"kiro"}, &stderr); err != nil {
		t.Fatalf("CopyDirForProviders: %v", err)
	}
	if !strings.Contains(stderr.String(), PreserveExistingWarningPrefix) {
		t.Fatalf("stderr = %q, want preservation warning", stderr.String())
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod %s: %v", target, err)
	}
	if got := readFile(t, target); !bytes.Equal(got, body) {
		t.Fatalf("unreadable AGENTS.md was overwritten with %q", got)
	}
	assertOnlyFiles(t, dst, "AGENTS.md")
}

func nearReleasedVariants(body []byte) map[string][]byte {
	return map[string][]byte{
		"no final newline":  bytes.TrimSuffix(body, []byte("\n")),
		"extra newline":     append(append([]byte(nil), body...), '\n'),
		"crlf line endings": bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n")),
		"edited":            bytes.Replace(body, []byte("You are an agent"), []byte("You are a reviewer agent"), 1),
		"appended note":     append(append([]byte(nil), body...), []byte("\n## Project notes\n\nUse tabs.\n")...),
	}
}

func gitBlobID(data []byte) string {
	h := sha1.New() //nolint:gosec // git blob ids are SHA-1.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertOnlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s holds %v, want exactly %v (no backup files)", dir, got, want)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
