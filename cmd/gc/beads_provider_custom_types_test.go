package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ensureCanonicalScopeConfigState is the single funnel every managed-scope
// canonical config.yaml write routes through — both the init path
// (normalizeCanonicalBdScopeFilesForInit / seedDeferredManagedBeadsErr) and the
// post-init sweep (normalizeScopeDoltConfig). These tests prove Go now owns the
// canonical `types.custom` shaping that gc-beads-bd.sh's ensure_types_custom_in_yaml
// used to do, across every backend that routes through the funnel.

func scopeConfigPath(dir string) string {
	return filepath.Join(dir, ".beads", "config.yaml")
}

// A fresh scope (no config.yaml) must end up with every doctor.RequiredCustomTypes
// registered in types.custom, written by Go without the shell touching the file.
func TestEnsureCanonicalScopeConfigStateInjectsRequiredCustomTypes(t *testing.T) {
	dir := t.TempDir()

	if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, dir, contract.ConfigState{IssuePrefix: "gc"}); err != nil {
		t.Fatalf("ensureCanonicalScopeConfigState() error = %v", err)
	}

	data, err := os.ReadFile(scopeConfigPath(dir))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	got := string(data)
	value, ok := scanTypesCustomLine(got)
	if !ok {
		t.Fatalf("config.yaml has no types.custom line:\n%s", got)
	}
	set := make(map[string]bool)
	for _, e := range strings.Split(value, ",") {
		set[strings.TrimSpace(e)] = true
	}
	for _, req := range doctor.RequiredCustomTypes {
		if !set[req] {
			t.Errorf("config.yaml types.custom missing required type %q (got %q)", req, value)
		}
	}
}

// A scope carrying an operator/pack custom type beyond the baseline must keep it
// after Go canonicalization — the never-narrow guarantee inherited from the shell.
func TestEnsureCanonicalScopeConfigStatePreservesExistingCustomTypes(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopeConfigPath(dir), []byte("issue_prefix: gc\ntypes.custom: pack_special\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, dir, contract.ConfigState{IssuePrefix: "gc"}); err != nil {
		t.Fatalf("ensureCanonicalScopeConfigState() error = %v", err)
	}

	data, err := os.ReadFile(scopeConfigPath(dir))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	value, ok := scanTypesCustomLine(string(data))
	if !ok {
		t.Fatalf("config.yaml has no types.custom line:\n%s", data)
	}
	if !strings.Contains(value, "pack_special") {
		t.Errorf("types.custom narrowed away operator type pack_special: got %q", value)
	}
	for _, req := range doctor.RequiredCustomTypes {
		if !strings.Contains(value, req) {
			t.Errorf("types.custom missing required type %q: got %q", req, value)
		}
	}
}

func scanTypesCustomLine(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "types.custom:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "types.custom:")), true
		}
	}
	return "", false
}
