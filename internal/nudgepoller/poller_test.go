package nudgepoller

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommandArgsMatchCmdlineMatcher(t *testing.T) {
	cityPath := "/tmp/gc-city"
	sessionName := "sess-worker"
	agentName := "agent"

	argv := append([]string{"gc"}, CommandArgs(cityPath, sessionName, agentName)...)
	if !CmdlineMatcher(cityPath, sessionName, agentName)(argv) {
		t.Fatalf("CmdlineMatcher did not match CommandArgs argv: %v", argv)
	}
}

func TestCmdlineMatcherRejectsWrongOwnership(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city", "--session", "sess-worker", "agent"}
	cases := []struct {
		name        string
		cityPath    string
		sessionName string
		agentName   string
	}{
		{name: "empty city", cityPath: "", sessionName: "sess-worker", agentName: "agent"},
		{name: "empty session", cityPath: "/tmp/gc-city", sessionName: "", agentName: "agent"},
		{name: "empty target", cityPath: "/tmp/gc-city", sessionName: "sess-worker", agentName: ""},
		{name: "wrong city", cityPath: "/tmp/other-city", sessionName: "sess-worker", agentName: "agent"},
		{name: "wrong session", cityPath: "/tmp/gc-city", sessionName: "other-session", agentName: "agent"},
		{name: "wrong target", cityPath: "/tmp/gc-city", sessionName: "sess-worker", agentName: "session-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if CmdlineMatcher(tc.cityPath, tc.sessionName, tc.agentName)(argv) {
				t.Fatalf("CmdlineMatcher(%q, %q, %q) matched %v", tc.cityPath, tc.sessionName, tc.agentName, argv)
			}
		})
	}
}

func TestCmdlineMatcherAcceptsFlagEqualsForm(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--session=sess-worker", "--city=/tmp/gc-city", "agent"}
	if !CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match equals-form flags: %v", argv)
	}
}

func TestArgvHasPollTargetEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{
			name: "generated target after flags",
			argv: []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city", "--session", "sess-worker", "agent"},
			want: true,
		},
		{
			name: "target before flags",
			argv: []string{"gc", "nudge", "poll", "agent", "--city", "/tmp/gc-city", "--session", "sess-worker"},
			want: true,
		},
		{
			name: "known space form flags before target",
			argv: []string{"gc", "nudge", "poll", "--interval", "1s", "--quiescence", "2s", "agent"},
			want: true,
		},
		{
			name: "known equals form flags before target",
			argv: []string{"gc", "nudge", "poll", "--interval=1s", "--quiescence=2s", "agent"},
			want: true,
		},
		{
			name: "unknown space form flag skips value",
			argv: []string{"gc", "nudge", "poll", "--future", "value", "agent"},
			want: true,
		},
		{
			name: "unknown equals form flag before target",
			argv: []string{"gc", "nudge", "poll", "--future=value", "agent"},
			want: true,
		},
		{
			name: "first positional must be target",
			argv: []string{"gc", "nudge", "poll", "other", "agent"},
			want: false,
		},
		{
			name: "no target",
			argv: []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argvHasPollTarget(tc.argv, "agent"); got != tc.want {
				t.Fatalf("argvHasPollTarget(%v, agent) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestCmdlineMatcherNormalizesCityPath(t *testing.T) {
	cityPath := t.TempDir()
	argv := []string{"gc", "nudge", "poll", "--city", cityPath, "--session", "sess-worker", "agent"}
	if !CmdlineMatcher(filepath.Join(cityPath, "."), "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match equivalent city path spelling: %v", argv)
	}
}

func TestCmdlineMatcherAcceptsAnyMatchingCityFlag(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/other-city", "--city=/tmp/gc-city", "--session", "sess-worker", "agent"}
	if !CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match later city flag: %v", argv)
	}
}

func TestPollerFileStemSanitizesSessionPrefix(t *testing.T) {
	stem := PollerFileStem(" ../sess worker/one ", "target")
	if !strings.HasPrefix(stem, "sess-worker-one-") {
		t.Fatalf("PollerFileStem prefix = %q, want sanitized session prefix", stem)
	}
	if strings.ContainsAny(stem, `/\ `) {
		t.Fatalf("PollerFileStem = %q, want filesystem-safe stem", stem)
	}
}

func TestPollerFileStemUsesFallbackPrefixForEmptySession(t *testing.T) {
	stem := PollerFileStem("   ", "target")
	if !strings.HasPrefix(stem, "session-") {
		t.Fatalf("PollerFileStem empty session prefix = %q, want session-*", stem)
	}
}

func TestPollerFileStemTruncatesLongSessionPrefix(t *testing.T) {
	stem := PollerFileStem(strings.Repeat("a", 60), "target")
	prefix, _, ok := strings.Cut(stem, "-")
	if !ok {
		t.Fatalf("PollerFileStem = %q, want prefix and digest", stem)
	}
	if len(prefix) != 48 {
		t.Fatalf("PollerFileStem prefix length = %d, want 48", len(prefix))
	}
}

func TestPollerFileStemDistinguishesSessionTargetTuples(t *testing.T) {
	if PollerFileStem("ab", "c") == PollerFileStem("a", "bc") {
		t.Fatal("PollerFileStem returned the same stem for distinct session/target tuples")
	}
}

func TestSafeFileStemPartTrimsUnsafeEdges(t *testing.T) {
	if got := safeFileStemPart(" ../worker session/. "); got != "worker-session" {
		t.Fatalf("safeFileStemPart() = %q, want worker-session", got)
	}
}

func TestCmdlineMatcherRequiresNudgePollCommand(t *testing.T) {
	argv := []string{"gc", "nudge", "--city", "/tmp/gc-city", "poll", "--session", "sess-worker", "agent"}
	if CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher matched non-contiguous nudge poll argv: %v", argv)
	}
}

// TestIsGoTestExecutableMatchesWindowsTestBinaryName pins the regression that
// let a test run fork-bomb the host. `go test` emits <pkg>.test on Unix and
// <pkg>.test.exe on Windows; a guard that only checked for a trailing ".test"
// never fired on Windows, so the test binary re-executed itself as a poller,
// ran its whole suite again, and spawned again from there.
func TestIsGoTestExecutableMatchesWindowsTestBinaryName(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		want bool
	}{
		{name: "unix test binary", path: "/tmp/build/api.test", want: true},
		{name: "windows test binary", path: `C:\build\api.test.exe`, want: true},
		{name: "windows test binary mixed case suffix", path: `C:\build\api.test.EXE`, want: true},
		// The stem is what the old guard missed: it trimmed ".EXE" case
		// insensitively but then compared ".test" exactly, so the very
		// spelling its comment called out still slipped through.
		{name: "windows test binary mixed case stem", path: `C:\build\Api.Test.EXE`, want: true},
		{name: "windows test binary upper case stem", path: `C:\build\API.TEST.EXE`, want: true},
		// Unix filenames are case sensitive, so "Api.Test" is a different
		// file from "api.test" and must not be taken for a test binary.
		{name: "unix mixed case stem is a different file", path: "/tmp/build/Api.Test", want: false},
		{name: "windows gc binary", path: `C:\Program Files\gc\gc.exe`, want: false},
		{name: "unix gc binary", path: "/usr/local/bin/gc", want: false},
		{name: "name merely containing test", path: `C:\build\testrunner.exe`, want: false},
		{name: "bazel style name without TEST_SRCDIR", path: `C:\build\api_test.exe`, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGoTestExecutable(tt.path); got != tt.want {
				t.Fatalf("IsGoTestExecutable(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestIsGoTestExecutableRejectsRenamedSpyBinaryUnderBazel keeps the bazel
// branch narrow: TEST_SRCDIR alone must not condemn a binary this process is
// merely executing, or a probe that execs a helper would be refused.
func TestIsGoTestExecutableRejectsRenamedSpyBinaryUnderBazel(t *testing.T) {
	t.Setenv("TEST_SRCDIR", t.TempDir())
	if IsGoTestExecutable(filepath.Join(t.TempDir(), "spy_test")) {
		t.Fatal("IsGoTestExecutable matched a spy binary that is not this executable")
	}
}

// TestIsGoTestExecutableRecognizesThisBinaryWhateverItIsCalled pins the hole
// every naming rule leaves open.
//
// `go test -o` names the binary whatever it is told to, and bazel names it
// <target>_test, so a test binary need not have ".test" anywhere in its name.
// The guard exists to stop a test binary re-executing itself and re-running its
// own suite, which is decided by what the binary *is*, not what it is called.
// This test runs inside a test binary, so its own executable is the case.
func TestIsGoTestExecutableRecognizesThisBinaryWhateverItIsCalled(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if !IsGoTestExecutable(self) {
		t.Fatalf("IsGoTestExecutable(%q) = false; this process is a Go test binary and re-executing it would re-run this suite", self)
	}

	// The same file reached through a copy of the path that differs only in
	// spelling must give the same answer, which is why the comparison is by
	// file identity rather than by string.
	if runtime.GOOS == "windows" {
		if upper := strings.ToUpper(self); !IsGoTestExecutable(upper) {
			t.Errorf("IsGoTestExecutable(%q) = false for this binary spelled in upper case; Windows paths are case-insensitive", upper)
		}
	}

	// A name-based guard would have to find ".test" in the path. A hard link
	// is the same file under a different name, so it isolates the name from
	// the identity: this must still be recognized while called api-tests.
	//
	// The link is cleaned up leniently rather than through t.TempDir, because
	// it names the running executable — Windows holds that file open, and a
	// cleanup that insisted would fail the test for an unrelated reason.
	linkDir, err := os.MkdirTemp("", "nudgepoller-link")
	if err != nil {
		t.Fatalf("creating link dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(linkDir) })
	linked := filepath.Join(linkDir, "api-tests"+executableSuffix())
	if err := os.Link(self, linked); err != nil {
		t.Skipf("cannot hard-link this test binary (needs the same volume): %v", err)
	}
	if strings.Contains(strings.ToLower(filepath.Base(linked)), ".test") {
		t.Fatalf("the link %q still carries a .test marker; the case is not being exercised", linked)
	}
	if !IsGoTestExecutable(linked) {
		t.Errorf("IsGoTestExecutable(%q) = false; it is this very test binary under a name carrying no .test marker, which is exactly what `go test -o` and bazel produce", linked)
	}
}

// TestIsGoTestExecutableLeavesProductionBinariesAlone pins the other side of
// the predicate: outside a test binary, this program re-executing itself is the
// normal way a poller starts and must not be refused.
//
// Every test runs inside a test binary, so the production case is unreachable
// without standing in for it.
func TestIsGoTestExecutableLeavesProductionBinariesAlone(t *testing.T) {
	orig := runningGoTest
	t.Cleanup(func() { runningGoTest = orig })
	runningGoTest = func() bool { return false }

	// Name the file something a production binary would be called, so the
	// fallback naming rules cannot be what decides this.
	production := filepath.Join(t.TempDir(), "gc"+executableSuffix())
	if err := os.WriteFile(production, []byte("not really a binary"), 0o700); err != nil {
		t.Fatalf("writing stand-in: %v", err)
	}
	if IsGoTestExecutable(production) {
		t.Fatalf("IsGoTestExecutable(%q) = true outside a test binary; pollers could never start", production)
	}
}

// executableSuffix returns the extension this platform gives a program: ".exe"
// on Windows and none on Unix. The tests above name their files with it rather
// than with this test binary's own extension, which is ".exe" on Windows but
// ".test" on Unix: the very marker those names must not carry.
func executableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// TestRunningGoTestDetectsThisBinary pins the signal the guard now depends on.
// If a toolchain change stopped registering the testing flags, the guard would
// silently fall back to naming rules and the fan-out hole would reopen.
func TestRunningGoTestDetectsThisBinary(t *testing.T) {
	if !runningGoTest() {
		t.Fatal("runningGoTest() = false inside a Go test binary; the identity check the guard relies on no longer works")
	}
}
