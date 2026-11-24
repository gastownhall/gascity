package scripts_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func nativeDepContractRead(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), path))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func nativeDepContractCILabel(t *testing.T) string {
	t.Helper()
	var names []string
	for _, chunk := range strings.Split(nativeDepContractRead(t, "scripts/BUILD.bazel"), "sh_test(\n")[1:] {
		rule, _, _ := strings.Cut(chunk, "\n)\n")
		if !strings.Contains(rule, `srcs = ["check-native-dependency-surface.sh"]`) {
			continue
		}
		m := regexp.MustCompile(`^\s*name = "([^"]+)",`).FindStringSubmatch(rule)
		if m == nil {
			t.Fatal("guard sh_test has no name")
		}
		names = append(names, m[1])
	}
	if len(names) != 1 {
		t.Fatalf("want one guard sh_test, got %v", names)
	}
	return "//scripts:" + names[0]
}

type nativeDepContractOptions struct {
	args          []string
	env           map[string]string
	unsetCap      bool
	missingSum    bool
	missingBinary bool
	nonExecutable bool
	emptySize     bool
	forbidden     string
}

func nativeDepContractRun(t *testing.T, options nativeDepContractOptions) (string, int, string) {
	t.Helper()
	dir := t.TempDir()
	shim := filepath.Join(dir, "bin")
	if err := os.Mkdir(shim, 0o700); err != nil {
		t.Fatal(err)
	}
	goLog := filepath.Join(dir, "go.log")
	writeExecutable(t, filepath.Join(shim, "go"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$NATIVE_DEP_GO_LOG\"\nexit 1\n")
	if options.emptySize {
		writeExecutable(t, filepath.Join(shim, "wc"), "#!/bin/sh\nif [ \"$1\" = '-c' ]; then exit 0; fi\nexec /usr/bin/wc \"$@\"\n")
	}
	if !options.missingSum {
		writeTestFile(t, filepath.Join(dir, "go.sum"), "github.com/steveyegge/beads v1.0.0 h1:x=\n")
	}
	binary := filepath.Join(dir, "gc")
	if !options.missingBinary {
		writeExecutable(t, binary, "#!/bin/sh\n# "+options.forbidden+"\nprintf '%s\\n' 'metrics help'\nexit 0\n")
	}
	if options.nonExecutable {
		if err := os.Chmod(binary, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := options.args
	if args == nil {
		args = []string{"--gc-binary", binary}
	}
	// The literal PATH and fresh HOME/TMPDIR prevent live coverage, agent,
	// toolchain or cap settings from leaking into these command-level cases.
	env := map[string]string{
		"PATH": shim + ":/usr/bin:/bin", "HOME": dir, "TMPDIR": dir,
		"GC_NATIVE_DEP_MAX_BINARY_BYTES": "1000000", "NATIVE_DEP_GO_LOG": goLog,
	}
	for k, v := range options.env {
		env[k] = v
	}
	if options.unsetCap {
		delete(env, "GC_NATIVE_DEP_MAX_BINARY_BYTES")
	}
	cmd := testCommand("/bin/bash", append([]string{filepath.Join(repoRoot(t), "scripts/check-native-dependency-surface.sh")}, args...)...)
	cmd.Dir = dir
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run guard: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	log, err := os.ReadFile(goLog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(out), code, string(log)
}

func nativeDepContractWant(t *testing.T, options nativeDepContractOptions, wantCode int, want ...string) string {
	t.Helper()
	out, code, log := nativeDepContractRun(t, options)
	if code != wantCode {
		t.Errorf("exit=%d, want %d; go invocations=%q\n%s", code, wantCode, log, out)
	}
	for _, text := range want {
		if !strings.Contains(out, text) {
			t.Errorf("output missing %q:\n%s", text, out)
		}
	}
	if log != "" {
		t.Errorf("guard invoked host go: %q", log)
	}
	return out
}

func TestNativeDepContractRequiresGCBinary(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no_args", []string{}},
		{"extra_arg", []string{"--gc-binary", "/bin/true", "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nativeDepContractWant(t, nativeDepContractOptions{args: tc.args}, 2, "usage:")
		})
	}
}

func TestNativeDepContractRequiresByteCap(t *testing.T) {
	for _, unset := range []bool{true, false} {
		t.Run(fmt.Sprintf("unset_%t", unset), func(t *testing.T) {
			nativeDepContractWant(t, nativeDepContractOptions{unsetCap: unset, env: map[string]string{"GC_NATIVE_DEP_MAX_BINARY_BYTES": ""}}, 2, "GC_NATIVE_DEP_MAX_BINARY_BYTES", "scripts/BUILD.bazel")
		})
	}
}

func TestNativeDepContractRejectsMalformedByteCap(t *testing.T) {
	for _, value := range []string{"abc", "0x10", "1.5e8", "-5", "0", "007", " 5", "5 ", "+5", "1000000000000000", "99999999999999999999"} {
		t.Run(fmt.Sprintf("value_%q", value), func(t *testing.T) {
			nativeDepContractWant(t, nativeDepContractOptions{env: map[string]string{"GC_NATIVE_DEP_MAX_BINARY_BYTES": value}}, 2, "GC_NATIVE_DEP_MAX_BINARY_BYTES", "scripts/BUILD.bazel")
		})
	}
}

func TestNativeDepContractAcceptsFifteenDigitByteCap(t *testing.T) {
	nativeDepContractWant(t, nativeDepContractOptions{env: map[string]string{"GC_NATIVE_DEP_MAX_BINARY_BYTES": "999999999999999"}}, 0)
}

func TestNativeDepContractRejectsMalformedOtherCaps(t *testing.T) {
	for _, key := range []string{"GC_NATIVE_DEP_MAX_MODULES", "GC_NATIVE_DEP_MAX_AWS_MODULES", "GC_NATIVE_DEP_MAX_AZURE_MODULES", "GC_NATIVE_DEP_MAX_DOLTHUB_MODULES", "GC_NATIVE_DEP_MAX_GOOGLE_API_MODULES"} {
		for _, value := range []string{"abc", "", "007", "-1", "1000000000000000", "0"} {
			t.Run(fmt.Sprintf("%s_value_%q", key, value), func(t *testing.T) {
				code := 2
				if key != "GC_NATIVE_DEP_MAX_MODULES" && value == "0" {
					code = 0
				}
				var want []string
				if code == 2 {
					want = []string{key}
				}
				nativeDepContractWant(t, nativeDepContractOptions{env: map[string]string{key: value}}, code, want...)
			})
		}
	}
}

func TestNativeDepContractUnsetOptionalCapsUseDefaults(t *testing.T) {
	nativeDepContractWant(t, nativeDepContractOptions{}, 0)
}

func TestNativeDepContractConfigErrorsBeatVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options nativeDepContractOptions
	}{
		{"coverage", nativeDepContractOptions{env: map[string]string{"COVERAGE": "1"}}},
		{"coverage_dir", nativeDepContractOptions{env: map[string]string{"COVERAGE_DIR": "/coverage"}}},
		{"module_overrun", nativeDepContractOptions{env: map[string]string{"GC_NATIVE_DEP_MAX_MODULES": "1"}}},
		{"forbidden_literal", nativeDepContractOptions{forbidden: "GC_PRODUCT_METRICS_TESTHOOK_ENDPOINT"}},
		{"missing_sum", nativeDepContractOptions{missingSum: true}},
		{"missing_binary", nativeDepContractOptions{missingBinary: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := tc.options
			if options.env == nil {
				options.env = make(map[string]string)
			}
			options.env["GC_NATIVE_DEP_MAX_BINARY_BYTES"] = "abc"
			nativeDepContractWant(t, options, 2, "GC_NATIVE_DEP_MAX_BINARY_BYTES", "scripts/BUILD.bazel")
		})
	}
}

func TestNativeDepContractRejectsEmptySizeRead(t *testing.T) {
	nativeDepContractWant(t, nativeDepContractOptions{emptySize: true}, 1)
}

func TestNativeDepContractPassLineReportsCap(t *testing.T) {
	for _, coverage := range []string{"0", "1"} {
		t.Run("coverage_"+coverage, func(t *testing.T) {
			out := nativeDepContractWant(t, nativeDepContractOptions{env: map[string]string{"COVERAGE": coverage}}, 0)
			lines := strings.Split(strings.TrimSpace(out), "\n")
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, "native dependency guard: modules=") || !strings.HasSuffix(last, " max_binary_bytes=1000000") {
				t.Errorf("PASS line must retain prefix and append cap, got %q", last)
			}
		})
	}
}

func TestNativeDepContractScriptHasNoSelfBuild(t *testing.T) {
	script := nativeDepContractRead(t, "scripts/check-native-dependency-surface.sh")
	for _, forbidden := range []string{"go build", "CGO_ENABLED", "190000000"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("guard retains obsolete self-build path %q", forbidden)
		}
	}
}

func TestNativeDepContractUnchangedFailuresStillFail(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options nativeDepContractOptions
		code    int
	}{
		{"non_executable", nativeDepContractOptions{nonExecutable: true}, 1},
		{"missing_binary", nativeDepContractOptions{missingBinary: true}, 1},
		{"unknown_arg", nativeDepContractOptions{args: []string{"--unknown"}}, 2},
		{"missing_value", nativeDepContractOptions{args: []string{"--gc-binary"}}, 2},
		{"empty_value", nativeDepContractOptions{args: []string{"--gc-binary", ""}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) { nativeDepContractWant(t, tc.options, tc.code) })
	}
}
