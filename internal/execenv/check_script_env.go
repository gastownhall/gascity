package execenv

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Operator-supplied check-script environment: the name rules.
//
// The controller runs check scripts in an explicit environment (see
// internal/convergence.ConditionEnv.Environ): HOME is the city directory, PATH
// is short, and the GC_* variables describe the step. An operator can add
// variables to that environment through [check_script_env] in city.toml, for
// tools that locate their own inputs through the environment (Python derives
// its per-user site-packages from HOME, for instance). This file holds the two
// rules the table has to satisfy, in a leaf package so that both the config
// loader (which validates the table) and the convergence package (which
// applies it) can share them without importing each other:
//
//  1. A name the check environment writes itself is reserved. Operator entries
//     are appended and can never replace one of those.
//  2. A short list of names is refused outright because a child process acts
//     on them before any script logic runs, or because they move the very
//     discovery the HOME redirect exists to prevent. The bar is deliberately
//     narrow: an operator editing city.toml is trusted and can already edit the
//     script, so interpreter option variables and other tool-specific knobs are
//     the operator's business. What is refused is anything that would undo the
//     redirect for every tool at once or inject into every child.

// CheckScriptEnvWrittenKeys are the names ConditionEnv.Environ writes. Every
// one is reserved for the operator table. The convergence package pins this
// list to what Environ actually emits (TestEveryEnvironKeyIsReserved).
var CheckScriptEnvWrittenKeys = []string{
	"PATH",
	"HOME",
	"GC_HOME",
	"TMPDIR",
	"BEADS_DIR",
	"GC_BEAD_ID",
	"GC_ITERATION",
	"GC_WISP_ID",
	"GC_ITERATION_DURATION_MS",
	"GC_CUMULATIVE_DURATION_MS",
	"GC_MAX_ITERATIONS",
	"GC_DOC_PATH",
	"GC_AGENT_VERDICT",
	"GC_AGENT_PROVIDER",
	"GC_AGENT_MODEL",
	"GC_WORK_DIR",
	"GC_STORE_PATH",
	"GC_ARTIFACT_DIR",
	"GC_MOLECULE_DIR",
	"GC_INTEGRATION_REAL_BD",
	"BEADS_CREDENTIALS_FILE",
	// Passed through from the controller when set.
	"BEADS_DOLT_AUTO_START",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	"BEADS_DOLT_PASSWORD",
	"GC_DOLT",
	"GC_DOLT_HOST",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_DOLT_PASSWORD",
	// Written by citylayout.CityRuntimeEnvForRuntimeDir.
	"GC_CITY",
	"GC_CITY_PATH",
	"GC_CITY_RUNTIME_DIR",
	"GC_CONTROL_DISPATCHER_TRACE_DEFAULT",
}

// checkScriptEnvStorePrefixes are name prefixes reserved because bd and gc
// read them to choose a store, a binary, or a backend: one city-wide value
// would point every gate's bd somewhere else.
var checkScriptEnvStorePrefixes = []string{"BEADS_", "BD_"}

// checkScriptEnvHazardKeys are names a config value must not set because a
// child process acts on them before the script body runs, or because they hand
// a live credential agent or the controller's authority to the script.
var checkScriptEnvHazardKeys = map[string]string{
	// Sourced by every bash (BASH_ENV) or sh (ENV) before the script body.
	"BASH_ENV": "every bash reads it before the script body runs",
	"ENV":      "every sh reads it before the script body runs",
	// Credential discovery the HOME redirect is meant to cut.
	"GNUPGHOME":      "it moves gpg's key and configuration directory out from under the HOME redirect",
	"SSH_AUTH_SOCK":  "it hands the script a live ssh agent",
	"GPG_AGENT_INFO": "it hands the script a live gpg agent",
	// Authorizes writes to protected convergence metadata.
	"GC_CONTROLLER_TOKEN": "it is the controller's own authority",
}

// checkScriptEnvHazardPrefixes are name prefixes refused for the same reasons:
// XDG_* redirects the configuration and data directories gh, git, bd and
// others read instead of HOME, for every tool at once; LD_* and DYLD_* let a
// value inject code into every process the script starts.
var checkScriptEnvHazardPrefixes = map[string]string{
	"XDG_":  "it redirects every tool's configuration and data directories, which is what the HOME redirect exists to prevent",
	"LD_":   "the dynamic loader acts on it in every process the script starts",
	"DYLD_": "the dynamic loader acts on it in every process the script starts",
}

var checkScriptEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsReservedCheckScriptEnvKey reports whether key may not be supplied through
// [check_script_env]: it is a variable the check environment sets itself, a
// store or binary selector bd reads, or a name whose value a child process
// acts on before the script runs.
func IsReservedCheckScriptEnvKey(key string) bool {
	_, reason := checkScriptEnvReservation(key)
	return reason != ""
}

// checkScriptEnvReservation returns why key is reserved, or "" when it is not,
// and whether the reason is that the check environment writes the key itself.
func checkScriptEnvReservation(key string) (written bool, reason string) {
	for _, reserved := range CheckScriptEnvWrittenKeys {
		if key == reserved {
			return true, "the check environment sets it itself"
		}
	}
	for _, prefix := range checkScriptEnvStorePrefixes {
		if strings.HasPrefix(key, prefix) {
			return false, "bd and gc read " + prefix + "* names to select a store, a binary or a backend"
		}
	}
	if why, ok := checkScriptEnvHazardKeys[key]; ok {
		return false, why
	}
	for prefix, why := range checkScriptEnvHazardPrefixes {
		if strings.HasPrefix(key, prefix) {
			return false, why
		}
	}
	return false, ""
}

// ValidateCheckScriptEnv rejects a [check_script_env] table whose keys are not
// valid environment variable names or are reserved. It reports the first
// offending key in sorted order so the message is stable across loads.
func ValidateCheckScriptEnv(env map[string]string) error {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !checkScriptEnvNameRE.MatchString(key) {
			return fmt.Errorf("[check_script_env]: %q is not a valid environment variable name", key)
		}
		if written, reason := checkScriptEnvReservation(key); reason != "" {
			if written {
				return fmt.Errorf("[check_script_env]: %q is reserved: %s and an operator value cannot override it", key, reason)
			}
			return fmt.Errorf("[check_script_env]: %q is reserved: %s", key, reason)
		}
	}
	return nil
}

// AppendCheckScriptEnv appends the operator-supplied variables to env in
// sorted key order, skipping any reserved or malformed key. Validation at
// config load already refuses those; skipping them here as well keeps the
// written environment authoritative for a caller that built its table without
// that validation.
func AppendCheckScriptEnv(env []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return env
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		if !checkScriptEnvNameRE.MatchString(key) || IsReservedCheckScriptEnvKey(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+extra[key])
	}
	return env
}
