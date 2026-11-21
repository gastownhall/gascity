//go:build windows

package clientauth

import (
	"context"
	"testing"
)

// TestRunCredentialCommandStripsMixedCaseSiblingRequests pins that an
// inherited GC_*_INFO request is stripped under any spelling. Windows compares
// environment names case-insensitively, so an inherited gc_grant_info is the
// helper's $env:GC_GRANT_INFO.
func TestRunCredentialCommandStripsMixedCaseSiblingRequests(t *testing.T) {
	t.Setenv("gc_grant_info", "stale-binding")
	result, err := runCredentialCommand(context.Background(),
		`[Console]::Out.Write('{"token":"' + $env:GC_GRANT_INFO + '"}')`, ExecInfo{Version: Version})
	if err != nil {
		t.Fatal(err)
	}
	if result.Token != "" {
		t.Fatalf("a sibling request reached the credential command: %q", result.Token)
	}
}
