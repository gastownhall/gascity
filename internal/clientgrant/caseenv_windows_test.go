//go:build windows

package clientgrant

import (
	"context"
	"testing"
)

// TestRunGrantCommandStripsMixedCaseSiblingRequests pins that an inherited
// GC_*_INFO request is stripped under any spelling. Windows compares
// environment names case-insensitively, so an inherited gc_exec_info is the
// helper's $env:GC_EXEC_INFO.
func TestRunGrantCommandStripsMixedCaseSiblingRequests(t *testing.T) {
	t.Setenv("gc_exec_info", "stale-binding")
	result, err := runGrantCommand(context.Background(), `[Console]::Out.Write($env:GC_EXEC_INFO)`, GrantInfo{Version: Version})
	if err != nil {
		t.Fatal(err)
	}
	if result != "" {
		t.Fatalf("a sibling request reached the grant command: %q", result)
	}
}
