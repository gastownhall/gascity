package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// collapsedIdentityWorkQuery mirrors the default work query's assigned tier: it
// walks "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS" and returns the work
// assigned to whichever identity matches. Only the "olivia" identity owns work.
const collapsedIdentityWorkQuery = `sh -c 'for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do if [ "$id" = olivia ]; then printf "[{\"id\":\"mc-olivia-1\",\"status\":\"in_progress\",\"assignee\":\"olivia\"}]"; exit 0; fi; done; printf "[]"'`

// writeCollapsedIdentityCity writes the maintainer-city olivia shape: a
// single-slot (max_active_sessions = 1) agent that is ALSO the template of an
// on-demand [[named_session]] of the same name.
func writeCollapsedIdentityCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "olivia"
scope = "city"
max_active_sessions = 1
work_query = '''` + collapsedIdentityWorkQuery + `'''

[[named_session]]
name = "olivia"
template = "olivia"
scope = "city"
mode = "on_demand"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

// newCollapsedPoolSessionBead creates the pool session bead the supervisor's
// buildDesiredState collapsed onto the canonical identity ("collapsing phantom
// pool identity ... to olivia"): its runtime name stepped aside to
// "olivia-pool" (the named session reserves "olivia"), but its persisted
// alias/agent_name are the canonical "olivia".
func newCollapsedPoolSessionBead(t *testing.T, cityDir string, alias, agentName, instanceToken string) string {
	t.Helper()
	return newOliviaSessionBead(t, cityDir, map[string]string{
		"session_name":   "olivia-pool",
		"template":       "olivia",
		"alias":          alias,
		"agent_name":     agentName,
		"state":          string(session.StateActive),
		"instance_token": instanceToken,
	})
}

func newOliviaSessionBead(t *testing.T, cityDir string, metadata map[string]string) string {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "olivia",
		Type:     session.BeadType,
		Labels:   []string{"gc:session", "agent:olivia"},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return bead.ID
}

func setCollapsedPoolClaimEnv(t *testing.T, cityDir, sessionID, instanceToken string) {
	t.Helper()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "olivia")
	// clearPoolTemplateRuntimeIdentity: a pool spawn runs with a BLANK
	// GC_ALIAS and GC_AGENT on its (stepped-aside) runtime session name.
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "olivia-pool")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv("GC_SESSION_NAME", "olivia-pool")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")
	t.Setenv("GC_INSTANCE_TOKEN", instanceToken)
}

// TestHookClaimCollapsedSingletonPoolServesNamedIdentity reproduces the olivia
// specimen: the supervisor collapses the single-slot pool session onto the
// canonical identity "olivia" and wakes it on namedWorkReady (a bead assigned to
// olivia), but the running session's env still says GC_AGENT=olivia-pool with a
// blank GC_ALIAS, so its claim looked only for olivia-pool / session-id work and
// drained no_work while olivia's assignment sat unclaimed. A session whose bead
// carries the collapsed canonical identity must claim that identity's work.
func TestHookClaimCollapsedSingletonPoolServesNamedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, alias, agentName string
	}{
		{name: "alias collapsed", alias: "olivia", agentName: "olivia"},
		{name: "alias collapsed, agent_name stepped aside", alias: "olivia", agentName: "olivia-pool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeCollapsedIdentityCity(t)
			sessionID := newCollapsedPoolSessionBead(t, cityDir, tc.alias, tc.agentName, "live-token")
			setCollapsedPoolClaimEnv(t, cityDir, sessionID, "live-token")

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("stdout is not a JSON claim result: %v\n%s", err, stdout.String())
			}
			if result.Action != "work" || result.BeadID != "mc-olivia-1" {
				t.Fatalf("result = %+v, want action=work bead=mc-olivia-1: the collapsed pool session serves olivia; stderr=%s", result, stderr.String())
			}
		})
	}
}

// TestHookClaimUncollapsedPoolSessionDoesNotAdoptNamedWork is the ga-80pen8
// control: a pool session whose bead does NOT carry the canonical identity
// (a suffixed / not-yet-collapsed member) must still not adopt the named
// holder's work.
func TestHookClaimUncollapsedPoolSessionDoesNotAdoptNamedWork(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeCollapsedIdentityCity(t)
	sessionID := newCollapsedPoolSessionBead(t, cityDir, "", "olivia-pool", "live-token")
	setCollapsedPoolClaimEnv(t, cityDir, sessionID, "live-token")

	var stdout, stderr bytes.Buffer
	_ = cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s; stderr=%s", err, stdout.String(), stderr.String())
	}
	if result.Action != "drain" || result.Reason != "no_work" {
		t.Fatalf("result = %+v, want a no_work drain: an uncollapsed pool session must not claim olivia's work", result)
	}
}

// runCollapsedIdentityClaim runs one `gc hook --claim --json` and decodes it.
func runCollapsedIdentityClaim(t *testing.T) hookClaimJSONResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	_ = cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s; stderr=%s", err, stdout.String(), stderr.String())
	}
	return result
}

// TestHookClaimAgentNameAloneDoesNotCollapseIdentity: agent_name is NOT an
// exclusive identity — every session of the template may carry it — so a pool
// session whose alias is deferred (the on-demand named olivia holds or may
// claim the alias) must not adopt olivia's in_progress work on agent_name
// alone. Only the alias, written under the city alias lock, is exclusive.
func TestHookClaimAgentNameAloneDoesNotCollapseIdentity(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeCollapsedIdentityCity(t)
	sessionID := newCollapsedPoolSessionBead(t, cityDir, "", "olivia", "live-token")
	setCollapsedPoolClaimEnv(t, cityDir, sessionID, "live-token")

	if result := runCollapsedIdentityClaim(t); result.Action != "drain" || result.Reason != "no_work" {
		t.Fatalf("result = %+v, want a no_work drain: agent_name=olivia without the olivia alias must not claim olivia's work", result)
	}
}

// TestHookClaimManualSessionDoesNotCollapseIdentity: `gc session new olivia
// --alias scratch` stamps agent_name=olivia on a manual session; a manual
// session never serves the canonical identity, even if its bead carries it.
func TestHookClaimManualSessionDoesNotCollapseIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, alias string
	}{
		{name: "own alias", alias: "scratch"},
		{name: "canonical alias", alias: "olivia"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeCollapsedIdentityCity(t)
			sessionID := newOliviaSessionBead(t, cityDir, map[string]string{
				"session_name":   "s-scratch",
				"template":       "olivia",
				"alias":          tc.alias,
				"agent_name":     "olivia",
				"session_origin": "manual",
				"manual_session": "true",
				"state":          string(session.StateActive),
				"instance_token": "live-token",
			})
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_TEMPLATE", "olivia")
			t.Setenv("GC_ALIAS", "scratch")
			t.Setenv("GC_AGENT", "s-scratch")
			t.Setenv("GC_SESSION_ID", sessionID)
			t.Setenv("GC_SESSION_NAME", "s-scratch")
			t.Setenv("GC_SESSION_ORIGIN", "manual")
			t.Setenv("GC_INSTANCE_TOKEN", "live-token")

			if result := runCollapsedIdentityClaim(t); result.Action == "work" {
				t.Fatalf("result = %+v, want no claim: a manual session must not serve the canonical olivia identity", result)
			}
		})
	}
}

// TestHookClaimNamedHolderAndDeferredPoolSessionOnlyAliasHolderClaims: the
// woken on-demand named olivia holds the alias while the single-slot pool
// session's alias is deferred (agent_name=olivia). Both used to pass the
// identity check and could take the same in_progress bead; only the alias
// holder may claim it.
func TestHookClaimNamedHolderAndDeferredPoolSessionOnlyAliasHolderClaims(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeCollapsedIdentityCity(t)
	namedID := newOliviaSessionBead(t, cityDir, map[string]string{
		"session_name":                       "olivia",
		"template":                           "olivia",
		"alias":                              "olivia",
		"agent_name":                         "olivia",
		session.NamedSessionMetadataKey:      "true",
		session.NamedSessionIdentityMetadata: "olivia",
		session.NamedSessionModeMetadata:     "on_demand",
		"session_origin":                     "named",
		"state":                              string(session.StateActive),
		"instance_token":                     "named-token",
	})
	poolID := newCollapsedPoolSessionBead(t, cityDir, "", "olivia", "pool-token")

	// Pool session: must not take the named holder's bead.
	setCollapsedPoolClaimEnv(t, cityDir, poolID, "pool-token")
	if result := runCollapsedIdentityClaim(t); result.Action != "drain" || result.Reason != "no_work" {
		t.Fatalf("pool session result = %+v, want a no_work drain: the alias-deferred pool session must not claim olivia's work", result)
	}

	// Named holder: owns the alias, claims the bead.
	t.Setenv("GC_ALIAS", "olivia")
	t.Setenv("GC_AGENT", "olivia")
	t.Setenv("GC_SESSION_ID", namedID)
	t.Setenv("GC_SESSION_NAME", "olivia")
	t.Setenv("GC_SESSION_ORIGIN", "named")
	t.Setenv("GC_INSTANCE_TOKEN", "named-token")
	if result := runCollapsedIdentityClaim(t); result.Action != "work" || result.BeadID != "mc-olivia-1" {
		t.Fatalf("named holder result = %+v, want action=work bead=mc-olivia-1", result)
	}
}
