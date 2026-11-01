package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// isForeignTestCity writes a city.toml whose roster mirrors the shapes a city
// actually runs, so the verb is exercised against resolved config rather than
// a hand-built config.City: the verb's job includes LOADING the roster, and a
// test that skipped that step would not catch a verb that resolves nothing and
// calls every identity foreign. It runs on the file bead store, and returns
// the IDs of the closed session records it seeds there: one under a qualified
// alias that left the roster, and one unaliased ephemeral seat.
func isForeignTestCity(t *testing.T) (cityPath, aliasRecordID, ephemeralRecordID string) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	cityPath = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "repo"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	// The namepool is a FILE at load time (Agent.NamepoolNames is toml:"-"), so
	// the themed-instance resolver can only be exercised through one.
	if err := os.WriteFile(filepath.Join(cityPath, "namepool.txt"), []byte("furiosa\nnux\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(namepool): %v", err)
	}
	cityToml := `[workspace]
name = "testcity"
prefix = "tc"

[[rigs]]
name = "repo"
path = "repo"
prefix = "rp"

[[agent]]
name = "worker"
dir = "repo"
min_active_sessions = 0
max_active_sessions = 2
namepool = "namepool.txt"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	store, err := openStoreAtForCityWithConfig(cityPath, cityPath, nil)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	seed := func(metadata map[string]string) string {
		t.Helper()
		b, err := store.Create(beads.Bead{
			Title:    "session record",
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: metadata,
		})
		if err != nil {
			t.Fatalf("seed session record: %v", err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("close session record: %v", err)
		}
		return b.ID
	}
	aliasRecordID = seed(map[string]string{"alias": "repo/retired-seat"})
	ephemeralRecordID = seed(map[string]string{"session_name": "worker-dead-seat", "session_origin": "ephemeral"})
	return cityPath, aliasRecordID, ephemeralRecordID
}

func runIsForeign(t *testing.T, args ...string) (code int, out AgentIsForeignJSON, raw, stderr string) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer
	code = run(args, &stdoutBuf, &stderrBuf)
	raw = stdoutBuf.String()
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &out); err != nil {
			t.Fatalf("is-foreign %v: stdout is not parseable JSON (%v):\n%s", args, err, raw)
		}
	}
	return code, out, raw, stderrBuf.String()
}

func isForeignArgs(cityPath string, identities ...string) []string {
	return append([]string{"--city", cityPath, "agent", "is-foreign", "--json", "--"}, identities...)
}

// TestAgentIsForeign_VerdictsAndExitCodes is the non-vacuity test: local and
// foreign identities are asserted in ONE table. Either half alone is
// satisfiable by a verb that answers the same way for everything.
func TestAgentIsForeign_VerdictsAndExitCodes(t *testing.T) {
	cityPath, aliasRecordID, ephemeralRecordID := isForeignTestCity(t)

	for _, tc := range []struct {
		name       string
		identity   string
		wantCode   int
		wantVerd   string
		wantReason claimProvenanceReason
		wantDetail string
	}{
		{"configured agent", "repo/worker", agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonRoster, ""},
		{"namepool instance", "repo/furiosa", agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonRoster, ""},
		{"numeric slot instance", "repo/worker-3", agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonRoster, ""},
		{"qualified alias with a closed record", "repo/retired-seat", agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonSessionRecord, aliasRecordID},
		{"session bead ID with a closed record", ephemeralRecordID, agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonSessionRecord, ephemeralRecordID},
		{"runtime session name with a closed record", "worker-dead-seat", agentIsForeignExitLocal, agentIsForeignVerdictLocal, claimReasonSessionRecord, ephemeralRecordID},
		// ga-8yi7ne: "pool" is not a binding this city mints, and "worker" is
		// a local agent, so a binding-stripping gate would call this local.
		{"foreign binding", "repo/pool.worker-1", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonForeignBinding, "pool"},
		{"absent from the roster", "repo/stranger", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"rig this city does not configure", "otherrig/worker", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonAbsentFromRoster, ""},
		// ga-x1f77i: another store's bare session bead ID.
		{"foreign bare session bead ID", "we-wisp-126vyfx", agentIsForeignExitForeign, agentIsForeignVerdictForeign, claimReasonAbsentFromRoster, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, raw, stderr := runIsForeign(t, isForeignArgs(cityPath, tc.identity)...)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.wantCode, raw, stderr)
			}
			if out.Verdict != tc.wantVerd || out.Reason != string(tc.wantReason) || out.Detail != tc.wantDetail {
				t.Fatalf("verdict/reason/detail = %q/%q/%q, want %q/%q/%q (raw: %s)", out.Verdict, out.Reason, out.Detail, tc.wantVerd, tc.wantReason, tc.wantDetail, raw)
			}
			if out.Identity != tc.identity {
				t.Fatalf("identity echoed as %q, want %q", out.Identity, tc.identity)
			}
			if strings.TrimSpace(out.RosterSource) == "" {
				t.Fatalf("roster_source is empty; a verdict must name the roster it was read from (raw: %s)", raw)
			}
		})
	}
}

// TestAgentIsForeign_CannotAnswerExitsTwo pins the fail-closed contract's
// other half: exit 2 means "cannot answer", distinct from 1 ("foreign"), so a
// mis-invocation never reads as "local" and reaps. The degraded answer is still
// well-formed JSON, so a caller can say why it protected a claim.
func TestAgentIsForeign_CannotAnswerExitsTwo(t *testing.T) {
	cityPath, _, _ := isForeignTestCity(t)
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "city.toml"), []byte("this is not = valid = toml ["), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no identity", isForeignArgs(cityPath)},
		{"two identities", isForeignArgs(cityPath, "repo/worker", "repo/stranger")},
		{"unresolvable city", isForeignArgs(filepath.Join(t.TempDir(), "missing"), "repo/worker")},
		{"unparseable city.toml", isForeignArgs(broken, "repo/worker")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, raw, stderr := runIsForeign(t, tc.args...)
			if code != agentIsForeignExitUnknown {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, agentIsForeignExitUnknown, raw, stderr)
			}
			if out.Verdict != agentIsForeignVerdictUnknown {
				t.Fatalf("verdict = %q, want %q (raw: %s; stderr: %s)", out.Verdict, agentIsForeignVerdictUnknown, raw, stderr)
			}
			if strings.TrimSpace(out.Detail) == "" {
				t.Fatalf("degraded verdict carries no detail (raw: %s)", raw)
			}
		})
	}
}

// TestAgentIsForeign_WarningsStayOffStdout guards the parse contract: a caller
// reads stdout as JSON, so a config-load warning there would make a healthy
// verdict unparseable, which the fail-closed rule turns into a protect.
func TestAgentIsForeign_WarningsStayOffStdout(t *testing.T) {
	cityPath, _, _ := isForeignTestCity(t)

	code, out, raw, stderr := runIsForeign(t, isForeignArgs(cityPath, "repo/worker")...)
	// Non-vacuity: this only proves anything if a warning was produced. The
	// fixture declares the rig path in city.toml, which the loader warns about.
	if strings.TrimSpace(stderr) == "" {
		t.Fatal("fixture produced no config warning, so this test cannot show that warnings stay off stdout")
	}
	if code != agentIsForeignExitLocal || out.Identity != "repo/worker" {
		t.Fatalf("exit = %d, identity = %q; want 0 and repo/worker\nstdout: %s\nstderr: %s", code, out.Identity, raw, stderr)
	}
}

// TestAgentIsForeign_JSONMatchesDeclaredSchema validates every verdict's
// payload against the declared result schema.
func TestAgentIsForeign_JSONMatchesDeclaredSchema(t *testing.T) {
	cityPath, _, _ := isForeignTestCity(t)
	for _, identity := range []string{"repo/worker", "repo/stranger", "repo/pool.worker-1"} {
		var stdout, stderr bytes.Buffer
		run(isForeignArgs(cityPath, identity), &stdout, &stderr)
		validateManagementJSONPayload(t, []string{"agent", "is-foreign"}, &stdout)
	}
	var stdout, stderr bytes.Buffer
	run(isForeignArgs(cityPath), &stdout, &stderr)
	validateManagementJSONPayload(t, []string{"agent", "is-foreign"}, &stdout)
}

// TestAgentIsForeign_DashLeadingIdentityAfterSeparator answers an identity
// that looks like a flag when it follows "--", as the orphan-sweep order
// passes every untrusted assignee.
func TestAgentIsForeign_DashLeadingIdentityAfterSeparator(t *testing.T) {
	cityPath, _, _ := isForeignTestCity(t)
	code, out, raw, stderr := runIsForeign(t, isForeignArgs(cityPath, "-odd")...)
	if code != agentIsForeignExitForeign || out.Verdict != agentIsForeignVerdictForeign || out.Identity != "-odd" {
		t.Fatalf("exit = %d, verdict = %q, identity = %q; want 1, foreign, -odd\nstdout: %s\nstderr: %s", code, out.Verdict, out.Identity, raw, stderr)
	}
}

// TestAgentIsForeign_TextOutput keeps the human form one line per verdict.
func TestAgentIsForeign_TextOutput(t *testing.T) {
	cityPath, _, _ := isForeignTestCity(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "agent", "is-foreign", "repo/pool.worker-1"}, &stdout, &stderr)
	if code != agentIsForeignExitForeign {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, agentIsForeignExitForeign, stderr.String())
	}
	if got, want := stdout.String(), "repo/pool.worker-1 foreign: foreign_binding (pool)\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// TestAgentIsForeign_LoadsCityConfigOnceOnBdCity keeps the verb's cost down on
// a bd-backed city: orphan-sweep calls it once per unrecognized claimant every
// sweep, so the store open must reuse the config the verb already loaded
// rather than parse city.toml and every pack include a second time.
//
// Not parallel: loadCityConfigCalls is process-wide.
func TestAgentIsForeign_LoadsCityConfigOnceOnBdCity(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Skip native preflight so the open goes straight to the bd store.
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "1")
	t.Setenv("GC_BEADS", "")
	// No managed Dolt server: the bd stub answers every read.
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	toml := "[workspace]\nname = \"t\"\nprefix = \"mc\"\n\n[beads]\nprovider = \"bd\"\n\n[[agent]]\nname = \"worker\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	before := loadCityConfigCalls.Load()
	var stdout, stderr bytes.Buffer
	// The identity is off the roster, so answering it needs the session store.
	doAgentIsForeign(fsys.OSFS{}, cityDir, "nobody-here", true, &stdout, &stderr)
	if grew := loadCityConfigCalls.Load() - before; grew != 1 {
		t.Fatalf("gc agent is-foreign parsed city config %d times on a bd city, want exactly 1 (its own load)\nstdout: %s\nstderr: %s", grew, stdout.String(), stderr.String())
	}
}
