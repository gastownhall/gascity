package workertest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	worker "github.com/gastownhall/gascity/internal/worker"
)

// StructuredCorpusRoot is the directory holding real, captured provider
// transcripts used as golden inputs for the WC-STRUCT-* conformance family.
//
// Layout: testdata/corpus/<provider>/<name>.jsonl, where <provider> is the
// worker provider family (claude, codex, gemini, ...). It is empty by default;
// drop sanitized real captures in to enable golden-corpus validation without
// any code change. See testdata/corpus/README.md for the capture procedure.
const StructuredCorpusRoot = "testdata/corpus"

// CorpusCapture identifies one captured transcript under the corpus root.
type CorpusCapture struct {
	Provider string
	Path     string
}

// DiscoverStructuredCorpus returns every *.jsonl capture under root, grouped by
// its provider directory name and sorted by path. A missing root yields no
// captures and no error, so the corpus stays optional until populated.
func DiscoverStructuredCorpus(root string) ([]CorpusCapture, error) {
	providerDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var captures []CorpusCapture
	for _, providerDir := range providerDirs {
		if !providerDir.IsDir() {
			continue
		}
		provider := providerDir.Name()
		files, err := os.ReadDir(filepath.Join(root, provider))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}
			captures = append(captures, CorpusCapture{
				Provider: provider,
				Path:     filepath.Join(root, provider, file.Name()),
			})
		}
	}
	sort.Slice(captures, func(i, j int) bool { return captures[i].Path < captures[j].Path })
	return captures, nil
}

// LoadCorpusHistory normalizes a captured transcript into worker history through
// the real provider adapter, so corpus captures exercise the same path as live
// sessions.
func LoadCorpusHistory(capture CorpusCapture) (*worker.HistorySnapshot, error) {
	return (worker.SessionLogAdapter{}).LoadHistory(worker.LoadRequest{
		Provider:       capture.Provider,
		TranscriptPath: capture.Path,
		GCSessionID:    "corpus-" + capture.Provider,
	})
}
