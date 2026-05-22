package logutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var expectedWalkthroughURLs = map[string]string{
	"bd_op_init_timeout":   "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#bd-op-init-timeout",
	"pack_schema_mismatch": "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#pack-schema-mismatch",
	"duplicate_name_v1v2":  "https://docs.gascityhall.com/guides/migrating-to-pack-vnext#agents",
	"duplicate_name_other": "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#duplicate-name",
	"unknown_field":        "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#unknown-field-agent-pool",
	"rig_path_required":    "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#rig-path-required",
	"template_not_found":   "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#template-not-found",
	"duplicate_identity":   "https://docs.gascityhall.com/troubleshooting/gc-start-walkthrough#duplicate-identity",
}

func TestWalkthroughURLsMatchContract(t *testing.T) {
	if got, want := len(WalkthroughURL), len(expectedWalkthroughURLs); got != want {
		t.Fatalf("WalkthroughURL has %d entries, want %d", got, want)
	}
	for key, want := range expectedWalkthroughURLs {
		if got := WalkthroughURL[key]; got != want {
			t.Fatalf("WalkthroughURL[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestWalkthroughURLStringsStayInContractFile(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	allowed := map[string]bool{
		filepath.Clean(filepath.Join("internal", "logutil", "walkthrough_urls.go")):      true,
		filepath.Clean(filepath.Join("internal", "logutil", "walkthrough_urls_test.go")): true,
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".gc", "node_modules":
				return filepath.SkipDir
			default:
				return nil
			}
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.Clean(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, url := range expectedWalkthroughURLs {
			if strings.Contains(text, url) && !allowed[rel] {
				t.Fatalf("%s hardcodes walkthrough URL %q; use WalkthroughURL from internal/logutil/walkthrough_urls.go", rel, url)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}
}
