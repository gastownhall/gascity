package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// stubExactStoreFactory makes every factory store open in this test answer
// from an in-memory store holding held. Its Get is exact, like the native
// store's SQL lookup on a native-eligible scope, so it reports an id that only
// prefix-matches a held bead as absent. It returns a count of the opens.
func stubExactStoreFactory(t *testing.T, held ...beads.Bead) *atomic.Int64 {
	t.Helper()
	opens := &atomic.Int64{}
	restore := openStoreFactoryForCity
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		opens.Add(1)
		return beads.StoreOpenResult{Store: beads.NewMemStoreFrom(0, held, nil)}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = restore })
	return opens
}

// writeBoundBackendMetadata binds scopeRoot's workspace to a remote storage
// backend the way a hosted city's metadata does (backend plus a complete
// storage binding), so gc hands every read and write to the workspace's bd.
func writeBoundBackendMetadata(t *testing.T, scopeRoot, backend string) {
	t.Helper()
	metadata := `{"database":"dolt","backend":"` + backend + `","project_id":"d2e95604-e869-478c-ad1a-ddee6e8bc3fc",` +
		`"storage_endpoint":"https://bdserve.example.invalid:30635","storage_database":"bd_prj_test"}` + "\n"
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeScopeBackendMetadata gives scopeRoot's workspace the metadata a
// workspace on backend carries: none for "", the bare local Dolt marker for
// "dolt", and a complete remote binding for any other backend.
func writeScopeBackendMetadata(t *testing.T, scopeRoot, backend string) {
	t.Helper()
	switch backend {
	case "":
	case "dolt":
		if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(`{"backend":"dolt"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		writeBoundBackendMetadata(t, scopeRoot, backend)
	}
}

func readBdCallLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// TestGcBdWriteGuardAsksBdEvenWhenTheScopeOpensNative pins the exact-ID write
// guard (gcy-g4o) on a scope whose store opens native. The collision the guard
// exists for is bd's own: when a write's id names no bead exactly, bd resolves
// it by prefix match and mutates whatever it matched. A native store's Get is
// an exact lookup, so it can only report such an id absent, and the guard used
// to treat absent as "let bd decide" — the write went through to the matched
// bead. Only bd's resolver can show the collision, so the guard reads through
// bd whatever the scope's store opens as.
func TestGcBdWriteGuardAsksBdEvenWhenTheScopeOpensNative(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, "")
	setCwd(t, cityDir)
	calls := writeBatchGuardBdStub(t, `    "show --json demo-dv7")
      printf '[{"id":"demo-wisp-dv78","status":"open"}]\n' ;;`)
	stubExactStoreFactory(t, beads.Bead{ID: "demo-wisp-dv78", Title: "matched by prefix", Status: "open", Type: "task"})

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"update", "demo-dv7", "--status", "in_progress"}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBd update = %d, want 1 for a substring collision; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "substring collision") {
		t.Fatalf("stderr = %q, want the collision refusal", stderr.String())
	}
	if log := readBdCallLog(t, calls); strings.Contains(log, "update ") {
		t.Fatalf("bd update ran despite the collision:\n%s", log)
	}
}

// TestGcBdWriteGuardDoesNotOpenTheScopeStore pins the cost side of the same
// fix: the guard's read is one bd call, never a store open. On a native-eligible
// Dolt scope that open is a preflight (a bd context fork plus schema probes)
// and a library open, about 1.6 s on a production rig, to answer a question a
// single bd show answers.
func TestGcBdWriteGuardDoesNotOpenTheScopeStore(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, "")
	setCwd(t, cityDir)
	calls := writeBatchGuardBdStub(t, `    "show --json demo-abc")
      printf '[{"id":"demo-abc","status":"open"}]\n' ;;`)
	opens := stubExactStoreFactory(t, beads.Bead{ID: "demo-abc", Title: "t", Status: "open", Type: "task"})

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"update", "demo-abc", "--status", "in_progress"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd update = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got := opens.Load(); got != 0 {
		t.Fatalf("store opens = %d, want 0: the guard reads through bd", got)
	}
	log := readBdCallLog(t, calls)
	if got := strings.Count(log, "show --json demo-abc"); got != 1 {
		t.Fatalf("guard reads = %d, want 1:\n%s", got, log)
	}
	if !strings.Contains(log, "update demo-abc --status in_progress") {
		t.Fatalf("the verified update was not forwarded to bd:\n%s", log)
	}
}

// TestGcBdWriteGuardSkipsTheReadWhereBdResolvesIDsExactly pins the exact-ID
// backends. bd's HTTP store refuses partial-id search, so a write's prefixed
// id either names its bead exactly or fails in bd: the collision the guard
// reads for cannot happen there, and the read is a whole bd round trip
// (~0.45 s against a remote serve) per write. Close-shaped writes keep the
// read, because the close gate and the claim-identity close consume the bead
// itself; so does a write naming an unprefixed id, which bd rewrites
// (TestGcBdWriteGuardReadsAnUnprefixedIDWhereBdResolvesIDsExactly).
func TestGcBdWriteGuardSkipsTheReadWhereBdResolvesIDsExactly(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantReads int
	}{
		{"update", []string{"update", "demo-abc", "--status", "in_progress"}, 0},
		{"update metadata", []string{"update", "demo-abc", "--set-metadata", "k=v"}, 0},
		{"reopen", []string{"reopen", "demo-abc"}, 0},
		{"heartbeat", []string{"heartbeat", "demo-abc"}, 0},
		{"close reads for the gate and claim identity", []string{"close", "demo-abc"}, 1},
		{"update to closed reads for the gate", []string{"update", "demo-abc", "--status", "closed"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cityDir := bdSQLRefusalCityDir(t, "")
			setCwd(t, cityDir)
			// bd's http store refuses partial-id search: ids resolve exactly.
			writeBoundBackendMetadata(t, cityDir, "http")
			calls := writeBatchGuardBdStub(t, `    "show --json demo-abc")
      printf '[{"id":"demo-abc","status":"open"}]\n' ;;`)

			var stdout, stderr bytes.Buffer
			if code := doBd(tc.args, &stdout, &stderr); code != 0 {
				t.Fatalf("doBd(%v) = %d, want 0; stderr=%q", tc.args, code, stderr.String())
			}
			log := readBdCallLog(t, calls)
			if got := strings.Count(log, "show --json demo-abc"); got != tc.wantReads {
				t.Fatalf("guard reads = %d, want %d:\n%s", got, tc.wantReads, log)
			}
			if !strings.Contains(log, strings.Join(tc.args, " ")) {
				t.Fatalf("the write was not forwarded to bd:\n%s", log)
			}
		})
	}
}

// TestGcBdWriteGuardReadsAnUnprefixedIDWhereBdResolvesIDsExactly pins the
// limit of the exact-ID skip. bd's exact-only resolver retries an id that is
// not prefixed (a bare hash) as <issue_prefix>-<id>, so on an http scope
// `update abc` lands on demo-abc, a bead the write never named. The read is
// skipped only when every id the write names is prefixed, and the guard
// refuses the rewrite as it refuses any id bd resolves to a different bead.
func TestGcBdWriteGuardReadsAnUnprefixedIDWhereBdResolvesIDsExactly(t *testing.T) {
	for _, args := range [][]string{
		{"update", "abc", "--status", "in_progress"},
		{"update", "demo-abc", "abc", "demo-def", "--status", "in_progress"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, cityDir := bdSQLRefusalCityDir(t, "")
			setCwd(t, cityDir)
			writeBoundBackendMetadata(t, cityDir, "http")
			calls := writeBatchGuardBdStub(t, `    "show --json demo-abc abc demo-def")
      printf '[{"id":"demo-abc","status":"open"},{"id":"demo-def","status":"open"}]\n' ;;
    "show --json abc")
      printf '[{"id":"demo-abc","status":"open"}]\n' ;;`)

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code != 1 {
				t.Fatalf("doBd(%v) = %d, want 1 for an id bd rewrites to demo-abc; stderr=%q", args, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), `bead "abc" resolved to a different bead ID`) {
				t.Fatalf("stderr = %q, want the refusal to name abc", stderr.String())
			}
			if log := readBdCallLog(t, calls); strings.Contains(log, "update ") {
				t.Fatalf("bd update ran on an id bd rewrites:\n%s", log)
			}
		})
	}
}

// TestGcBdWriteGuardKeepsTheReadWhereBdResolvesFuzzily pins the other side of
// the exact-ID skip: a workspace whose metadata names a backend bd resolves by
// prefix match (Dolt, a bound Postgres, or no marker at all) keeps the guard
// read.
func TestGcBdWriteGuardKeepsTheReadWhereBdResolvesFuzzily(t *testing.T) {
	for _, backend := range []string{"", "dolt", "postgres"} {
		t.Run("backend="+backend, func(t *testing.T) {
			_, cityDir := bdSQLRefusalCityDir(t, "")
			setCwd(t, cityDir)
			writeScopeBackendMetadata(t, cityDir, backend)
			calls := writeBatchGuardBdStub(t, `    "show --json demo-abc")
      printf '[{"id":"demo-abc","status":"open"}]\n' ;;`)

			var stdout, stderr bytes.Buffer
			if code := doBd([]string{"update", "demo-abc", "--status", "in_progress"}, &stdout, &stderr); code != 0 {
				t.Fatalf("doBd update = %d, want 0; stderr=%q", code, stderr.String())
			}
			if got := strings.Count(readBdCallLog(t, calls), "show --json demo-abc"); got != 1 {
				t.Fatalf("guard reads = %d, want 1", got)
			}
		})
	}
}

// TestGcBdWriteGuardCloseKeepsTheClaimIdentityOnExactIDBackends pins why
// close-shaped writes keep the read where bd resolves ids exactly: a session
// closing work it claimed closes it under the identity the claim recorded,
// and that identity comes from the bead the guard read.
func TestGcBdWriteGuardCloseKeepsTheClaimIdentityOnExactIDBackends(t *testing.T) {
	for _, args := range [][]string{
		{"close", "demo-abc"},
		{"update", "demo-abc", "--status", "closed"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, cityDir := bdSQLRefusalCityDir(t, "")
			setCwd(t, cityDir)
			writeBoundBackendMetadata(t, cityDir, "http")
			binDir := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "bd-calls.log")
			script := "#!/bin/sh\nprintf '%s actor=%s\\n' \"$*\" \"${BEADS_ACTOR:-}\" >> " + logPath + "\n" +
				"if [ \"$1\" = show ]; then printf '[{\"id\":\"demo-abc\",\"status\":\"in_progress\",\"assignee\":\"sess-1\"}]\\n'; fi\nexit 0\n"
			if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GC_SESSION_ID", "sess-1")
			t.Setenv("BEADS_ACTOR", "someone-else")

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code != 0 {
				t.Fatalf("doBd(%v) = %d, want 0; stderr=%q", args, code, stderr.String())
			}
			want := strings.Join(args, " ") + " actor=sess-1"
			if log := readBdCallLog(t, logPath); !strings.Contains(log, want) {
				t.Fatalf("bd calls = %q, want the close to run as the claim's identity (%q)", log, want)
			}
		})
	}
}

// guardRigCityDir is bdSQLRefusalCityDir plus one bound rig, r1 (prefix rr),
// whose workspace lives outside the city. cityBackend and rigBackend set each
// workspace's metadata (see writeScopeBackendMetadata).
func guardRigCityDir(t *testing.T, cityBackend, rigBackend string) (cityDir, rigDir string) {
	t.Helper()
	rigDir = t.TempDir()
	_, cityDir = bdSQLRefusalCityDir(t, fmt.Sprintf("\n[[rigs]]\nname = \"r1\"\npath = %q\nprefix = \"rr\"\n", rigDir))
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: rr
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScopeBackendMetadata(t, cityDir, cityBackend)
	writeScopeBackendMetadata(t, rigDir, rigBackend)
	return cityDir, rigDir
}

// bdCallContext is the bd context one bd call ran in.
type bdCallContext struct {
	cwd, rig, beadsDir string
}

// writeScopeLoggingBdStub replaces the fixture's bd with one that logs every
// call's args and bd context, and answers `show` from the case arms given.
func writeScopeLoggingBdStub(t *testing.T, showArms string) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd-calls.log")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$*\" \"$(pwd -P)\" \"${GC_RIG:-}\" \"${BEADS_DIR:-}\" >> " + logPath + "\n" +
		"if [ \"$1\" = show ]; then\n  case \"$*\" in\n" + showArms + "\n  esac\nfi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// bdCallContextsFor returns, in call order, the context of every call in a
// writeScopeLoggingBdStub log whose args are exactly args.
func bdCallContextsFor(log, args string) []bdCallContext {
	var contexts []bdCallContext
	for _, line := range strings.Split(log, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) == 4 && fields[0] == args {
			contexts = append(contexts, bdCallContext{cwd: fields[1], rig: fields[2], beadsDir: fields[3]})
		}
	}
	return contexts
}

// TestGcBdWriteGuardDecidesAndReadsAtTheRigItWrites pins the guard on a rig
// write. Whether to read is the rig workspace's question, not the city's, and
// the read runs in the same bd context (cwd, GC_RIG, BEADS_DIR) as the write
// it guards: a read through the city's context reports a rig id absent, which
// lets a collision through (gcy-g4o).
func TestGcBdWriteGuardDecidesAndReadsAtTheRigItWrites(t *testing.T) {
	cases := []struct {
		name                    string
		cityBackend, rigBackend string
		wantReads               int
	}{
		{"fuzzy rig in an exact-ID city reads", "http", "dolt", 1},
		{"exact-ID rig in a fuzzy city skips", "dolt", "http", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir, rigDir := guardRigCityDir(t, tc.cityBackend, tc.rigBackend)
			setCwd(t, cityDir)
			calls := writeScopeLoggingBdStub(t, `    "show --json rr-abc")
      printf '[{"id":"rr-abc","status":"open"}]\n' ;;`)

			args := []string{"update", "rr-abc", "--status", "in_progress"}
			var stdout, stderr bytes.Buffer
			if code := doBd(append([]string{"--rig", "r1"}, args...), &stdout, &stderr); code != 0 {
				t.Fatalf("doBd --rig r1 %v = %d, want 0; stderr=%q", args, code, stderr.String())
			}
			log := readBdCallLog(t, calls)
			writes := bdCallContextsFor(log, strings.Join(args, " "))
			if len(writes) != 1 {
				t.Fatalf("forwarded writes = %d, want 1:\n%s", len(writes), log)
			}
			write := writes[0]
			if !samePath(write.cwd, rigDir) || write.rig != "r1" || !samePath(write.beadsDir, filepath.Join(rigDir, ".beads")) {
				t.Fatalf("the write ran in %+v, want the rig's bd context (%s, GC_RIG=r1)", write, rigDir)
			}
			reads := bdCallContextsFor(log, "show --json rr-abc")
			if len(reads) != tc.wantReads {
				t.Fatalf("guard reads = %d, want %d:\n%s", len(reads), tc.wantReads, log)
			}
			for _, read := range reads {
				if read != write {
					t.Fatalf("the guard read ran in %+v, the write in %+v: the read must ask the bd the write goes to", read, write)
				}
			}
		})
	}
}
