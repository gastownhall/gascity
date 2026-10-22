package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// rnfCity is city B: rig "beta" is guarded (federation = "rig") and runs the
// pool "beta/worker"; rig "alpha" is not guarded. Both share nothing here;
// the guard decides from config and this city's own session records alone.
func rnfCity() *config.City {
	beta := rnsGuardedRig("beta")
	beta.Path = "beta"
	beta.Prefix = "bt"
	return &config.City{
		Workspace: config.Workspace{Name: "city-b", Prefix: "gcx"},
		Rigs:      []config.Rig{beta, {Name: "alpha", Path: "alpha", Prefix: "al"}},
		Agents: []config.Agent{
			{Name: "worker", Dir: "beta"},
			{Name: "worker", Dir: "alpha"},
		},
	}
}

// rnfProvenance returns a provenance func that records whether it ran.
func rnfProvenance(verdict claimProvenance, called *bool) func() claimProvenance {
	return func() claimProvenance {
		*called = true
		return verdict
	}
}

// rnfPanicResolver fails the test if the namespace asks it anything: a
// refusal that needs no record must not read the session store.
type rnfPanicResolver struct{ t *testing.T }

func (r rnfPanicResolver) lookupOwnSession(v string) (ownSessionRecord, bool, error) {
	r.t.Fatalf("session lookup for %q; the namespace must decide this without a store read", v)
	return ownSessionRecord{}, false, nil
}

func (r rnfPanicResolver) lookupOwnSessionsByName(v string, _ func(ownSessionRecord) bool) ([]ownSessionRecord, error) {
	r.t.Fatalf("session name lookup for %q; the namespace must decide this without a store read", v)
	return nil, nil
}

// TestRigNamespaceClaimProvenance_Verdicts asserts local and foreign answers
// in one table, so neither half is satisfiable by a gate that answers the
// same way for everything.
func TestRigNamespaceClaimProvenance_Verdicts(t *testing.T) {
	cfg := rnfCity()
	roster := claimProvenance{Verdict: claimVerdictLocal, Reason: claimReasonRoster}
	absent := claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonAbsentFromRoster}
	ownSessions, _ := rnsCityBResolver(t)

	for _, tc := range []struct {
		name         string
		rig          string
		identity     string
		sessions     ownSessionResolver
		fallback     claimProvenance
		want         claimProvenance
		wantChecked  bool
		wantVia      admitVia
		wantFallback bool
	}{
		{
			name: "lexical, roster vouches", rig: "beta", identity: "beta/worker-2", sessions: ownSessions,
			fallback: roster, want: roster, wantChecked: true, wantVia: admitLexical, wantFallback: true,
		},
		{
			name: "lexical, nothing vouches", rig: "beta", identity: "beta/stranger", sessions: ownSessions,
			fallback: absent, want: absent, wantChecked: true, wantVia: admitLexical, wantFallback: true,
		},
		{
			name: "own pool session id", rig: "beta", identity: poolIDB, sessions: ownSessions,
			fallback: absent, want: claimProvenance{Verdict: claimVerdictLocal, Reason: claimReasonSessionRecord, Detail: poolIDB},
			wantChecked: true, wantVia: admitSessionRecord,
		},
		{
			name: "own pool session name", rig: "beta", identity: poolNameB, sessions: ownSessions,
			fallback: absent, want: claimProvenance{Verdict: claimVerdictLocal, Reason: claimReasonSessionRecord, Detail: poolIDB},
			wantChecked: true, wantVia: admitSessionRecord,
		},
		{
			name: "another city's pool session id", rig: "beta", identity: poolIDA, sessions: ownSessions,
			fallback: roster, want: claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonNamespace, Detail: "outside beta/"},
			wantChecked: true,
		},
		{
			name: "bare name", rig: "beta", identity: "neighbor", sessions: rnfPanicResolver{t},
			fallback: roster, want: claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonNamespace, Detail: "outside beta/"},
			wantChecked: true,
		},
		{
			name: "another rig's identity", rig: "beta", identity: "alpha/worker", sessions: rnfPanicResolver{t},
			fallback: roster, want: claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonNamespace, Detail: "outside beta/"},
			wantChecked: true,
		},
		{
			name: "case differs from the namespace", rig: "beta", identity: "Beta/worker", sessions: rnfPanicResolver{t},
			fallback: roster, want: claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonNamespace, Detail: "outside beta/"},
			wantChecked: true,
		},
		{
			name: "short store-minted session id", rig: "beta", identity: shortIDB, sessions: rnfPanicResolver{t},
			fallback: roster, want: claimProvenance{Verdict: claimVerdictForeign, Reason: claimReasonNamespace, Detail: "outside beta/"},
			wantChecked: true,
		},
		{
			name: "unguarded rig keeps today's verdict", rig: "alpha", identity: "neighbor", sessions: rnfPanicResolver{t},
			fallback: absent, want: absent, wantFallback: true,
		},
		{
			name: "unguarded rig, roster", rig: "alpha", identity: "alpha/worker", sessions: rnfPanicResolver{t},
			fallback: roster, want: roster, wantFallback: true,
		},
		{
			name: "unknown rig", rig: "gamma", identity: "gamma/worker", sessions: rnfPanicResolver{t},
			fallback: roster, want: claimProvenance{Verdict: claimVerdictUnknown, Reason: claimReasonNoConfig, Detail: `unknown rig "gamma"`},
		},
		{
			// A rig name differing only in case is another rig: never the
			// guarded one, and never "unguarded".
			name: "rig name in another case", rig: "Beta", identity: "neighbor", sessions: rnfPanicResolver{t},
			fallback: roster, want: claimProvenance{Verdict: claimVerdictUnknown, Reason: claimReasonNoConfig, Detail: `unknown rig "Beta"`},
		},
		{
			name: "empty rig name", rig: "", identity: "neighbor", sessions: rnfPanicResolver{t},
			fallback: roster, want: claimProvenance{Verdict: claimVerdictUnknown, Reason: claimReasonNoConfig, Detail: `unknown rig ""`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			got := rigNamespaceClaimProvenance(cfg, tc.rig, tc.identity, tc.sessions, rnfProvenance(tc.fallback, &called))
			if got.claimProvenance != tc.want {
				t.Fatalf("verdict = %+v, want %+v", got.claimProvenance, tc.want)
			}
			if got.Checked != tc.wantChecked || got.Via != tc.wantVia {
				t.Fatalf("checked/via = %v/%s, want %v/%s", got.Checked, got.Via, tc.wantChecked, tc.wantVia)
			}
			if called != tc.wantFallback {
				t.Fatalf("today's provenance ran = %v, want %v", called, tc.wantFallback)
			}
		})
	}
}

// TestRigNamespaceClaimProvenance_LookupErrorIsNeverLocal: a session store
// that cannot be read leaves a pool identity unadmitted and the verb unable
// to answer, so the claim is protected. It must not fall through to today's
// provenance, which could answer local from the roster or a record.
func TestRigNamespaceClaimProvenance_LookupErrorIsNeverLocal(t *testing.T) {
	cfg := rnfCity()
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	store.setErr(errors.New("session store unreachable"))
	sessions := newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store))
	for _, identity := range []string{poolIDB, poolNameB} {
		called := false
		got := rigNamespaceClaimProvenance(cfg, "beta", identity, sessions, rnfProvenance(claimProvenance{Verdict: claimVerdictLocal, Reason: claimReasonRoster}, &called))
		if got.Verdict != claimVerdictUnknown || got.Reason != claimReasonLookupFailed || !got.Checked {
			t.Fatalf("%s: verdict = %+v (checked %v), want unknown/lookup_failed, checked", identity, got.claimProvenance, got.Checked)
		}
		if !strings.Contains(got.Detail, "session store unreachable") {
			t.Fatalf("%s: detail %q does not carry the lookup error", identity, got.Detail)
		}
		if called {
			t.Fatalf("%s: today's provenance ran after a failed lookup", identity)
		}
	}
	// A fake resolver that reports a record together with an error is still
	// an error: the namespace never admits on a failed lookup.
	fake := rnsFakeResolver{rec: ownSessionRecord{ID: poolIDB, Template: templateB}, ok: true, err: errors.New("partial read")}
	got := rigNamespaceClaimProvenance(cfg, "beta", poolIDB, fake, func() claimProvenance {
		t.Fatal("today's provenance ran after a failed lookup")
		return claimProvenance{}
	})
	if got.Verdict == claimVerdictLocal {
		t.Fatalf("a lookup error answered local: %+v", got)
	}
}

// --- the verb, end to end, over a loaded city ---

const rnfToken = "0123456789abcdef0123456789abcdef"

// rnfVerbCity writes city B to disk: guarded rig "beta" with pool
// "beta/worker", unguarded rig "alpha", on the file bead store.
func rnfVerbCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	cityPath := t.TempDir()
	for _, dir := range []string{"beta", "alpha", ".gc"} {
		if err := os.MkdirAll(filepath.Join(cityPath, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	cityToml := `[workspace]
name = "city-b"
prefix = "tc"

[[rigs]]
name = "beta"
path = "beta"
prefix = "bt"
federation = "rig"
namespace = "exclusive"

[[rigs]]
name = "alpha"
path = "alpha"
prefix = "al"

[[agent]]
name = "worker"
dir = "beta"
min_active_sessions = 0
max_active_sessions = 2

[[agent]]
name = "worker"
dir = "alpha"
min_active_sessions = 0
max_active_sessions = 2
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	return cityPath
}

func rigNamespaceArgs(cityPath, rig string, identities ...string) []string {
	return append([]string{"--city", cityPath, "agent", "is-foreign", "--rig-namespace", rig, "--json", "--"}, identities...)
}

// TestAgentIsForeign_RigNamespaceVerdicts runs the verb over a loaded city:
// lexical local, own pool id local, another city's pool id, a bare name
// (namespace), an unknown rig, and an unguarded rig.
func TestAgentIsForeign_RigNamespaceVerdicts(t *testing.T) {
	cityPath := rnfVerbCity(t)
	ownPoolID := "tc-session-" + rnfToken
	// This process stands in for one of the city's own pool sessions of
	// beta/worker: the CLI resolver's fast path reads it from the
	// environment, as a pool session running the verb would.
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_SESSION_ID", ownPoolID)
	t.Setenv("GC_SESSION_NAME", "worker-"+ownPoolID)
	t.Setenv("GC_TEMPLATE", "beta/worker")

	for _, tc := range []struct {
		name        string
		rig         string
		identity    string
		wantCode    int
		wantVerdict string
		wantReason  claimProvenanceReason
		wantDetail  string
		wantChecked bool
		wantVia     string
	}{
		{"lexical, roster", "beta", "beta/worker-2", agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonRoster, "", true, "lexical"},
		{"lexical, absent from the roster", "beta", "beta/stranger", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonAbsentFromRoster, "", true, "lexical"},
		{"own pool session id", "beta", ownPoolID, agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonSessionRecord, ownPoolID, true, "session_record"},
		{"another city's pool session id", "beta", "tc-session-fedcba9876543210fedcba9876543210", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonNamespace, "outside beta/", true, ""},
		{"bare name", "beta", "neighbor", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonNamespace, "outside beta/", true, ""},
		{"a person", "beta", "Pat Example", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonNamespace, "outside beta/", true, ""},
		{"same agent on the other rig", "beta", "alpha/worker", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonNamespace, "outside beta/", true, ""},
		{"unguarded rig, bare name", "alpha", "neighbor", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonAbsentFromRoster, "", false, ""},
		{"unguarded rig, roster", "alpha", "alpha/worker", agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonRoster, "", false, ""},
		{"unknown rig", "gamma", "gamma/worker", agentIsForeignExitUnknown, agentIsForeignVerdictUnknown, claimReasonNoConfig, `unknown rig "gamma"`, false, ""},
		{"rig name in another case", "Beta", "neighbor", agentIsForeignExitUnknown, agentIsForeignVerdictUnknown, claimReasonNoConfig, `unknown rig "Beta"`, false, ""},
		// The flag decides the mode, not its value: an empty rig is still a
		// rig question, and no rig answers it.
		{"empty rig name", "", "beta/worker", agentIsForeignExitUnknown, agentIsForeignVerdictUnknown, claimReasonNoConfig, `unknown rig ""`, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, raw, stderr := runIsForeign(t, rigNamespaceArgs(cityPath, tc.rig, tc.identity)...)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.wantCode, raw, stderr)
			}
			if out.Verdict != tc.wantVerdict || out.Reason != string(tc.wantReason) || out.Detail != tc.wantDetail {
				t.Fatalf("verdict/reason/detail = %q/%q/%q, want %q/%q/%q (raw: %s)", out.Verdict, out.Reason, out.Detail, tc.wantVerdict, tc.wantReason, tc.wantDetail, raw)
			}
			if out.RigNamespace != tc.rig {
				t.Fatalf("rig_namespace = %q, want the flag echoed as %q (raw: %s)", out.RigNamespace, tc.rig, raw)
			}
			if out.NamespaceChecked != tc.wantChecked || out.NamespaceVia != tc.wantVia {
				t.Fatalf("namespace_checked/via = %v/%q, want %v/%q (raw: %s)", out.NamespaceChecked, out.NamespaceVia, tc.wantChecked, tc.wantVia, raw)
			}
			var stdout, errOut bytes.Buffer
			run(rigNamespaceArgs(cityPath, tc.rig, tc.identity), &stdout, &errOut)
			validateManagementJSONPayload(t, []string{"agent", "is-foreign"}, &stdout)
		})
	}
}

// TestAgentIsForeign_WithoutRigNamespaceIsUnchanged: the guard applies only
// when the caller names the rig; the plain verb answers as before and echoes
// no rig, so a caller cannot mistake it for a namespace answer.
func TestAgentIsForeign_WithoutRigNamespaceIsUnchanged(t *testing.T) {
	cityPath := rnfVerbCity(t)
	code, out, raw, stderr := runIsForeign(t, isForeignArgs(cityPath, "beta/worker")...)
	if code != agentIsForeignExitLocal || out.Reason != string(claimReasonRoster) {
		t.Fatalf("exit/reason = %d/%q, want 0/roster\nstdout: %s\nstderr: %s", code, out.Reason, raw, stderr)
	}
	if out.RigNamespace != "" || out.NamespaceChecked || out.NamespaceVia != "" {
		t.Fatalf("plain verb reported namespace fields (raw: %s)", raw)
	}
	if strings.Contains(raw, "rig_namespace") {
		t.Fatalf("plain verb JSON carries rig_namespace: %s", raw)
	}
}

// TestAgentIsForeign_RigNamespaceBadUsageExitsTwo: a usage error with the
// flag set still cannot answer, and echoes the rig it was asked about.
func TestAgentIsForeign_RigNamespaceBadUsageExitsTwo(t *testing.T) {
	cityPath := rnfVerbCity(t)
	for _, args := range [][]string{
		rigNamespaceArgs(cityPath, "beta"),
		rigNamespaceArgs(cityPath, "beta", "neighbor", "beta/worker"),
	} {
		code, out, raw, stderr := runIsForeign(t, args...)
		if code != agentIsForeignExitUnknown || out.Verdict != agentIsForeignVerdictUnknown {
			t.Fatalf("%v: exit/verdict = %d/%q, want 2/unknown\nstdout: %s\nstderr: %s", args, code, out.Verdict, raw, stderr)
		}
		if out.RigNamespace != "beta" {
			t.Fatalf("%v: rig_namespace = %q, want beta (raw: %s)", args, out.RigNamespace, raw)
		}
	}
}

// --- the Go pool sweep's gate ---

// TestForeignClaimGate_RigNamespace: in a guarded rig the pool sweep keeps a
// claim the namespace does not admit, counts it per rig without naming it,
// and still sends an admitted claim through the usual liveness path. An
// unguarded rig's claim is untouched by the namespace.
func TestForeignClaimGate_RigNamespace(t *testing.T) {
	cfg := rnfCity()
	ownSessions, _ := rnsCityBResolver(t)
	sessionStore := beads.NewMemStore()
	gate := newForeignClaimGate(cfg, beads.SessionStore{Store: sessionStore})
	gate.namespaces = &poolRigNamespaceGate{cfg: cfg, sessions: ownSessions}
	live := map[string]bool{}

	// Kept by the namespace: counted for beta, not named.
	for _, c := range []struct{ assignee, workID, leg string }{
		{"neighbor", "bt-1", "rig:beta"},
		{"Pat Example", "bt-2", "rig:beta"},
		{poolIDA, "bt-3", "rig:beta"},
		// The leg is unknown: the bead's prefix names the rig.
		{"neighbor", "bt-4", ""},
	} {
		if !gate.namespaces.keeps(gate, c.assignee, c.workID, c.leg) {
			t.Fatalf("claim %s by %q on %q was not kept by beta's namespace", c.workID, c.assignee, c.leg)
		}
	}
	if got := gate.namespaces.kept["beta"]; got != 4 {
		t.Fatalf("kept[beta] = %d, want 4", got)
	}
	if gate.protected.claims() != 0 {
		t.Fatalf("namespace keeps were named as protected: %s", gate.protected.summary())
	}
	if got, want := gate.namespaces.summary(), "releaseOrphanedPoolAssignments: kept 4 claims outside guarded rig namespaces (beta: 4)"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}

	// Admitted: lexical and own pool session. The namespace lets them
	// through to the roster and liveness checks.
	for _, assignee := range []string{"beta/worker-2", poolIDB} {
		if gate.namespaces.keeps(gate, assignee, "bt-9", "rig:beta") {
			t.Fatalf("admitted %q was kept by the namespace", assignee)
		}
	}
	// An unguarded rig and the city's own store: the namespace does not
	// apply; the roster/record gate decides as before.
	if gate.namespaces.keeps(gate, "neighbor", "al-1", "rig:alpha") {
		t.Fatal("a claim in an unguarded rig was kept by a namespace")
	}
	if gate.namespaces.keeps(gate, "neighbor", "bt-5", "city") {
		t.Fatal("a claim in the city store was kept by a namespace")
	}
	// (A bare "neighbor" would pass the Go sweep's shape exemption to the
	// liveness probe; a qualified stranger is the roster gate's case.)
	if !gate.claimMayStillBeHeld("alpha/stranger", "al-1", "rig:alpha", live) {
		t.Fatal("an unobservable claim in an unguarded rig was not protected by the roster gate")
	}
	if gate.protected.claims() != 1 {
		t.Fatalf("protected claims = %d, want the one unguarded-rig claim named", gate.protected.claims())
	}
}

// TestForeignClaimGate_RigNamespaceLookupErrorProtects: a session store that
// cannot be read keeps a pool claim, and the claim is named, not counted, so
// a persistent failure stays visible.
func TestForeignClaimGate_RigNamespaceLookupErrorProtects(t *testing.T) {
	cfg := rnfCity()
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	store.setErr(errors.New("session store unreachable"))
	gate := newForeignClaimGate(cfg, beads.SessionStore{Store: beads.NewMemStore()})
	gate.namespaces = &poolRigNamespaceGate{cfg: cfg, sessions: newControllerSessionResolver(cityPathB, nil, rnsStoreOpener(store))}
	if !gate.claimMayStillBeHeld(poolIDB, "bt-1", "rig:beta", map[string]bool{}) {
		t.Fatal("a pool claim was released after the session lookup failed")
	}
	if gate.lookupErr == nil || gate.protected.claims() != 1 || len(gate.namespaces.kept) != 0 {
		t.Fatalf("lookupErr = %v, protected = %d, kept = %v; want the error recorded and the claim named", gate.lookupErr, gate.protected.claims(), gate.namespaces.kept)
	}
}

// TestDeadAssigneeKeepLog_NamespaceCountLogsOnChange: the count line is
// logged when it changes, not every pass.
func TestDeadAssigneeKeepLog_NamespaceCountLogsOnChange(t *testing.T) {
	var l deadAssigneeKeepLog
	line := "releaseOrphanedPoolAssignments: kept 2 claims outside guarded rig namespaces (beta: 2)"
	if !l.logNamespaceKept(line) {
		t.Fatal("first count was not logged")
	}
	if l.logNamespaceKept(line) {
		t.Fatal("an unchanged count was logged again")
	}
	if !l.logNamespaceKept(strings.Replace(line, "2", "3", 2)) {
		t.Fatal("a changed count was not logged")
	}
	if !l.logNamespaceKept("") {
		t.Fatal("a drop to none was not logged")
	}
	if l.logNamespaceKept("") {
		t.Fatal("none was logged twice")
	}
	var none *deadAssigneeKeepLog
	if none.logNamespaceKept("") {
		t.Fatal("a nil log logged an empty line")
	}
}

// TestNewPoolRigNamespaceGate_NilWithoutGuardedRigs: a city with no guarded
// rig gets no namespace gate, so its sweep is unchanged.
func TestNewPoolRigNamespaceGate_NilWithoutGuardedRigs(t *testing.T) {
	cfg := rnfCity()
	cfg.Rigs[0].Federation = ""
	if g := newPoolRigNamespaceGate(cfg, cityPathB, beads.SessionStore{Store: beads.NewMemStore()}, nil); g != nil {
		t.Fatal("a city without guarded rigs got a namespace gate")
	}
	cfg = rnfCity()
	if g := newPoolRigNamespaceGate(cfg, cityPathB, beads.SessionStore{Store: beads.NewMemStore()}, nil); g == nil {
		t.Fatal("a city with a guarded rig got no namespace gate")
	}
}

// TestReleaseOrphanedPoolAssignments_GuardedRigKeepsClaimsOutsideNamespace is
// one sweep over a guarded rig: a bare claim (which the Go sweep's shape
// exemption would otherwise probe and release), a person's claim and another
// city's pool session claim are kept and counted, not named; a dead local
// pool worker's claim is still released.
func TestReleaseOrphanedPoolAssignments_GuardedRigKeepsClaimsOutsideNamespace(t *testing.T) {
	cityPath := t.TempDir()
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	cfg := foreignIdentityTestCity(t)
	cfg.Rigs[0].Federation = config.RigFederationRig

	kept := []beads.Bead{
		seedForeignIdentityWork(t, rigStore, "claimed by a bare name", "neighbor"),
		seedForeignIdentityWork(t, rigStore, "claimed by a person", "Pat Example"),
		seedForeignIdentityWork(t, rigStore, "claimed by another city's pool session", poolIDA),
	}
	local := seedForeignIdentityWork(t, rigStore, "held by a dead local pool worker", "repo/worker-1")
	work := append(append([]beads.Bead(nil), kept...), local)
	stores := []beads.Store{rigStore, rigStore, rigStore, rigStore}
	refs := []string{"repo", "repo", "repo", "repo"}

	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignmentsFromBeads(cityStore, cfg, cityPath, nil, work, stores, refs, map[string]beads.Store{"repo": rigStore})
	if len(released) != 1 || released[0].ID != local.ID {
		t.Fatalf("released = %v, want exactly [%s] (the dead local pool worker)", released, local.ID)
	}
	for _, wb := range kept {
		got, err := rigStore.Get(wb.ID)
		if err != nil {
			t.Fatalf("Get %s: %v", wb.ID, err)
		}
		if got.Status != "in_progress" || got.Assignee != wb.Assignee {
			t.Fatalf("%s = %q/%q, want the claim by %q intact", wb.ID, got.Status, got.Assignee, wb.Assignee)
		}
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "kept 3 claims outside guarded rig namespaces (repo: 3)") {
		t.Fatalf("missing the namespace count line:\n%s", logged)
	}
	if strings.Contains(logged, "foreign/unknown identities") {
		t.Fatalf("namespace keeps were named as protected identities:\n%s", logged)
	}

	// The same sweep with the rig unguarded keeps today's behavior: the bare
	// claim is probed and released.
	cfg.Rigs[0].Federation = ""
	bare := seedForeignIdentityWork(t, rigStore, "bare claim, unguarded", "neighbor")
	released = releaseOrphanedPoolAssignmentsFromBeads(cityStore, cfg, cityPath, nil, []beads.Bead{bare}, []beads.Store{rigStore}, []string{"repo"}, map[string]beads.Store{"repo": rigStore})
	if len(released) != 1 {
		t.Fatalf("unguarded rig: released = %v, want the bare claim released as before", released)
	}
}

// TestAgentIsForeign_RigNamespaceOwnPoolSessionFromStore takes the CLI's
// store path, not the environment fast path: the caller is no session, and
// the city's own pool session record is found in its sessions store by id.
// Another pool id under the same prefix, with no record, is foreign.
func TestAgentIsForeign_RigNamespaceOwnPoolSessionFromStore(t *testing.T) {
	cityPath := rnfVerbCity(t)
	for _, key := range []string{"GC_CITY_PATH", "GC_CITY", "GC_SESSION_ID", "GC_SESSION_NAME", "GC_TEMPLATE"} {
		t.Setenv(key, "")
	}
	ownPoolID := "tc-session-" + rnfToken
	store, err := openStoreAtForCityWithConfig(cityPath, cityPath, nil)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	engine, _, _ := unwrapBeadPolicyStore(splitStrictFrontOf(store))
	fileStore, ok := engine.(*beads.FileStore)
	if !ok {
		t.Fatalf("city store engine is %T, want *beads.FileStore", engine)
	}
	fileStore.HonorExplicitIDs = true
	rec := rnsSessionBead(ownPoolID, "beta/worker", "worker-"+ownPoolID)
	rec.Status = ""
	created, err := store.Create(rec)
	if err != nil || created.ID != ownPoolID {
		t.Fatalf("seed own pool session record: id %q, err %v", created.ID, err)
	}

	for _, tc := range []struct {
		identity   string
		wantCode   int
		wantReason claimProvenanceReason
		wantVia    string
	}{
		{ownPoolID, agentIsForeignExitLocal, claimReasonSessionRecord, "session_record"},
		{"worker-" + ownPoolID, agentIsForeignExitLocal, claimReasonSessionRecord, "session_record"},
		{"tc-session-fedcba9876543210fedcba9876543210", agentIsForeignExitForeign, claimReasonNamespace, ""},
	} {
		code, out, raw, stderr := runIsForeign(t, rigNamespaceArgs(cityPath, "beta", tc.identity)...)
		if code != tc.wantCode || out.Reason != string(tc.wantReason) || out.NamespaceVia != tc.wantVia {
			t.Fatalf("%s: exit/reason/via = %d/%q/%q, want %d/%q/%q\nstdout: %s\nstderr: %s", tc.identity, code, out.Reason, out.NamespaceVia, tc.wantCode, tc.wantReason, tc.wantVia, raw, stderr)
		}
	}
}

// TestForeignClaimGate_UnknownRigLegFallsBackToPrefix: a leg naming a rig
// this city does not configure says nothing about the store; the bead's
// prefix still finds the guarded rig, so the claim is judged there.
func TestForeignClaimGate_UnknownRigLegFallsBackToPrefix(t *testing.T) {
	cfg := rnfCity()
	ownSessions, _ := rnsCityBResolver(t)
	gate := newForeignClaimGate(cfg, beads.SessionStore{Store: beads.NewMemStore()})
	gate.namespaces = &poolRigNamespaceGate{cfg: cfg, sessions: ownSessions}
	if !gate.namespaces.keeps(gate, "neighbor", "bt-7", "rig:gamma") {
		t.Fatal("a guarded rig's claim under an unknown-rig leg was not judged by the rig's namespace")
	}
	if gate.namespaces.keeps(gate, "neighbor", "zz-7", "rig:gamma") {
		t.Fatal("a claim with no guarded rig by leg or prefix was kept by a namespace")
	}
}

// TestReleaseOrphanedPoolAssignments_GuardedRigOwnPoolSessionFromSessionStore
// runs the controller's path end to end: newPoolRigNamespaceGate's resolver
// reads the city's sessions store, behind the bead-policy layer (which hides
// the store's pool id prefix; the HQ prefix differs, so only the look-through
// finds it). This city's own dead pool session's claim is released; another
// city's pool session claim is kept.
func TestReleaseOrphanedPoolAssignments_GuardedRigOwnPoolSessionFromSessionStore(t *testing.T) {
	cityPath := t.TempDir()
	rigStore := beads.NewMemStore()
	cfg := foreignIdentityTestCity(t)
	cfg.Workspace.Prefix = "hq"
	cfg.Rigs[0].Federation = config.RigFederationRig
	record := rnsSessionBead(poolIDB, "repo/worker", "worker-"+poolIDB)
	record.Status = "closed"
	sessions := wrapStoreWithBeadPolicies(rnsNewCountingStore(record), cfg)

	own := seedForeignIdentityWork(t, rigStore, "held by this city's dead pool session", poolIDB)
	foreign := seedForeignIdentityWork(t, rigStore, "held by another city's pool session", poolIDA)
	logBuf := captureSweepLog(t)
	released := releaseOrphanedPoolAssignments(beads.NewMemStore(), beads.SessionStore{Store: sessions}, cfg, cityPath, nil,
		[]beads.Bead{own, foreign}, []beads.Store{rigStore, rigStore}, []string{"repo", "repo"},
		map[string]beads.Store{"repo": rigStore}, nil, nil, nil)
	if len(released) != 1 || released[0].ID != own.ID {
		t.Fatalf("released = %v, want exactly [%s] (this city's own dead pool session)\n%s", released, own.ID, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "kept 1 claims outside guarded rig namespaces (repo: 1)") {
		t.Fatalf("missing the namespace count for the other city's claim:\n%s", logBuf.String())
	}
}
