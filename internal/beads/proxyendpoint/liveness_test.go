package proxyendpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeTable is a process table a test writes by hand. Every liveness branch is
// reachable from it, so no test in this package needs a proxy, a privileged
// read, or a process of its own.
type fakeTable struct {
	alive    map[int]bool
	argv     map[int][]string
	argvErr  map[int]error
	birth    map[int]string
	birthErr map[int]error
}

func (f fakeTable) table() ProcessTable {
	return ProcessTable{
		Alive: func(pid int) bool { return f.alive[pid] },
		Argv: func(pid int) ([]string, error) {
			if err := f.argvErr[pid]; err != nil {
				return nil, err
			}
			return f.argv[pid], nil
		},
		Birth: func(pid int) (string, error) {
			if err := f.birthErr[pid]; err != nil {
				return "", err
			}
			return f.birth[pid], nil
		},
	}
}

// childArgv is the argv bd's fork-exec produces for a proxy supervisor, in the
// order bd builds it (beads dbproxy/proxy/endpoint.go forkExecChild).
func childArgv(root string, idle string) []string {
	return []string{
		"/opt/beads/bd-1.3.0", ChildVerb,
		RootFlag, root,
		"--port", "45123",
		IdleTimeoutFlag, idle,
		"--backend", "local-server",
		"--stop-epoch", "7",
		"--config", filepath.Join(root, ConfigFileName),
		"--database", "beads",
	}
}

func TestInspectVerdictTable(t *testing.T) {
	root := t.TempDir()
	rec := validRecord(t, root)
	writeRecord(t, root, rec)

	cases := []struct {
		name         string
		table        fakeTable
		wantVerdict  Verdict
		wantEvidence Evidence
		wantBirth    BirthOutcome
	}{
		{
			name: "live with both halves of the proof",
			table: fakeTable{
				alive: map[int]bool{rec.PID: true},
				argv:  map[int][]string{rec.PID: childArgv(root, "-1ns")},
				birth: map[int]string{rec.PID: rec.Birth},
			},
			wantVerdict:  VerdictLive,
			wantEvidence: EvidenceArgvBirth,
			wantBirth:    BirthMatch,
		},
		{
			name: "dead pid",
			table: fakeTable{
				alive: map[int]bool{rec.PID: false},
			},
			wantVerdict: VerdictDead,
		},
		{
			// The recycled-PID case a bare liveness probe gets wrong: the number
			// is live and belongs to something else entirely.
			name: "recycled pid running something else",
			table: fakeTable{
				alive: map[int]bool{rec.PID: true},
				argv:  map[int][]string{rec.PID: {"/usr/bin/vim", "notes.md"}},
			},
			wantVerdict: VerdictForeignProcess,
		},
		{
			// Same verb, another root: a second city's proxy that happens to
			// have inherited this PID number.
			name: "bd's proxy for another root",
			table: fakeTable{
				alive: map[int]bool{rec.PID: true},
				argv:  map[int][]string{rec.PID: childArgv(t.TempDir(), "-1ns")},
			},
			wantVerdict: VerdictForeignProcess,
		},
		{
			// bd restarted the proxy and gc did not see it happen: the record on
			// disk describes a generation that is no longer running, even though
			// the PID and argv match.
			name: "same argv, another birth",
			table: fakeTable{
				alive: map[int]bool{rec.PID: true},
				argv:  map[int][]string{rec.PID: childArgv(root, "-1ns")},
				birth: map[int]string{rec.PID: BirthToken("boot-1", "12345")},
			},
			wantVerdict:  VerdictBirthMismatch,
			wantEvidence: EvidenceArgv,
			wantBirth:    BirthMismatch,
		},
		{
			// A host with no recipe for bd's token: live on argv alone, and the
			// diagnostic says which half it got.
			name: "birth unavailable stays live on argv",
			table: fakeTable{
				alive:    map[int]bool{rec.PID: true},
				argv:     map[int][]string{rec.PID: childArgv(root, "-1ns")},
				birthErr: map[int]error{rec.PID: ErrBirthUnavailable},
			},
			wantVerdict:  VerdictLive,
			wantEvidence: EvidenceArgv,
			wantBirth:    BirthUnavailable,
		},
		{
			name: "unreadable argv is undetermined, not dead",
			table: fakeTable{
				alive:   map[int]bool{rec.PID: true},
				argvErr: map[int]error{rec.PID: os.ErrPermission},
			},
			wantVerdict: VerdictUndetermined,
		},
		{
			// The process exited between the liveness probe and the stat. Not a
			// mismatch, and not an endpoint to dial.
			name: "birth read failure is undetermined",
			table: fakeTable{
				alive:    map[int]bool{rec.PID: true},
				argv:     map[int][]string{rec.PID: childArgv(root, "-1ns")},
				birthErr: map[int]error{rec.PID: os.ErrNotExist},
			},
			wantVerdict:  VerdictUndetermined,
			wantEvidence: EvidenceArgv,
			wantBirth:    BirthUnchecked,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := Inspect(root, tc.table.table())
			if ep.Verdict != tc.wantVerdict {
				t.Fatalf("Inspect verdict = %v (%v), want %v", ep.Verdict, ep.Err, tc.wantVerdict)
			}
			if ep.Liveness.Evidence != tc.wantEvidence {
				t.Errorf("evidence = %v, want %v", ep.Liveness.Evidence, tc.wantEvidence)
			}
			if ep.Liveness.Birth != tc.wantBirth {
				t.Errorf("birth outcome = %v, want %v", ep.Liveness.Birth, tc.wantBirth)
			}
			if ep.Verdict.Live() != (tc.wantVerdict == VerdictLive) {
				t.Errorf("Live() = %v for verdict %v", ep.Verdict.Live(), ep.Verdict)
			}
		})
	}
}

func TestInspectRecordVerdicts(t *testing.T) {
	live := func(root string, rec Record) ProcessTable {
		return fakeTable{
			alive: map[int]bool{rec.PID: true},
			argv:  map[int][]string{rec.PID: childArgv(root, "-1ns")},
			birth: map[int]string{rec.PID: rec.Birth},
		}.table()
	}

	t.Run("no record", func(t *testing.T) {
		root := t.TempDir()
		ep := Inspect(root, DefaultProcessTable())
		if ep.Verdict != VerdictNoRecord || !errors.Is(ep.Err, ErrNoProxy) {
			t.Fatalf("Inspect on an empty root = %v (%v), want no_record", ep.Verdict, ep.Err)
		}
	})

	t.Run("malformed record", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(PIDPath(root), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ep := Inspect(root, DefaultProcessTable()); ep.Verdict != VerdictMalformed {
			t.Fatalf("Inspect on a truncated record = %v, want malformed", ep.Verdict)
		}
	})

	t.Run("legacy schema", func(t *testing.T) {
		root := t.TempDir()
		rec := validRecord(t, root)
		rec.Schema = 1
		writeRecord(t, root, rec)
		if ep := Inspect(root, live(root, rec)); ep.Verdict != VerdictLegacySchema {
			t.Fatalf("Inspect on a schema-1 record = %v, want legacy_schema", ep.Verdict)
		}
	})

	t.Run("a foreign record is refused without consulting the process table", func(t *testing.T) {
		root := t.TempDir()
		rec := validRecord(t, root)
		rec.RootID = "deadbeef"
		writeRecord(t, root, rec)
		consulted := false
		table := ProcessTable{
			Alive: func(int) bool { consulted = true; return true },
			Argv:  func(int) ([]string, error) { consulted = true; return nil, nil },
			Birth: func(int) (string, error) { consulted = true; return "", nil },
		}
		ep := Inspect(root, table)
		if ep.Verdict != VerdictNotOurs {
			t.Fatalf("Inspect on another root's record = %v, want not_ours", ep.Verdict)
		}
		if consulted {
			t.Fatal("a record that is not ours reached the process table; the identity check must come first")
		}
	})

	t.Run("the record and root id are reported whatever the verdict", func(t *testing.T) {
		root := t.TempDir()
		rec := validRecord(t, root)
		writeRecord(t, root, rec)
		ep := Inspect(root, fakeTable{alive: map[int]bool{rec.PID: false}}.table())
		if ep.Record != rec {
			t.Fatalf("Inspect dropped the record it read: %+v", ep.Record)
		}
		if ep.RootID != rec.RootID {
			t.Fatalf("Inspect root id = %q, want %q", ep.RootID, rec.RootID)
		}
	})
}

func TestArgvIdlePolicyExtraction(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		argv []string
		want IdlePolicy
	}{
		{
			// bd renders IdleTimeoutNever with Duration.String(), which is
			// "-1ns" and not "-1".
			name: "bd's never spelling",
			argv: childArgv(root, "-1ns"),
			want: IdlePolicy{Kind: IdleNever, Source: IdleSourceArgv},
		},
		{
			name: "bd's default window",
			argv: childArgv(root, "30s"),
			want: IdlePolicy{Kind: IdleFinite, Timeout: 30 * time.Second, Source: IdleSourceArgv},
		},
		{
			name: "an operator's window",
			argv: childArgv(root, "5m0s"),
			want: IdlePolicy{Kind: IdleFinite, Timeout: 5 * time.Minute, Source: IdleSourceArgv},
		},
		{
			// The supervisor disables its idle watcher for any value <= 0, and
			// the provider's substitution already happened before the fork, so
			// an argv zero is a proxy that will not retire.
			name: "zero on the command line is never",
			argv: childArgv(root, "0s"),
			want: IdlePolicy{Kind: IdleNever, Source: IdleSourceArgv},
		},
		{
			name: "the joined flag spelling",
			argv: []string{"bd", ChildVerb, RootFlag + "=" + root, IdleTimeoutFlag + "=45s"},
			want: IdlePolicy{Kind: IdleFinite, Timeout: 45 * time.Second, Source: IdleSourceArgv},
		},
		{
			name: "no flag leaves the question to the sidecar",
			argv: []string{"bd", ChildVerb, RootFlag, root},
			want: IdlePolicy{},
		},
		{
			name: "an unparseable value leaves the question open rather than guessing",
			argv: childArgv(root, "forever"),
			want: IdlePolicy{},
		},
		{
			name: "a trailing flag with no value",
			argv: []string{"bd", ChildVerb, RootFlag, root, IdleTimeoutFlag},
			want: IdlePolicy{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArgvIdlePolicy(tc.argv); got != tc.want {
				t.Fatalf("ArgvIdlePolicy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestArgvRunsChild(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"bd's own spelling", []string{"bd", ChildVerb, RootFlag, "/x"}, true},
		{"a versioned pin is still bd", []string{"/opt/beads/bd-1.3.0-rc.2", ChildVerb}, true},
		{"the verb has to be argv[1]", []string{"bd", "dolt", ChildVerb}, false},
		{"another verb", []string{"bd", "ping"}, false},
		{"argv[0] alone", []string{"bd"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		if got := ArgvRunsChild(tc.argv); got != tc.want {
			t.Errorf("%s: ArgvRunsChild(%q) = %v, want %v", tc.name, tc.argv, got, tc.want)
		}
	}
}

func TestArgvNamesRoot(t *testing.T) {
	base := t.TempDir()
	resolved := filepath.Join(base, "resolved")
	if err := os.MkdirAll(resolved, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		argv []string
		root string
		want bool
	}{
		{"exact spelling", []string{"bd", ChildVerb, RootFlag, resolved}, resolved, true},
		{"joined spelling", []string{"bd", ChildVerb, RootFlag + "=" + resolved}, resolved, true},
		{"argv names the symlink, caller the target", []string{"bd", ChildVerb, RootFlag, link}, resolved, true},
		{"argv names the target, caller the symlink", []string{"bd", ChildVerb, RootFlag, resolved}, link, true},
		{"a sibling root is not this root", []string{"bd", ChildVerb, RootFlag, sibling}, resolved, false},
		{"no --root at all", []string{"bd", ChildVerb, "--port", "1"}, resolved, false},
		{"an empty --root value", []string{"bd", ChildVerb, RootFlag, ""}, resolved, false},
		{"a trailing --root", []string{"bd", ChildVerb, RootFlag}, resolved, false},
		{"an empty caller root matches nothing", []string{"bd", ChildVerb, RootFlag, resolved}, "", false},
	}
	for _, tc := range cases {
		if got := ArgvNamesRoot(tc.argv, tc.root); got != tc.want {
			t.Errorf("%s: ArgvNamesRoot(%q, %q) = %v, want %v", tc.name, tc.argv, tc.root, got, tc.want)
		}
	}
}

// TestVerdictTokensAreStable pins the strings automation reads out of `gc doctor
// --json`. A rename here is a wire change, so it fails here first.
func TestVerdictTokensAreStable(t *testing.T) {
	want := map[Verdict]string{
		VerdictUnknown:        "unknown",
		VerdictLive:           "live",
		VerdictNoRecord:       "no_record",
		VerdictMalformed:      "malformed",
		VerdictLegacySchema:   "legacy_schema",
		VerdictNotOurs:        "not_ours",
		VerdictDead:           "dead",
		VerdictForeignProcess: "foreign_process",
		VerdictBirthMismatch:  "birth_mismatch",
		VerdictUndetermined:   "undetermined",
	}
	for verdict, token := range want {
		if got := verdict.String(); got != token {
			t.Errorf("Verdict(%d).String() = %q, want %q", verdict, got, token)
		}
	}
	if len(want) != int(VerdictUndetermined)+1 {
		t.Fatalf("the verdict enum has %d values but %d are pinned; add the new one here", int(VerdictUndetermined)+1, len(want))
	}
	for _, tc := range []struct {
		evidence Evidence
		token    string
	}{{EvidenceNone, "none"}, {EvidenceArgv, "argv"}, {EvidenceArgvBirth, "argv+birth"}} {
		if got := tc.evidence.String(); got != tc.token {
			t.Errorf("Evidence(%d).String() = %q, want %q", tc.evidence, got, tc.token)
		}
	}
	for _, tc := range []struct {
		birth BirthOutcome
		token string
	}{{BirthUnchecked, "unchecked"}, {BirthMatch, "match"}, {BirthMismatch, "mismatch"}, {BirthUnavailable, "unavailable"}} {
		if got := tc.birth.String(); got != tc.token {
			t.Errorf("BirthOutcome(%d).String() = %q, want %q", tc.birth, got, tc.token)
		}
	}
}

// TestCheckLivenessWithoutAProcessTable pins that a caller who supplies no
// process table gets "undetermined" rather than a live endpoint.
func TestCheckLivenessWithoutAProcessTable(t *testing.T) {
	root := t.TempDir()
	rec := validRecord(t, root)
	writeRecord(t, root, rec)
	if ep := Inspect(root, ProcessTable{}); ep.Verdict != VerdictUndetermined {
		t.Fatalf("Inspect with no process table = %v, want undetermined", ep.Verdict)
	}
}
