//go:build !windows

package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The recursion fuse (ga-kihcmff, incident ga-2t6u9dy): a `bd` on PATH that
// calls `gc bd` makes gc launch bd again, forever. Every Go path that launches
// a bd binary stamps GC_BD_DEPTH on the child and refuses at MaxBdDepth. These
// tests pin the helper itself and the three launch seams in this package (the
// runner, Purge, and the version probe).

// setProcessBdDepth makes GC_BD_DEPTH read as value in this process for the
// test; set=false leaves it genuinely unset (not merely empty).
func setProcessBdDepth(t *testing.T, value string, set bool) {
	t.Helper()
	t.Setenv(BdDepthEnvKey, value)
	if !set {
		if err := os.Unsetenv(BdDepthEnvKey); err != nil {
			t.Fatal(err)
		}
	}
}

// lastEnvEntry returns the last KEY=value entry for key, which is the one a
// child process sees.
func lastEnvEntry(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix), true
		}
	}
	return "", false
}

func TestBdDepthConstants(t *testing.T) {
	if BdDepthEnvKey != "GC_BD_DEPTH" {
		t.Fatalf("BdDepthEnvKey = %q, want GC_BD_DEPTH", BdDepthEnvKey)
	}
	if MaxBdDepth != 16 {
		t.Fatalf("MaxBdDepth = %d, want 16", MaxBdDepth)
	}
}

func TestGuardBdLaunchDepthTable(t *testing.T) {
	const bdPath = "/opt/stub/bd"
	cases := []struct {
		name      string
		value     string
		set       bool
		wantStamp string // empty: the launch must be refused
		malformed bool
	}{
		{name: "unset", set: false, wantStamp: "1"},
		{name: "empty", value: "", set: true, wantStamp: "1"},
		{name: "zero", value: "0", set: true, wantStamp: "1"},
		{name: "one", value: "1", set: true, wantStamp: "2"},
		{name: "fifteen allowed, stamps the bound", value: "15", set: true, wantStamp: "16"},
		{name: "sixteen refused", value: "16", set: true},
		{name: "seventeen refused", value: "17", set: true},
		{name: "huge in-range refused", value: "1000000", set: true},
		{name: "negative", value: "-1", set: true, malformed: true},
		{name: "plus sign", value: "+1", set: true, malformed: true},
		{name: "leading space", value: " 1", set: true, malformed: true},
		{name: "trailing space", value: "1 ", set: true, malformed: true},
		{name: "hex", value: "0x10", set: true, malformed: true},
		{name: "letters", value: "abc", set: true, malformed: true},
		{name: "exponent", value: "1e1", set: true, malformed: true},
		{name: "decimal point", value: "1.0", set: true, malformed: true},
		{name: "overflow uint64", value: "99999999999999999999999", set: true, malformed: true},
		{name: "non-ASCII digit", value: "٣", set: true, malformed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setProcessBdDepth(t, tc.value, tc.set)
			env, err := GuardBdLaunch(bdPath, []string{"PATH=/usr/bin"})
			if tc.wantStamp != "" {
				if err != nil {
					t.Fatalf("GuardBdLaunch: unexpected error %v", err)
				}
				got, ok := lastEnvEntry(env, BdDepthEnvKey)
				if !ok || got != tc.wantStamp {
					t.Fatalf("stamp = %q (present=%v), want %q; env=%v", got, ok, tc.wantStamp, env)
				}
				return
			}
			if err == nil {
				t.Fatalf("GuardBdLaunch succeeded with env %v, want a refusal", env)
			}
			if env != nil {
				t.Errorf("env = %v on refusal, want nil", env)
			}
			var depthErr *BdDepthError
			if !errors.As(err, &depthErr) {
				t.Fatalf("error %T %q is not a *BdDepthError", err, err)
			}
			msg := err.Error()
			if !strings.Contains(msg, BdDepthEnvKey) {
				t.Errorf("error %q does not name %s", msg, BdDepthEnvKey)
			}
			if tc.malformed {
				if !strings.Contains(msg, strconv.Quote(tc.value)) {
					t.Errorf("malformed-value error %q does not quote the value %q", msg, tc.value)
				}
				return
			}
			for _, want := range []string{bdPath, tc.value, strconv.Itoa(MaxBdDepth)} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not name %q (resolved bd path, depth, bound)", msg, want)
				}
			}
		})
	}
}

// A refusal is final: neither the write retry nor the read retry may treat it
// as transient, or a BdStore operation at the bound would launch and refuse
// several times instead of once.
func TestGuardBdLaunchRefusalIsNotClassifiedTransient(t *testing.T) {
	for _, value := range []string{"16", "999", "-1", "nope"} {
		setProcessBdDepth(t, value, true)
		_, err := GuardBdLaunch("/opt/stub/bd", nil)
		if err == nil {
			t.Fatalf("GuardBdLaunch with %s=%q succeeded, want a refusal", BdDepthEnvKey, value)
		}
		if isBdTransientWriteError(err) {
			t.Errorf("%s=%q: refusal %q is classified as a transient write error (it would be retried)", BdDepthEnvKey, value, err)
		}
		if isBdAmbiguousWriteError(err) {
			t.Errorf("%s=%q: refusal %q is classified as an ambiguous write error (it would be retried)", BdDepthEnvKey, value, err)
		}
	}
}

func TestGuardBdLaunchNilEnvInheritsProcessEnvironment(t *testing.T) {
	setProcessBdDepth(t, "3", true)
	t.Setenv("GC_BD_DEPTH_TEST_MARKER", "kept")
	env, err := GuardBdLaunch("/opt/stub/bd", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env == nil {
		t.Fatal("env = nil: a nil child env means inherit-all, so the stamp would be dropped")
	}
	if got, _ := lastEnvEntry(env, "GC_BD_DEPTH_TEST_MARKER"); got != "kept" {
		t.Errorf("materialized env lost the inherited marker: got %q", got)
	}
	if got, _ := lastEnvEntry(env, "PATH"); got != os.Getenv("PATH") {
		t.Errorf("materialized env PATH = %q, want the process PATH", got)
	}
	if got, _ := lastEnvEntry(env, BdDepthEnvKey); got != "4" {
		t.Errorf("stamp = %q, want 4", got)
	}
	if last := env[len(env)-1]; last != BdDepthEnvKey+"=4" {
		t.Errorf("last entry = %q, want the stamp %s=4", last, BdDepthEnvKey)
	}
}

// The depth comes from this process, never from the child env being composed:
// a stale or hostile GC_BD_DEPTH in a caller's env must not reset or inflate
// the count, and the stamp must win by being last.
func TestGuardBdLaunchReadsProcessDepthNotChildEnvAndStampsLast(t *testing.T) {
	setProcessBdDepth(t, "3", true)
	in := make([]string, 0, 8)
	in = append(in, "A=1", BdDepthEnvKey+"=99", "B=2")
	before := append([]string(nil), in...)
	env, err := GuardBdLaunch("/opt/stub/bd", in)
	if err != nil {
		t.Fatalf("GuardBdLaunch refused on a child-env depth of 99 while the process depth is 3: %v", err)
	}
	if last := env[len(env)-1]; last != BdDepthEnvKey+"=4" {
		t.Errorf("last entry = %q, want %s=4", last, BdDepthEnvKey)
	}
	if len(env) != len(before)+1 {
		t.Errorf("env = %v, want the input plus exactly one appended stamp", env)
	}
	for i, want := range before {
		if in[i] != want {
			t.Errorf("input env mutated at %d: %q, want %q", i, in[i], want)
		}
	}
	if len(in) != len(before) {
		t.Errorf("input env length changed to %d", len(in))
	}
	// A spare-capacity input must not have been appended into in place.
	if cap(in) > len(in) {
		if spare := in[:len(in)+1][len(in)]; spare != "" {
			t.Errorf("stamp %q was written into the caller's spare capacity", spare)
		}
	}
}

func TestGuardBdLaunchDoesNotWriteProcessEnvironment(t *testing.T) {
	setProcessBdDepth(t, "3", true)
	if _, err := GuardBdLaunch("/opt/stub/bd", nil); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(BdDepthEnvKey); got != "3" {
		t.Errorf("process %s = %q after a guarded launch, want it untouched (3)", BdDepthEnvKey, got)
	}
}

// writeDepthStub installs a bd on PATH that appends the GC_BD_DEPTH it was
// launched with to a log, and returns the stub's absolute path and the log.
func writeDepthStub(t *testing.T) (stub, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(t.TempDir(), "stub.log")
	stub = filepath.Join(dir, "bd")
	script := "#!/bin/sh\nprintf 'depth=%s\\n' \"$GC_BD_DEPTH\" >> '" + log + "'\necho '[]'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stub, log
}

func stubRuns(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestExecCommandRunnerStampsDepthOnBdChild(t *testing.T) {
	cases := map[string]func() CommandRunner{
		"merged env": func() CommandRunner { return ExecCommandRunnerWithEnv(map[string]string{"GC_TEST_X": "1"}) },
		"exact env": func() CommandRunner {
			return ExecCommandRunnerWithExactEnvContext(context.Background(), map[string]string{"GC_TEST_X": "1"})
		},
		"without ambient beads": func() CommandRunner {
			return ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{"GC_TEST_X": "1"})
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			setProcessBdDepth(t, "3", true)
			_, log := writeDepthStub(t)
			if _, err := mk()(t.TempDir(), "bd", "list", "--json"); err != nil {
				t.Fatalf("runner at depth 3: %v", err)
			}
			if got := stubRuns(t, log); len(got) != 1 || got[0] != "depth=4" {
				t.Fatalf("stub saw %v, want exactly one run with depth=4", got)
			}
		})
	}
}

func TestExecCommandRunnerRefusesBdLaunchAtTheBound(t *testing.T) {
	cases := map[string]func() CommandRunner{
		"merged env": func() CommandRunner { return ExecCommandRunnerWithEnv(nil) },
		"exact env": func() CommandRunner {
			return ExecCommandRunnerWithExactEnvContext(context.Background(), map[string]string{"GC_TEST_X": "1"})
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			setProcessBdDepth(t, "16", true)
			stub, log := writeDepthStub(t)
			_, err := mk()(t.TempDir(), "bd", "list", "--json")
			if err == nil {
				t.Fatal("runner launched bd at depth 16, want a refusal")
			}
			var depthErr *BdDepthError
			if !errors.As(err, &depthErr) {
				t.Fatalf("error %T %q is not a *BdDepthError", err, err)
			}
			if !strings.Contains(err.Error(), BdDepthEnvKey) || !strings.Contains(err.Error(), "16") {
				t.Errorf("error %q must name %s and the depth", err, BdDepthEnvKey)
			}
			if runs := stubRuns(t, log); len(runs) != 0 {
				t.Fatalf("stub %s ran %d time(s) at the bound: %v", stub, len(runs), runs)
			}
		})
	}
}

// The runner honors a BD_BIN pin by launching the pinned absolute path; the
// guard must cover that launch and name the pinned path, not the bare "bd".
func TestExecCommandRunnerRefusalNamesThePinnedBdBinary(t *testing.T) {
	setProcessBdDepth(t, "16", true)
	stub, log := writeDepthStub(t)
	runner := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": stub})
	_, err := runner(t.TempDir(), "bd", "list")
	if err == nil {
		t.Fatal("runner launched the pinned bd at depth 16, want a refusal")
	}
	if !strings.Contains(err.Error(), stub) {
		t.Errorf("error %q does not name the resolved pinned path %q", err, stub)
	}
	if runs := stubRuns(t, log); len(runs) != 0 {
		t.Fatalf("pinned stub ran at the bound: %v", runs)
	}
}

// Only bd launches carry the counter: another program launched through the
// same runner is neither stamped nor refused.
func TestExecCommandRunnerLeavesNonBdChildrenAlone(t *testing.T) {
	setProcessBdDepth(t, "16", true)
	out, err := ExecCommandRunnerWithEnv(nil)(t.TempDir(), "env")
	if err != nil {
		t.Fatalf("non-bd launch at depth 16 was refused: %v", err)
	}
	if !strings.Contains(string(out), "GC_BD_DEPTH=16\n") {
		t.Errorf("non-bd child env = %q, want GC_BD_DEPTH inherited unchanged at 16", out)
	}
}

// A BdStore operation at the bound makes exactly one attempt: neither retry
// loop may spin on the refusal.
func TestBdStoreRetryLoopsMakeOneAttemptAtTheBound(t *testing.T) {
	setProcessBdDepth(t, "16", true)
	_, log := writeDepthStub(t)
	inner := ExecCommandRunnerWithEnv(nil)
	attempts := 0
	counting := func(dir, name string, args ...string) ([]byte, error) {
		attempts++
		return inner(dir, name, args...)
	}
	s := NewBdStore(t.TempDir(), counting)

	if _, err := s.runBDTransientRead("list", "--json"); err == nil {
		t.Fatal("read at the bound succeeded, want a refusal")
	}
	if attempts != 1 {
		t.Errorf("read retry loop made %d attempts at the bound, want exactly 1", attempts)
	}

	attempts = 0
	_, err := s.runBDTransientWriteOutputWhen(isBdTransientWriteError, "update", "x-1", "--status", "open")
	if err == nil {
		t.Fatal("write at the bound succeeded, want a refusal")
	}
	if attempts != 1 {
		t.Errorf("write retry loop made %d attempts at the bound, want exactly 1", attempts)
	}
	if runs := stubRuns(t, log); len(runs) != 0 {
		t.Fatalf("stub ran at the bound: %v", runs)
	}
}

func TestExecPurgeStampsDepthAndRefusesAtTheBound(t *testing.T) {
	t.Run("stamps", func(t *testing.T) {
		setProcessBdDepth(t, "3", true)
		_, log := writeDepthStub(t)
		if _, err := execPurge(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, []string{"purge", "--json"}); err != nil {
			t.Fatalf("execPurge at depth 3: %v", err)
		}
		if got := stubRuns(t, log); len(got) != 1 || got[0] != "depth=4" {
			t.Fatalf("stub saw %v, want one run with depth=4", got)
		}
	})
	t.Run("refuses", func(t *testing.T) {
		setProcessBdDepth(t, "16", true)
		_, log := writeDepthStub(t)
		_, err := execPurge(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, []string{"purge", "--json"})
		if err == nil || !strings.Contains(err.Error(), BdDepthEnvKey) {
			t.Fatalf("execPurge at depth 16 error = %v, want a refusal naming %s", err, BdDepthEnvKey)
		}
		if runs := stubRuns(t, log); len(runs) != 0 {
			t.Fatalf("stub ran at the bound: %v", runs)
		}
	})
}

func TestProbeBinaryVersionGuardsOnlyBd(t *testing.T) {
	t.Run("bd is stamped", func(t *testing.T) {
		setProcessBdDepth(t, "3", true)
		stub, log := writeDepthStub(t)
		if _, err := probeBinaryVersion("bd", stub, nil); err != nil {
			t.Fatalf("probe at depth 3: %v", err)
		}
		if got := stubRuns(t, log); len(got) != 1 || got[0] != "depth=4" {
			t.Fatalf("stub saw %v, want one run with depth=4", got)
		}
	})
	t.Run("bd is refused at the bound", func(t *testing.T) {
		setProcessBdDepth(t, "16", true)
		stub, log := writeDepthStub(t)
		_, err := probeBinaryVersion("bd", stub, nil)
		if err == nil || !strings.Contains(err.Error(), BdDepthEnvKey) {
			t.Fatalf("probe at depth 16 error = %v, want a refusal naming %s", err, BdDepthEnvKey)
		}
		if runs := stubRuns(t, log); len(runs) != 0 {
			t.Fatalf("stub ran at the bound: %v", runs)
		}
	})
	t.Run("another program is untouched", func(t *testing.T) {
		setProcessBdDepth(t, "16", true)
		stub, log := writeDepthStub(t)
		if _, err := probeBinaryVersion("dolt", stub, nil); err != nil {
			t.Fatalf("non-bd probe at depth 16 was refused: %v", err)
		}
		if got := stubRuns(t, log); len(got) != 1 || got[0] != "depth=16" {
			t.Fatalf("stub saw %v, want one run with the inherited depth=16", got)
		}
	})
}
