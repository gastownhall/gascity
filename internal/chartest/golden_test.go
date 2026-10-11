package chartest_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/chartest"
)

func TestCapture_GoldenIsDeterministicAndSectioned(t *testing.T) {
	capt := chartest.Capture{
		Exit:          0,
		Stdout:        []byte("frontend/worker\n"),
		Stderr:        []byte("route=api\n"),
		JSONExit:      0,
		JSON:          []byte(`{"rig":"BEAD-1"}` + "\n"),
		JSONStderr:    []byte("route=api\n"),
		Events:        []string{"bead.created BEAD-1"},
		StoreReadback: []string{"BEAD-1 open"},
		Counts:        []chartest.Count{{Name: "api_requests", N: 1}},
	}
	got := string(capt.Golden())
	for _, section := range []string{
		"=== exit ===\n0\n",
		"=== stdout ===\nfrontend/worker\n",
		"=== stderr ===\nroute=api\n",
		"=== json_exit ===\n0\n",
		"=== json ===\n{\"rig\":\"BEAD-1\"}\n",
		"=== json_stderr ===\nroute=api\n",
		"=== events ===\nbead.created BEAD-1\n",
		"=== store ===\nBEAD-1 open\n",
		"=== counts ===\napi_requests=1\n",
	} {
		if !strings.Contains(got, section) {
			t.Errorf("golden missing section %q in:\n%s", section, got)
		}
	}
	// Deterministic: same capture renders identically.
	if string(capt.Golden()) != got {
		t.Fatal("Golden() not deterministic")
	}
}

func TestCapture_GoldenEncodesTrailingNewlineExplicitly(t *testing.T) {
	withNL := chartest.Capture{Stdout: []byte("foo\n")}.Golden()
	noNL := chartest.Capture{Stdout: []byte("foo")}.Golden()
	if bytes.Equal(withNL, noNL) {
		t.Fatal("streams with and without a trailing newline must render distinct goldens")
	}
	if !strings.Contains(string(noNL), `\ No newline at end of section`) {
		t.Errorf("missing no-newline marker:\n%s", noNL)
	}
	if strings.Contains(string(withNL), "No newline at end of section") {
		t.Errorf("marker must be absent when the stream ends in a newline:\n%s", withNL)
	}
}

func TestCompareGolden_MatchAndMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.golden")
	content := []byte("=== exit ===\n0\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Match: no failure recorded on a fresh sub-test.
	t.Run("match", func(t *testing.T) {
		chartest.CompareGolden(t, path, content)
	})

	// Mismatch: CompareGolden must fail. Use a recording TB.
	rec := &recordingTB{TB: t}
	chartest.CompareGolden(rec, path, []byte("=== exit ===\n1\n"))
	if !rec.failed {
		t.Fatal("CompareGolden did not fail on mismatch")
	}
}

// recordingTB records whether Errorf/Fatalf fired without aborting the parent.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Errorf(string, ...any) { r.failed = true }
func (r *recordingTB) Fatalf(string, ...any) { r.failed = true }
func (r *recordingTB) Helper()               {}
