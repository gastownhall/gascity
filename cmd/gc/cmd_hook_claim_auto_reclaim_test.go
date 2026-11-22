package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// The cmd-level pin of the auto_reclaim_stale_claims wiring (#6793 gap 2):
// config.Agent.AutoReclaimStaleClaims must reach hookClaimOptions on the
// `gc hook --claim` door, or the documented hook-side re-population knob
// (#6402) is inert — the reclaim lane below doHookClaim can never run from
// the CLI. The doHookClaim-level tests build hookClaimOptions by hand, one
// layer below the missing mapping, so they pass either way; these tests go
// through cmdHookWithOptions, the production construction.

// autoReclaimWorkQuery serves exactly one candidate: work-1, in progress and
// held by another identity (worker-dead), routed to the worker pool. The
// default work query's routed tier serves only unassigned beads, so a knob
// test needs a custom query that also serves route-matched beads another
// assignee holds.
const autoReclaimWorkQuery = `sh -c 'printf "[{\"id\":\"work-1\",\"status\":\"in_progress\",\"assignee\":\"worker-dead\",\"metadata\":{\"gc.routed_to\":\"worker\"}}]"'`

// autoReclaimWorkQueryFor returns the work query serving the base
// routed-and-held candidate with candidateMetadata merged over its metadata.
// The JSON quotes are shell printf escapes (\"), matching the base constant.
func autoReclaimWorkQueryFor(candidateMetadata map[string]string) string {
	metadata := map[string]string{"gc.routed_to": "worker"}
	for k, v := range candidateMetadata {
		metadata[k] = v
	}
	parts := make([]string, 0, len(metadata))
	for k, v := range metadata {
		parts = append(parts, `\"`+k+`\":\"`+v+`\"`)
	}
	slices.Sort(parts)
	return `sh -c 'printf "[{\"id\":\"work-1\",\"status\":\"in_progress\",\"assignee\":\"worker-dead\",\"metadata\":{` +
		strings.Join(parts, `,`) + `}}]"'`
}

// writeAutoReclaimCity writes a city with one city-scoped worker agent whose
// work_query serves the AC-1 candidate. agentExtra is spliced into the
// [[agent]] block (the knob under test); patchExtra is spliced at top level
// (a [[patches.agent]] entry).
func writeAutoReclaimCity(t *testing.T, workQuery, agentExtra, patchExtra string) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
scope = "city"
work_query = '''` + workQuery + `'''
` + agentExtra + patchExtra
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

// newAutoReclaimSessionBead creates the live session bead the claim fence
// verifies: active, named origin, instance token matching the env.
func newAutoReclaimSessionBead(t *testing.T, cityDir string, metadata map[string]string) string {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "worker-1",
		Type:     session.BeadType,
		Labels:   []string{"gc:session", "agent:worker"},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return bead.ID
}

// setAutoReclaimClaimEnv is the named-session runtime env: the alias holder
// identity worker-1 running the worker template.
func setAutoReclaimClaimEnv(t *testing.T, cityDir, sessionID string) {
	t.Helper()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_ALIAS", "worker-1")
	t.Setenv("GC_AGENT", "worker-1")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv("GC_SESSION_NAME", "worker-1")
	t.Setenv("GC_SESSION_ORIGIN", "named")
	t.Setenv("GC_INSTANCE_TOKEN", "live-token")
}

// fakeAutoReclaimBD is the bd boundary for the auto-reclaim scenarios. It
// records every argv the claim runs and answers the commands this fixture
// issues: the scoped reclaim, the claim CAS, and the post-claim reads and
// identity stamps of work-1.
type fakeAutoReclaimBD struct {
	// reclaimReply is the stdout body for `reclaim --id work-1 --json`.
	reclaimReply string
	// reclaimErr, when non-empty, makes the reclaim exit non-zero with this
	// message after printing reclaimReply.
	reclaimErr string
	// claimLost, when true, makes the claim CAS lose the race (bd's
	// claim-conflict exit) instead of returning the claimed row.
	claimLost bool
	argv      []string
}

// autoReclaimClaimedRow is the canonical work-1 row after this session claims it.
const autoReclaimClaimedRow = `{"id":"work-1","title":"stale work","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}`

// install swaps the production bd runner seam for the fake, restoring it on
// cleanup.
func (f *fakeAutoReclaimBD) install(t *testing.T) {
	t.Helper()
	original := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = original })
	hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			cmd := strings.Join(args, " ")
			f.argv = append(f.argv, cmd)
			if name != "bd" {
				t.Errorf("unexpected command: %s %s", name, cmd)
				return nil, fmt.Errorf("unexpected command: %s %s", name, cmd)
			}
			switch {
			case cmd == "reclaim --id work-1 --json":
				if f.reclaimErr != "" {
					return []byte(f.reclaimReply), errors.New(f.reclaimErr)
				}
				return []byte(f.reclaimReply), nil
			case cmd == "update work-1 --claim --json":
				if f.claimLost {
					return []byte(`{"error":"issue not claimable: status in_progress","schema_version":1}`),
						errors.New("exit status 1: Error updating work-1: issue not claimable: status in_progress")
				}
				return []byte(autoReclaimClaimedRow), nil
			case cmd == "show --json work-1",
				strings.HasPrefix(cmd, "update --json work-1"):
				return []byte(autoReclaimClaimedRow), nil
			case strings.HasPrefix(cmd, "query") || strings.HasPrefix(cmd, "list") || strings.HasPrefix(cmd, "sql"):
				return []byte("[]"), nil
			default:
				t.Errorf("unexpected bd command: %s", cmd)
				return nil, fmt.Errorf("unexpected bd command: %s", cmd)
			}
		}
	}
}

// reclaimArgv returns the recorded reclaim invocations.
func (f *fakeAutoReclaimBD) reclaimArgv() []string {
	var out []string
	for _, cmd := range f.argv {
		if cmd == "reclaim --id work-1 --json" {
			out = append(out, cmd)
		}
	}
	return out
}

// claimArgv returns the recorded claim mutations.
func (f *fakeAutoReclaimBD) claimArgv() []string {
	var out []string
	for _, cmd := range f.argv {
		if cmd == "update work-1 --claim --json" {
			out = append(out, cmd)
		}
	}
	return out
}

// runAutoReclaimClaim runs one `gc hook --claim --json` and decodes its JSON
// result. The code is the hook's own exit contract (a no-work drain is exit 1
// by design), so callers assert the action they expect, not a blanket 0.
func runAutoReclaimClaim(t *testing.T) (hookClaimJSONResult, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s; stderr=%s", err, stdout.String(), stderr.String())
	}
	return result, code
}

// TestHookClaimAttemptsStaleReclaimWhenAgentOptsIn is AC-1: an agent with
// auto_reclaim_stale_claims = true whose work query serves a route-matched
// bead held by another identity gets exactly one scoped bd reclaim when bd
// reports nothing reclaimed — no claim mutation for the bead, and the
// invocation drains no_work.
func TestHookClaimAttemptsStaleReclaimWhenAgentOptsIn(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeAutoReclaimCity(t, autoReclaimWorkQuery, "auto_reclaim_stale_claims = true\n", "")
	sessionID := newAutoReclaimSessionBead(t, cityDir, map[string]string{
		"session_name":   "worker-1",
		"template":       "worker",
		"alias":          "worker-1",
		"agent_name":     "worker",
		"session_origin": "named",
		"state":          string(session.StateActive),
		"instance_token": "live-token",
	})
	setAutoReclaimClaimEnv(t, cityDir, sessionID)
	bd := &fakeAutoReclaimBD{reclaimReply: `{"reclaimed":[],"count":0}`}
	bd.install(t)

	result, code := runAutoReclaimClaim(t)
	// The hook's own contract: a no-work drain is exit 1.
	if code != 1 {
		t.Fatalf("gc hook --claim exited %d for a no-work drain, want 1", code)
	}
	if got := bd.reclaimArgv(); len(got) != 1 {
		t.Fatalf("bd reclaim ran %q, want exactly one scoped reclaim of work-1 (the knob is wired from config.Agent)", got)
	}
	if got := bd.claimArgv(); len(got) != 0 {
		t.Fatalf("claim ran %q, want none: a reclaim that reports nothing reclaimed leaves the candidate untouched", got)
	}
	if len(bd.argv) != 1 {
		t.Fatalf("bd ran %q, want only the reclaim", bd.argv)
	}
	if result.Action != "drain" || result.Reason != "no_work" {
		t.Fatalf("result = %+v, want a no_work drain", result)
	}
}

// TestHookClaimReclaimEligibilityGatesAreHonored pins the reclaim lane's
// eligibility gates at the cmd level: a knob-on session still attempts no
// reclaim for a candidate that is routed elsewhere, still inside its
// build-budget deferral, or that excludes this claimant.
func TestHookClaimReclaimEligibilityGatesAreHonored(t *testing.T) {
	for _, tc := range []struct {
		name      string
		metadata  map[string]string
		wantArgv  int
		wantDrain bool
	}{
		{
			name:      "routed to another agent",
			metadata:  map[string]string{"gc.routed_to": "other"},
			wantArgv:  0,
			wantDrain: true,
		},
		{
			name:      "budget deferred",
			metadata:  map[string]string{"gc.budget_deferred_until": "2099-01-01T00:00:00Z"},
			wantArgv:  0,
			wantDrain: true,
		},
		{
			name:      "session excluded",
			metadata:  map[string]string{"gc.exclude_claimants": "worker-1"},
			wantArgv:  0,
			wantDrain: true,
		},
		{
			name:      "reclaim errors",
			metadata:  nil,
			wantArgv:  1,
			wantDrain: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeAutoReclaimCity(t, autoReclaimWorkQueryFor(tc.metadata), "auto_reclaim_stale_claims = true\n", "")
			sessionID := newAutoReclaimSessionBead(t, cityDir, map[string]string{
				"session_name":   "worker-1",
				"template":       "worker",
				"alias":          "worker-1",
				"agent_name":     "worker",
				"session_origin": "named",
				"state":          string(session.StateActive),
				"instance_token": "live-token",
			})
			setAutoReclaimClaimEnv(t, cityDir, sessionID)
			bd := &fakeAutoReclaimBD{
				reclaimReply: `{"reclaimed":[],"count":0}`,
				reclaimErr:   "exit status 1: Error: no stale claims",
			}
			bd.install(t)

			result, code := runAutoReclaimClaim(t)
			if code != 1 {
				t.Fatalf("gc hook --claim exited %d for a no-work drain, want 1", code)
			}
			if got := len(bd.reclaimArgv()); got != tc.wantArgv {
				t.Fatalf("bd reclaim ran %d times, want %d (argv=%q)", got, tc.wantArgv, bd.argv)
			}
			if got := bd.claimArgv(); len(got) != 0 {
				t.Fatalf("claim ran %q, want none", got)
			}
			if tc.wantDrain && (result.Action != "drain" || result.Reason != "no_work") {
				t.Fatalf("result = %+v, want a no_work drain", result)
			}
		})
	}
}
