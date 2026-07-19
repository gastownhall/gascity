package workertest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStructuredCorpusConformance runs the WC-STRUCT-* requirements against every
// real captured transcript under testdata/corpus/. It is intentionally optional:
// when the corpus is empty it emits a visible NOTICE and skips, so a populated
// corpus lights up golden-corpus validation with no code change while an empty
// corpus never silently passes.
func TestStructuredCorpusConformance(t *testing.T) {
	captures, err := DiscoverStructuredCorpus(StructuredCorpusRoot)
	if err != nil {
		t.Fatalf("discover corpus: %v", err)
	}
	if len(captures) == 0 {
		t.Logf("NOTICE: no captured transcripts under %s/. The WC-STRUCT-* family is "+
			"validated against synthetic fixtures (TestStructuredConformance); drop sanitized "+
			"real provider captures into %s/<provider>/ to enable golden-corpus validation. "+
			"See %s/README.md for the capture procedure.",
			StructuredCorpusRoot, StructuredCorpusRoot, StructuredCorpusRoot)
		t.Skip("no structured corpus captures present")
	}

	reporter := NewSuiteReporter(t, "structured-corpus", map[string]string{"tier": "worker-core"})
	for _, capture := range captures {
		capture := capture
		t.Run(capture.Provider+"/"+filepath.Base(capture.Path), func(t *testing.T) {
			history, err := LoadCorpusHistory(capture)
			if err != nil {
				t.Fatalf("normalize %s: %v", capture.Path, err)
			}
			profile := ProfileID(capture.Provider)
			reporter.Require(t, StructuredToolResultResult(profile, history))
			reporter.Require(t, StructuredNoNativeLeakResult(profile, history))
			if edit := StructuredEditEvidenceResult(profile, history); edit.Status != ResultUnsupported {
				reporter.Require(t, edit)
			}
		})
	}
}

// TestDiscoverAndLoadStructuredCorpus exercises the corpus loader end-to-end
// against a temporary capture, so the harness mechanism is proven even while the
// committed corpus is empty.
func TestDiscoverAndLoadStructuredCorpus(t *testing.T) {
	root := t.TempDir()
	claudeDir := filepath.Join(root, "claude")
	if err := os.MkdirAll(claudeDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	capturePath := filepath.Join(claudeDir, "capture.jsonl")
	if err := os.WriteFile(capturePath, []byte(strings.Join(claudeStructuredFixtureLines(), "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write capture: %v", err)
	}

	captures, err := DiscoverStructuredCorpus(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(captures) != 1 || captures[0].Provider != "claude" {
		t.Fatalf("captures = %+v, want one claude capture", captures)
	}

	history, err := LoadCorpusHistory(captures[0])
	if err != nil {
		t.Fatalf("load corpus history: %v", err)
	}
	profile := ProfileID(captures[0].Provider)
	if r := StructuredToolResultResult(profile, history); !r.Passed() {
		t.Fatalf("corpus capture failed WC-STRUCT-001: %v", r.Err())
	}
	if r := StructuredEditEvidenceResult(profile, history); !r.Passed() {
		t.Fatalf("corpus capture failed WC-STRUCT-003: %v", r.Err())
	}

	// A missing corpus root is not an error; it simply yields no captures.
	missing, err := DiscoverStructuredCorpus(filepath.Join(root, "does-not-exist"))
	if err != nil || missing != nil {
		t.Fatalf("missing root = (%v, %v), want (nil, nil)", missing, err)
	}
}
