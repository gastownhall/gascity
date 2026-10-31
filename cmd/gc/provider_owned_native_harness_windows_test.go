//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// nativeBdTest runs gc's native gc-beads-bd provider against the recording
// fake bd (provider_owned_fake_bd_test.go), in a scratch city.
type nativeBdTest struct {
	t       *testing.T
	root    string
	city    string
	log     string
	bdBin   string
	replies map[string]nativeFakeBdReply
}

func newNativeBdTest(t *testing.T) *nativeBdTest {
	t.Helper()
	bdBin := nativeFakeBd(t)
	root := t.TempDir()
	city := filepath.Join(root, "city")
	if err := os.MkdirAll(city, 0o755); err != nil {
		t.Fatal(err)
	}
	return &nativeBdTest{
		t:       t,
		root:    root,
		city:    city,
		log:     filepath.Join(root, "bd-calls.jsonl"),
		bdBin:   bdBin,
		replies: map[string]nativeFakeBdReply{},
	}
}

// forSubtest is n bound to a subtest's t, sharing n's city and fake bd log.
// A helper must fail the test that is running it: failing the parent from a
// subtest's goroutine stops every later subtest from running at all.
func (n *nativeBdTest) forSubtest(t *testing.T) *nativeBdTest {
	sub := *n
	sub.t = t
	return &sub
}

// nativeDirectLocalScope is a direct scope whose Dolt bd itself starts.
var nativeDirectLocalScope = map[string]string{
	"metadata.json": `{"backend":"dolt","database":"dolt","dolt_mode":"server"}`,
	"config.yaml":   "gc.endpoint_origin: city_canonical\ndolt.auto-start: true\n",
}

// scope creates the scope rel (the city itself when empty) with files in
// its .beads directory.
func (n *nativeBdTest) scope(rel string, files map[string]string) string {
	n.t.Helper()
	dir := n.city
	if rel != "" {
		dir = filepath.Join(n.city, rel)
	}
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		n.t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(beadsDir, name), []byte(content), 0o644); err != nil {
			n.t.Fatal(err)
		}
	}
	return dir
}

// environ is the provider environment for scope: the ambient environment
// without any gc, beads or bd setting, then what gc hands a provider-owned op,
// then extra.
func (n *nativeBdTest) environ(scope string, extra ...string) []string {
	n.t.Helper()
	var env []string
	for _, entry := range os.Environ() {
		name := strings.ToUpper(nativeEnvName(entry))
		if strings.HasPrefix(name, "GC_") || strings.HasPrefix(name, "BEADS_") || strings.HasPrefix(name, "BD_") {
			continue
		}
		env = append(env, entry)
	}
	replies, err := json.Marshal(n.replies)
	if err != nil {
		n.t.Fatal(err)
	}
	env = append(env,
		"GC_CITY_PATH="+n.city,
		"GC_BEADS_PROVIDER_OWNED=1",
		"BEADS_DIR="+filepath.Join(scope, ".beads"),
		"BD_BIN="+n.bdBin,
		nativeFakeBdLogEnv+"="+n.log,
		nativeFakeBdRepliesEnv+"="+string(replies),
	)
	return append(env, extra...)
}

func (n *nativeBdTest) run(environ []string, args ...string) (string, error) {
	n.t.Helper()
	var stderr bytes.Buffer
	err := runNativeBdProviderOwnedOp(context.Background(), environ, &stderr, args)
	return stderr.String(), err
}

// calls is every bd run since the last reset, in order.
func (n *nativeBdTest) calls() []nativeFakeBdCall {
	n.t.Helper()
	data, err := os.ReadFile(n.log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		n.t.Fatal(err)
	}
	var calls []nativeFakeBdCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var call nativeFakeBdCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			n.t.Fatalf("fake bd record %q: %v", line, err)
		}
		calls = append(calls, call)
	}
	return calls
}

func (n *nativeBdTest) reset() {
	n.t.Helper()
	if err := os.Remove(n.log); err != nil && !errors.Is(err, fs.ErrNotExist) {
		n.t.Fatal(err)
	}
}

// requireArgs fails unless bd ran exactly want, in order.
func (n *nativeBdTest) requireArgs(want ...[]string) []nativeFakeBdCall {
	n.t.Helper()
	calls := n.calls()
	got := make([][]string, 0, len(calls))
	for _, call := range calls {
		got = append(got, call.Args)
	}
	if len(want) == 0 && len(got) == 0 {
		return calls
	}
	if len(got) != len(want) {
		n.t.Fatalf("bd ran %q, want %q", got, want)
	}
	for i := range want {
		if strings.Join(got[i], "\x00") != strings.Join(want[i], "\x00") {
			n.t.Fatalf("bd ran %q, want %q", got, want)
		}
	}
	return calls
}

// callEnv looks name up in what bd saw, as Windows does: without case, last
// definition winning.
func callEnv(call nativeFakeBdCall, name string) (string, bool) {
	for i := len(call.Env) - 1; i >= 0; i-- {
		key := nativeEnvName(call.Env[i])
		if strings.EqualFold(key, name) {
			if len(key) < len(call.Env[i]) {
				return call.Env[i][len(key)+1:], true
			}
			return "", true
		}
	}
	return "", false
}

func requireCallEnv(t *testing.T, call nativeFakeBdCall, name, want string) {
	t.Helper()
	if got, ok := callEnv(call, name); !ok || got != want {
		t.Fatalf("bd %q saw %s=%q (set %v), want %q", call.Args, name, got, ok, want)
	}
}

func requireCallEnvAbsent(t *testing.T, call nativeFakeBdCall, names ...string) {
	t.Helper()
	for _, name := range names {
		if got, ok := callEnv(call, name); ok {
			t.Fatalf("bd %q saw %s=%q, want it unset", call.Args, name, got)
		}
	}
}

func requireCallDir(t *testing.T, call nativeFakeBdCall, want string) {
	t.Helper()
	if !samePath(call.Dir, want) {
		t.Fatalf("bd %q ran in %q, want %q", call.Args, call.Dir, want)
	}
}

// requireScriptExit fails unless err is the script's own refusal with status.
func requireScriptExit(t *testing.T, err error, status int) {
	t.Helper()
	var exit nativeProviderExit
	if !errors.As(err, &exit) || int(exit) != status {
		t.Fatalf("error = %v (%T), want the script's own exit status %d", err, err, status)
	}
}

var (
	nativeFakeBdOnce sync.Once
	nativeFakeBdPath string
	nativeFakeBdErr  error
)

// nativeFakeBd is the recording fake as bd.exe: one copy of this test binary,
// shared by the run and removed with its fixture root.
//
// Both halves matter. internal/testenv scrubs BEADS_DIR, GC_CITY_PATH and the
// Dolt endpoint variables from any process named like a test binary, which is
// the very environment the fake is there to record; under another name it
// keeps them. And a copy, unlike a hard link, is not the running image, so
// Windows lets the run delete it.
func nativeFakeBd(t *testing.T) string {
	t.Helper()
	nativeFakeBdOnce.Do(func() {
		if sharedTestFixtureRoot == "" {
			nativeFakeBdErr = errors.New("no shared fixture root; TestMain did not run")
			return
		}
		exe, err := os.Executable()
		if err != nil {
			nativeFakeBdErr = err
			return
		}
		dst := filepath.Join(sharedTestFixtureRoot, "native-fake-bd", "bd.exe")
		if err := copyExecutable(exe, dst); err != nil {
			nativeFakeBdErr = err
			return
		}
		nativeFakeBdPath = dst
	})
	if nativeFakeBdErr != nil {
		t.Fatalf("native fake bd: %v", nativeFakeBdErr)
	}
	return nativeFakeBdPath
}

func copyExecutable(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck
		return err
	}
	return out.Close()
}

// writeDecoyBd puts a bd.exe in dir that is not a program, so a lookup that
// finds it fails to start rather than recording a run.
func writeDecoyBd(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bd.exe"), []byte("not a program\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// withoutEnvNames drops names from environ, without case.
func withoutEnvNames(environ []string, names ...string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		key := nativeEnvName(entry)
		dropped := false
		for _, name := range names {
			if strings.EqualFold(key, name) {
				dropped = true
			}
		}
		if !dropped {
			out = append(out, entry)
		}
	}
	return out
}
