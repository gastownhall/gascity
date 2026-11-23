package connect

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCodesAreUniqueAndPrefixed(t *testing.T) {
	seen := make(map[Code]bool)
	for _, c := range Codes() {
		if !strings.HasPrefix(string(c), "connect_") {
			t.Errorf("code %q lacks the connect_ prefix", c)
		}
		if seen[c] {
			t.Errorf("code %q is listed twice", c)
		}
		seen[c] = true
	}
	if len(seen) != 26 {
		t.Errorf("Codes() has %d codes, want the 26 §3.8 publishes", len(seen))
	}
}

// TestCodesGolden pins the published §3.8 code strings, in Codes() order.
// A published code is never renamed or reused, so this list only grows.
func TestCodesGolden(t *testing.T) {
	want := []string{
		"connect_bundle_invalid",
		"connect_bundle_version",
		"connect_contract_unsupported",
		"connect_bundle_insecure_file",
		"connect_no_city",
		"connect_city_mismatch",
		"connect_controller_host",
		"connect_storage_mismatch",
		"connect_competing_selector",
		"connect_identity_incomplete",
		"connect_identity_mismatch",
		"connect_session_conflict",
		"connect_generation_stale",
		"connect_cohosted_session",
		"connect_workspace_unexpected",
		"connect_workspace_unproven",
		"connect_workspace_project_mismatch",
		"connect_ledger_unreachable",
		"connect_ledger_unauthorized",
		"connect_ledger_project_mismatch",
		"connect_wire_incompatible",
		"connect_journal_required",
		"connect_journal_neutralize_failed",
		"connect_bd_tooling",
		"connect_self_verify_failed",
		"connect_not_connected",
	}
	got := make([]string, len(Codes()))
	for i, c := range Codes() {
		got[i] = string(c)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Codes() = %q\nwant %q", got, want)
	}
}

func TestRefusalErrorAndCodeOf(t *testing.T) {
	cause := errors.New("cause")
	r := &Refusal{Code: CodeLedgerUnreachable, Detail: "dial failed", Err: cause}
	if got, want := r.Error(), "connect_ledger_unreachable: dial failed"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got := (&Refusal{Code: CodeNoCity}).Error(); got != "connect_no_city" {
		t.Errorf("Error() without detail = %q", got)
	}
	wrapped := fmt.Errorf("gc connect: %w", r)
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is does not reach the cause through the refusal")
	}
	if code, ok := CodeOf(wrapped); !ok || code != CodeLedgerUnreachable {
		t.Errorf("CodeOf(wrapped) = (%q, %v)", code, ok)
	}
	if _, ok := CodeOf(cause); ok {
		t.Error("CodeOf found a code on a plain error")
	}
}
