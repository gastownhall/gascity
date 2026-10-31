package execenv

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateCheckScriptEnv(t *testing.T) {
	t.Parallel()
	if err := ValidateCheckScriptEnv(nil); err != nil {
		t.Fatalf("nil table: %v", err)
	}
	if err := ValidateCheckScriptEnv(map[string]string{"PYTHONUSERBASE": "/x", "MY_TOOL_HOME": "", "lower_case": "ok"}); err != nil {
		t.Fatalf("ordinary keys: %v", err)
	}
	// GC_* names the check environment does not write stay allowed: the stock
	// artifact validator reads GC_BUILD_SCHEMA_ROOTS and GC_RIG_ROOT as hints.
	if err := ValidateCheckScriptEnv(map[string]string{"GC_BUILD_SCHEMA_ROOTS": "/schemas", "GC_RIG_ROOT": "/rig"}); err != nil {
		t.Fatalf("unreserved GC_ keys: %v", err)
	}
	// Interpreter options and explicit git settings are the operator's
	// business: the name says which tool they affect.
	if err := ValidateCheckScriptEnv(map[string]string{"PYTHONPATH": "/x", "NODE_OPTIONS": "--x", "GIT_CONFIG_GLOBAL": "/g"}); err != nil {
		t.Fatalf("tool-specific keys: %v", err)
	}

	cases := []struct {
		key        string
		wantReason string
	}{
		// Written by the check environment itself.
		{"HOME", "sets it itself"},
		{"PATH", "sets it itself"},
		{"TMPDIR", "sets it itself"},
		{"BEADS_DIR", "sets it itself"},
		{"GC_HOME", "sets it itself"},
		{"GC_BEAD_ID", "sets it itself"},
		{"GC_STORE_PATH", "sets it itself"},
		{"GC_WORK_DIR", "sets it itself"},
		{"BEADS_CREDENTIALS_FILE", "sets it itself"},
		{"GC_DOLT_PASSWORD", "sets it itself"},
		{"GC_CITY", "sets it itself"},
		{"GC_CITY_RUNTIME_DIR", "sets it itself"},
		// Store, binary and backend selectors bd reads.
		{"BEADS_BACKEND", "select a store"},
		{"BEADS_DOLT_SERVER_SOCKET", "select a store"},
		{"BD_BIN", "select a store"},
		// Acted on before the script runs, or redirects discovery.
		{"BASH_ENV", "before the script body"},
		{"ENV", "before the script body"},
		{"XDG_CONFIG_HOME", "configuration and data directories"},
		{"XDG_DATA_HOME", "configuration and data directories"},
		{"LD_PRELOAD", "dynamic loader"},
		{"LD_LIBRARY_PATH", "dynamic loader"},
		{"DYLD_INSERT_LIBRARIES", "dynamic loader"},
		{"SSH_AUTH_SOCK", "ssh agent"},
		{"GNUPGHOME", "gpg"},
		{"GPG_AGENT_INFO", "gpg agent"},
		{"GC_CONTROLLER_TOKEN", "authority"},
	}
	for _, tc := range cases {
		err := ValidateCheckScriptEnv(map[string]string{tc.key: "v"})
		if err == nil {
			t.Errorf("%s: accepted, want rejected", tc.key)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, `"`+tc.key+`"`) || !strings.Contains(msg, "check_script_env") || !strings.Contains(msg, tc.wantReason) {
			t.Errorf("%s: error %q must name the key, the table, and the reason (%q)", tc.key, msg, tc.wantReason)
		}
		if !IsReservedCheckScriptEnvKey(tc.key) {
			t.Errorf("%s: IsReservedCheckScriptEnvKey = false, want true", tc.key)
		}
	}

	for _, key := range []string{"", "1ABC", "HAS-DASH", "HAS SPACE", "A=B", "ünïcode"} {
		if err := ValidateCheckScriptEnv(map[string]string{key: "v"}); err == nil || !strings.Contains(err.Error(), "not a valid environment variable name") {
			t.Errorf("%q: error = %v, want rejected as an invalid name", key, err)
		}
	}

	// The first offending key in sorted order is reported, so the message is
	// stable across runs regardless of map iteration.
	err := ValidateCheckScriptEnv(map[string]string{"ZZZ_HOME": "ok", "PATH": "x", "HOME": "y"})
	if err == nil || !strings.Contains(err.Error(), `"HOME"`) {
		t.Fatalf("error = %v, want the sorted-first reserved key HOME", err)
	}
}

func TestAppendCheckScriptEnv(t *testing.T) {
	t.Parallel()
	base := []string{"HOME=/city", "PATH=/safe", "BEADS_DIR=/city/.beads"}
	if got := AppendCheckScriptEnv(base, nil); !slices.Equal(got, base) {
		t.Fatalf("nil table changed env: %v", got)
	}
	if got := AppendCheckScriptEnv(base, map[string]string{}); !slices.Equal(got, base) {
		t.Fatalf("empty table changed env: %v", got)
	}
	got := AppendCheckScriptEnv(base, map[string]string{
		"PYTHONUSERBASE": "/real/home/Library/Python/3.14",
		"ALPHA":          "1",
		"EMPTY":          "",
		"HOME":           "/real/home", // reserved: dropped
		"PATH":           "/evil",      // reserved: dropped
		"BEADS_BACKEND":  "sqlite",     // reserved prefix: dropped
		"BASH_ENV":       "/evil.sh",   // hazard: dropped
		"bad-name":       "x",          // malformed: dropped
	})
	want := append(slices.Clone(base), "ALPHA=1", "EMPTY=", "PYTHONUSERBASE=/real/home/Library/Python/3.14")
	if !slices.Equal(got, want) {
		t.Fatalf("AppendCheckScriptEnv = %v, want %v (sorted; reserved and malformed dropped; empty value kept as set-but-empty)", got, want)
	}
}
