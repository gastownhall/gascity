package scripts_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The real hook entry point composes staged-file detection, beads validation,
// engine selection and validation failures. Only external tools are recorded.
func TestPreCommitLocalRuntimeDocsSelection(t *testing.T) {
	for _, tc := range []struct {
		name, mode, probe, staged  string
		bazel                      bool
		bdExit, makeExit, wantCode int
		wantMake                   string
		wantProbe, wantBanner      bool
		wantText                   []string
	}{
		{
			name: "missing ICU validates docs through Go", bazel: true, probe: "missing libicui18n.so.74 libicuuc.so.74 libicudata.so.74", wantMake: "check-docs-go", wantProbe: true, wantBanner: true,
			wantText: []string{"libicui18n.so.74 libicuuc.so.74 libicudata.so.74", "GC_PRECOMMIT_DOCS=bazel", "BAZEL_FLAGS=--config=remote-exec"},
		},
		{name: "usable ICU retains Bazel", bazel: true, probe: "ok", wantMake: "check-docs", wantProbe: true},
		{name: "other platform retains Bazel", bazel: true, probe: "n/a Darwin/arm64", wantMake: "check-docs", wantProbe: true},
		{
			name: "unknown probe validates docs through Go", bazel: true, probe: "unknown timeout tool is unavailable", wantMake: "check-docs-go", wantProbe: true, wantBanner: true,
			wantText: []string{"could not verify", "timeout tool is unavailable"},
		},
		{name: "empty probe validates docs through Go", bazel: true, wantMake: "check-docs-go", wantProbe: true, wantBanner: true, wantText: []string{"could not verify", "probe produced no usable result"}},
		{name: "malformed probe validates docs through Go", bazel: true, probe: "missing", wantMake: "check-docs-go", wantProbe: true, wantBanner: true, wantText: []string{"could not verify", "probe produced no usable result"}},
		{name: "no bazel validates docs through Go", probe: "missing libicuuc.so.74", wantMake: "check-docs-go", wantBanner: true, wantText: []string{"bazel is not installed"}},
		{name: "explicit auto probes", mode: "auto", bazel: true, probe: "ok", wantMake: "check-docs", wantProbe: true},
		{name: "explicit Bazel ignores missing ICU", mode: "bazel", bazel: true, probe: "missing libicuuc.so.74", wantMake: "check-docs"},
		{name: "explicit Bazel delegates unavailable tool failure", mode: "bazel", makeExit: 7, wantMake: "check-docs", wantCode: 7},
		{name: "explicit Go never probes", mode: "go", bazel: true, probe: "missing libicuuc.so.74", wantMake: "check-docs-go", wantBanner: true, wantText: []string{"why: GC_PRECOMMIT_DOCS=go"}},
		{name: "unknown docs mode is fatal", mode: "skip", bazel: true, probe: "ok", wantCode: 2, wantText: []string{"GC_PRECOMMIT_DOCS"}},
		{name: "beads rejection short circuits", bazel: true, probe: "missing libicuuc.so.74", bdExit: 9, wantCode: 9, wantText: []string{"beads-validation"}},
		{name: "Go docs failure blocks commit", bazel: true, probe: "missing libicuuc.so.74", makeExit: 7, wantCode: 7, wantMake: "check-docs-go", wantProbe: true, wantBanner: true, wantText: []string{"docsync-validation-failed"}},
		{name: "Bazel docs failure blocks commit", bazel: true, probe: "ok", makeExit: 7, wantCode: 7, wantMake: "check-docs", wantProbe: true, wantText: []string{"docsync-validation-failed"}},
		{name: "unstaged docs do not trigger probe", bazel: true, probe: "missing libicuuc.so.74", staged: "fixture.txt"},
		{name: "docs knob is lazy for unrelated files", mode: "skip", bazel: true, probe: "missing libicuuc.so.74", staged: "fixture.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, record := newPreCommitDocsFixture(t, tc.bazel, tc.bdExit, tc.makeExit)
			installLocalRuntimeProbe(t, repoRoot(t), f.repo, tc.probe, record)
			staged := tc.staged
			if staged == "" {
				staged = "README.md"
			}
			writeTestFile(t, filepath.Join(f.repo, staged), "staged content\n")
			// Keep an unstaged docs edit present too: only the index selects docs.
			writeTestFile(t, filepath.Join(f.repo, "UNSTAGED.md"), "not staged\n")
			f.git(t, "add", staged)
			f.env = append(f.env, "GC_PRECOMMIT_DOCS="+tc.mode)
			cmd := testCommand("bash", filepath.Join(f.repo, ".githooks/pre-commit"))
			cmd.Dir, cmd.Env = f.repo, f.env
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if cmd.ProcessState == nil {
					t.Fatalf("start pre-commit: %v", err)
				}
				code = cmd.ProcessState.ExitCode()
			}
			if code != tc.wantCode {
				t.Errorf("pre-commit exit = %d, want %d\n%s", code, tc.wantCode, out)
			}
			if got := strings.TrimSpace(f.read(t, f.makeRuns)); got != tc.wantMake {
				t.Errorf("make = %q, want %q\n%s", got, tc.wantMake, out)
			}
			wantCalls := ""
			if tc.wantProbe {
				wantCalls = "probe\n"
			}
			if got := f.read(t, record); got != wantCalls {
				t.Errorf("runtime probe calls = %q, want %q", got, wantCalls)
			}
			assertPreCommitDocsBanner(t, string(out), tc.wantBanner)
			for _, text := range tc.wantText {
				if !strings.Contains(string(out), text) {
					t.Errorf("output missing %q:\n%s", text, out)
				}
			}
		})
	}
}

func newPreCommitDocsFixture(t *testing.T, bazel bool, bdExit, makeExit int) (*prePushFixture, string) {
	t.Helper()
	repo, records := t.TempDir(), t.TempDir()
	root := repoRoot(t)
	installBeadsChainForTempRepo(t, root, repo)
	writeExecutable(t, filepath.Join(repo, ".githooks/pre-commit"), readFile(t, root, ".githooks/pre-commit"))
	stubs := map[string]string{
		"bd": `#!/usr/bin/env bash
printf 'beads-validation\n'
exit "$DOCS_BD_EXIT"
`,
		"make": `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$MAKE_RECORD"
printf 'MAKE-INVOKED %s\n' "$*"
if [ "$DOCS_MAKE_EXIT" != 0 ]; then printf 'docsync-validation-failed\n'; fi
exit "$DOCS_MAKE_EXIT"
`,
	}
	if bazel {
		stubs["bazel"] = "#!/usr/bin/env bash\nexit 0\n"
	}
	f := &prePushFixture{repo: repo, makeRuns: filepath.Join(records, "make")}
	f.env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, stubs), "HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"MAKE_RECORD=" + f.makeRuns,
		"DOCS_BD_EXIT=" + strconv.Itoa(bdExit), "DOCS_MAKE_EXIT=" + strconv.Itoa(makeExit),
	}
	f.git(t, "init", "-q", "-b", "main")
	return f, filepath.Join(records, "runtime-probes")
}

func assertPreCommitDocsBanner(t *testing.T, out string, want bool) {
	t.Helper()
	const banner = "pre-commit: running make check-docs-go (go test ./test/docsync): NOT the Bazel docs gate CI enforces."
	if strings.Contains(out, banner) != want {
		t.Errorf("Go docs banner present = %v, want %v:\n%s", strings.Contains(out, banner), want, out)
	}
	if !want {
		return
	}
	for _, text := range []string{"pre-commit: why:", "CI runs bazel test //...", "declared inputs", "can still fail", "pre-commit: fix:", "GC_PRECOMMIT_DOCS=bazel"} {
		if !strings.Contains(out, text) {
			t.Errorf("fallback banner missing %q:\n%s", text, out)
		}
	}
	if i := strings.Index(out, "MAKE-INVOKED"); i < 0 || strings.Index(out, banner) > i {
		t.Errorf("fallback explanation must precede docs validation:\n%s", out)
	}
}
