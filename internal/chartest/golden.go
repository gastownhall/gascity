package chartest

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden rewrites golden files instead of comparing. Distinct flag name
// so it never collides with other packages' -update flags in a shared test
// binary (cmd/gc imports this package).
var updateGolden = flag.Bool("chartest-update", false, "rewrite chartest golden files")

// Capture is the full observable surface of one command invocation on one lane,
// already canonicalized and deterministically ordered by the harness. It
// serializes to a single golden file so a lane's whole behavior is frozen in
// one place. The ENTIRE rendered golden — human text (Stdout/Stderr) and the
// JSON run alike — is currently compared byte-exact by CompareGolden. The
// shape+additive JSON differ (JSONShapeDiff / CanonicalizeStreams) is tested
// scaffolding that is NOT yet wired into the comparison path; wire it before
// characterizing a multi-element `--json` surface whose element order is
// non-deterministic, or byte-exact comparison will flake on that order.
type Capture struct {
	Exit          int
	Stdout        []byte
	Stderr        []byte
	JSONExit      int      // exit code of the --json run (distinct invocation)
	JSON          []byte   // stdout of the --json run
	JSONStderr    []byte   // stderr of the --json run (route line, warnings, errors)
	Events        []string // canonicalized, sorted by the harness
	StoreReadback []string // canonicalized, sorted by the harness
	Counts        []Count  // boundary counts the harness actually measured, in a fixed order
}

// Count is one named boundary measurement (e.g. api_requests=1). Only counts the
// harness genuinely instruments are recorded, so a golden never asserts an
// unmeasured invariant as zero.
type Count struct {
	Name string
	N    int
}

// Golden renders the capture to its deterministic sectioned byte form. The
// human run (Exit/Stdout/Stderr) and the --json run (JSONExit/JSON/JSONStderr)
// are both frozen in full, so a refactor that changes only the --json path's
// exit, route line, or stderr is still caught.
func (c Capture) Golden() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "=== exit ===\n%d\n", c.Exit)
	writeStreamSection(&b, "stdout", c.Stdout)
	writeStreamSection(&b, "stderr", c.Stderr)
	fmt.Fprintf(&b, "=== json_exit ===\n%d\n", c.JSONExit)
	writeStreamSection(&b, "json", c.JSON)
	writeStreamSection(&b, "json_stderr", c.JSONStderr)
	fmt.Fprintf(&b, "=== events ===\n")
	for _, e := range c.Events {
		fmt.Fprintf(&b, "%s\n", e)
	}
	fmt.Fprintf(&b, "=== store ===\n")
	for _, s := range c.StoreReadback {
		fmt.Fprintf(&b, "%s\n", s)
	}
	fmt.Fprintf(&b, "=== counts ===\n")
	for _, ct := range c.Counts {
		fmt.Fprintf(&b, "%s=%d\n", ct.Name, ct.N)
	}
	return b.Bytes()
}

// writeStreamSection emits a byte stream verbatim under its header, encoding the
// trailing-newline boundary EXPLICITLY (git-diff style) rather than normalizing
// it away — presence/absence of a final newline is observable CLI behavior the
// harness must freeze.
func writeStreamSection(b *bytes.Buffer, name string, data []byte) {
	fmt.Fprintf(b, "=== %s ===\n", name)
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteString("\n\\ No newline at end of section\n")
	}
}

// CompareGolden compares got against the golden at path, or rewrites it when
// -chartest-update is set. On mismatch it fails t with a readable diff header.
func CompareGolden(t testing.TB, path string, got []byte) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("chartest: mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("chartest: write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("chartest: read golden %s: %v (run with -chartest-update to create it)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("chartest: golden mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}
