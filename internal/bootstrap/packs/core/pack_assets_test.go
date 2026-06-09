package core

import (
	"io/fs"
	"testing"
)

func TestCoreMaintenanceExecAssets(t *testing.T) {
	required := []string{
		"assets/scripts/_bd_trace.sh",
		"assets/scripts/dolt-target.sh",
		"assets/scripts/escalate.sh",
		"assets/scripts/jsonl-export.sh",
		"assets/scripts/reaper.sh",
		"orders/jsonl-export.toml",
		"orders/reaper.toml",
	}
	for _, path := range required {
		if _, err := fs.Stat(PackFS, path); err != nil {
			t.Fatalf("core pack missing %s: %v", path, err)
		}
	}

	retired := []string{
		"formulas/mol-dog-jsonl.toml",
		"formulas/mol-dog-reaper.toml",
		"orders/mol-dog-jsonl.toml",
		"orders/mol-dog-reaper.toml",
	}
	for _, path := range retired {
		if _, err := fs.Stat(PackFS, path); err == nil {
			t.Fatalf("core pack must not carry retired Dog maintenance asset %s", path)
		}
	}
}
