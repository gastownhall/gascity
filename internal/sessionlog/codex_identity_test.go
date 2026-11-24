package sessionlog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func keyedIdentityTime() time.Time {
	return time.Date(2026, 6, 15, 10, 0, 0, 0, time.Local)
}

func keyedIdentityLine(id, cwd string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`, id, cwd)
}

func writeKeyedIdentityRollout(t *testing.T, root string, at time.Time, key, line string) string {
	t.Helper()
	day := filepath.Join(root, at.Format("2006/01/02"))
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(day, "rollout-"+at.Format("2006-01-02T15-04-05")+"-"+key+".jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertKeyedIdentity(t *testing.T, got CodexKeyedLookup, outcome CodexKeyedOutcome, path string, differs bool) {
	t.Helper()
	want := CodexKeyedLookup{Path: path, Outcome: outcome, CWDDiffers: differs}
	if got != want {
		t.Errorf("keyed lookup = %+v, want %+v", got, want)
	}
}

// K1-K5: the complete first record distinguishes a contradiction from a write
// still in progress. Complete JSON needs no trailing newline.
func TestCodexIdentityFirstLine(t *testing.T) {
	const key = "identity-key"
	cases := []struct {
		name    string
		line    string
		outcome CodexKeyedOutcome
		differs bool
	}{
		{"K1_drift", keyedIdentityLine(key, "/other"), CodexKeyedFound, true},
		{"K2_same_cwd", keyedIdentityLine(key, "/work"), CodexKeyedFound, false},
		{"K2_empty_cwd", keyedIdentityLine(key, ""), CodexKeyedFound, false},
		{"K3_other_id", keyedIdentityLine("another-key", "/work"), CodexKeyedIdentityMismatch, false},
		{"K3_prefix_id", keyedIdentityLine("identity", "/work"), CodexKeyedIdentityMismatch, false},
		{"K3_substring_id", keyedIdentityLine("key", "/work"), CodexKeyedIdentityMismatch, false},
		{"K4_missing_id_same_cwd", `{"type":"session_meta","payload":{"cwd":"/work"}}`, CodexKeyedFound, false},
		{"K4_empty_id_same_cwd", keyedIdentityLine("", "/work"), CodexKeyedFound, false},
		{"K4_missing_id_other_cwd", `{"type":"session_meta","payload":{"cwd":"/other"}}`, CodexKeyedIdentityMismatch, false},
		{"K4_other_type", `{"type":"event_msg","payload":{"id":"identity-key","cwd":"/work"}}`, CodexKeyedIdentityMismatch, false},
		{"K5_empty", "", CodexKeyedUncertain, false},
		{"K5_partial", `{"type":"session_meta","payload":`, CodexKeyedUncertain, false},
		{"K5_garbage", "not-json\n", CodexKeyedUncertain, false},
		{"K5_multiple_json_values", keyedIdentityLine(key, "/work") + ` {}`, CodexKeyedUncertain, false},
		{"K5_over_scanner_cap", fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":"/work","padding":%q}}`, key, strings.Repeat("x", 1<<20)), CodexKeyedUncertain, false},
		{"K5_above_initial_buffer", fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":"/work","padding":%q}}`, key, strings.Repeat("x", 70<<10)), CodexKeyedFound, false},
		{"K5_first_line_only", keyedIdentityLine(key, "/work") + "\nnot-json\n", CodexKeyedFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeKeyedIdentityRollout(t, root, keyedIdentityTime(), key, tc.line)
			wantPath := ""
			if tc.outcome == CodexKeyedFound {
				wantPath = path
			}
			got := FindCodexSessionFileByIdentity([]string{root}, "/work", key, keyedIdentityTime(), keyedIdentityTime())
			assertKeyedIdentity(t, got, tc.outcome, wantPath, tc.differs)
		})
	}
}

// K6-K7 and K10: names are exact identities, never a path to construct or a
// reason to choose a same-directory neighbor.
func TestCodexIdentityPhysicalUniquenessAndExactNames(t *testing.T) {
	for _, name := range []string{"K6_distinct", "K6_symlink", "K7_absent", "K10_neighbor", "K10_prefix", "K10_substring", "K10_separator"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			at := keyedIdentityTime()
			key := "identity-key"
			outcome, want := CodexKeyedAbsent, ""
			switch name {
			case "K6_distinct":
				writeKeyedIdentityRollout(t, root, at, key, keyedIdentityLine(key, "/work"))
				writeKeyedIdentityRollout(t, root, at.AddDate(0, 0, -1), key, keyedIdentityLine(key, "/work"))
				outcome = CodexKeyedAmbiguous
			case "K6_symlink":
				want = writeKeyedIdentityRollout(t, root, at, key, keyedIdentityLine(key, "/work"))
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				got := FindCodexSessionFileByIdentity([]string{root, alias}, "/work", key, at, at)
				assertKeyedIdentity(t, got, CodexKeyedFound, want, false)
				return
			case "K10_neighbor":
				writeKeyedIdentityRollout(t, root, at, "unrelated", keyedIdentityLine("unrelated", "/work"))
			case "K10_prefix":
				writeKeyedIdentityRollout(t, root, at, key+"-long", keyedIdentityLine(key, "/work"))
			case "K10_substring":
				writeKeyedIdentityRollout(t, root, at, "x-"+key+"-x", keyedIdentityLine(key, "/work"))
			case "K10_separator":
				writeKeyedIdentityRollout(t, root, at, key, keyedIdentityLine(key, "/work"))
				key = "../" + key
			}
			assertKeyedIdentity(t, FindCodexSessionFileByIdentity([]string{root}, "/work", key, at, at), outcome, want, false)
		})
	}
}

func TestCodexIdentityInvalidDoesNoIO(t *testing.T) {
	at := keyedIdentityTime()
	for _, tc := range []struct {
		name, cwd, key string
		before, after  time.Time
	}{
		{"K9_empty_key", "/work", "", at, at},
		{"K9_empty_workdir", "", "key", at, at},
		{"K9_whitespace_key", "/work", "  ", at, at},
		{"K9_whitespace_workdir", "  ", "key", at, at},
		{"K9_zero_end", "/work", "key", at, time.Time{}},
		{"K9_reversed_padded_days", "/work", "key", at, at.AddDate(0, 0, -4)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, opens := 0, 0
			got := findCodexSessionFileByIdentityWithReaders([]string{t.TempDir()}, tc.cwd, tc.key, tc.before, tc.after,
				func(string) ([]os.DirEntry, error) { reads++; return nil, os.ErrPermission },
				func(string) (io.ReadCloser, error) { opens++; return nil, os.ErrPermission })
			assertKeyedIdentity(t, got, CodexKeyedInvalid, "", false)
			if reads != 0 || opens != 0 {
				t.Fatalf("invalid lookup performed %d ReadDir, %d opens", reads, opens)
			}
		})
	}
}

// K5 and K8 use function-valued readers, so IO faults do not depend on uid or
// host permissions. A match collected before a later fault is not authoritative.
func TestCodexIdentityReadFaultsAndBounds(t *testing.T) {
	for _, scenario := range []string{"found", "absent", "ambiguous", "root_fault", "late_fault", "open_fault", "vanished", "read_fault"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			at := keyedIdentityTime()
			const key = "bounded-key"
			path := writeKeyedIdentityRollout(t, root, at, key, keyedIdentityLine(key, "/work"))
			search := []string{root}
			outcome, wantOpens, wantPath := CodexKeyedFound, 1, path
			switch scenario {
			case "absent":
				keyPathRemove(t, path)
				outcome, wantOpens, wantPath = CodexKeyedAbsent, 0, ""
			case "ambiguous":
				writeKeyedIdentityRollout(t, root, at.AddDate(0, 0, -1), key, keyedIdentityLine(key, "/work"))
				outcome, wantOpens, wantPath = CodexKeyedAmbiguous, 0, ""
			case "root_fault", "late_fault":
				outcome, wantOpens, wantPath = CodexKeyedUncertain, 0, ""
			case "open_fault", "vanished", "read_fault":
				outcome, wantPath = CodexKeyedUncertain, ""
			}
			faultRoot := filepath.Join(t.TempDir(), "fault")
			if scenario == "late_fault" {
				search = append(search, faultRoot)
			}
			reads, opens := 0, 0
			readDir := func(p string) ([]os.DirEntry, error) {
				reads++
				if scenario == "root_fault" || p == faultRoot {
					return nil, os.ErrPermission
				}
				return os.ReadDir(p)
			}
			open := func(p string) (io.ReadCloser, error) {
				opens++
				if scenario == "open_fault" {
					return nil, os.ErrPermission
				}
				if scenario == "vanished" {
					return nil, os.ErrNotExist
				}
				if scenario == "read_fault" {
					return io.NopCloser(keyedErrorReader{}), nil
				}
				return os.Open(p)
			}
			got := findCodexSessionFileByIdentityWithReaders(search, "/work", key, at, at, readDir, open)
			assertKeyedIdentity(t, got, outcome, wantPath, false)
			if opens != wantOpens {
				t.Errorf("opens = %d, want %d", opens, wantOpens)
			}
			oldReads := 0
			findCodexSessionFilesByIDWithReadDir(search, []CodexSessionTarget{{Key: "probe", WorkDir: "/work", SessionID: key, NotBefore: at, NotAfter: at}}, func(p string) ([]os.DirEntry, error) {
				oldReads++
				if scenario == "root_fault" || p == faultRoot {
					return nil, os.ErrPermission
				}
				return os.ReadDir(p)
			})
			if reads == 0 || reads > oldReads {
				t.Errorf("ReadDir = %d, old finder %d", reads, oldReads)
			}
		})
	}
}

func keyPathRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

type keyedErrorReader struct{}

func (keyedErrorReader) Read([]byte) (int, error) { return 0, errors.New("injected read fault") }

func TestCodexIdentityReadDirBudget(t *testing.T) {
	at := keyedIdentityTime()
	// Each absent search root costs one read. Virtual roots avoid thousands of
	// fixtures and intentionally exercise the request-wide bound.
	roots := make([]string, codexBatchRequestReadDirCap+1)
	parent := t.TempDir()
	for i := range roots {
		roots[i] = filepath.Join(parent, fmt.Sprintf("root-%d", i))
	}
	reads, opens := 0, 0
	got := findCodexSessionFileByIdentityWithReaders(roots, "/work", "budget-key", at, at,
		func(string) ([]os.DirEntry, error) { reads++; return nil, os.ErrNotExist },
		func(string) (io.ReadCloser, error) { opens++; return nil, os.ErrPermission })
	assertKeyedIdentity(t, got, CodexKeyedUncertain, "", false)
	if reads != 4096 || opens != 0 {
		t.Fatalf("budget used %d ReadDir, %d opens, want 4096/0", reads, opens)
	}
}

// Z6 pins the existing caps, including the disjoint UUID creation-day hint.
func TestCodexIdentityDayCaps(t *testing.T) {
	if codexBatchRequestDayDirCap != 375 || codexBatchRequestReadDirCap != 4096 || codexByIDDayDirCap != 370 {
		t.Fatal("Z6: keyed discovery caps changed")
	}
	at := keyedIdentityTime()
	states := buildCodexBatchTargets([]CodexSessionTarget{{Key: "cap", WorkDir: "/work", SessionID: codexBatchUUIDv7At(at.AddDate(-4, 0, 0)), NotBefore: at.AddDate(-2, 0, 0), NotAfter: at}})
	if len(states) != 1 {
		t.Fatalf("admitted targets = %d, want 1", len(states))
	}
	if days := len(codexBatchTargetDays(&states[0])); days != 375 {
		t.Fatalf("planned days = %d, want 375", days)
	}
}

// Z2: the filesystem fake records mutation calls as well as reads. The real
// filesystem cases above separately cover physical symlink identity.
func TestCodexIdentityDiscoveryIsReadOnly(t *testing.T) {
	root := t.TempDir()
	at := keyedIdentityTime()
	path := filepath.Join(root, at.Format("2006/01/02"), "rollout-"+at.Format("2006-01-02T15-04-05")+"-readonly-key.jsonl")
	fs := fsys.NewFake()
	fs.Files[path] = []byte(keyedIdentityLine("readonly-key", "/work"))
	before := maps.Clone(fs.Files)
	got := findCodexSessionFileByIdentityWithReaders([]string{root}, "/work", "readonly-key", at, at, fs.ReadDir,
		func(p string) (io.ReadCloser, error) {
			data, err := fs.ReadFile(p)
			return io.NopCloser(bytes.NewReader(data)), err
		})
	assertKeyedIdentity(t, got, CodexKeyedFound, path, false)
	for _, call := range fs.Calls {
		if call.Method != "ReadDir" && call.Method != "ReadFile" {
			t.Errorf("discovery mutated filesystem: %+v", call)
		}
	}
	if !maps.EqualFunc(before, fs.Files, bytes.Equal) {
		t.Fatal("discovery changed rollout files")
	}
}
