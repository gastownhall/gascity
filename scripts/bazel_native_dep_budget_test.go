package scripts_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// D2 in architecture decision ga-amvyim69 defines the rounding and review
// window independently of the recorded measurements and chosen horizon.
const (
	nativeDepBudgetCapQuantum    int64 = 1_000_000
	nativeDepBudgetReviewQuantum int64 = 500_000
	nativeDepBudgetReviewDays    int64 = 30
)

func nativeDepBudgetRead(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func nativeDepBudgetRule(t *testing.T) (string, string) {
	t.Helper()
	build := nativeDepBudgetRead(t, "scripts/BUILD.bazel")
	const start = "sh_test(\n    name = \"check_native_dependency_surface_test\",\n"
	if strings.Count(build, start) != 1 {
		t.Fatal("want exactly one canonical native dependency sh_test")
	}
	before, rule, _ := strings.Cut(build, start)
	rule, _, ok := strings.Cut(rule, "\n)\n")
	if !ok {
		t.Fatal("unterminated native dependency sh_test")
	}
	return before, rule
}

func nativeDepBudgetCap(t *testing.T, rule string) int64 {
	t.Helper()
	const key = "GC_NATIVE_DEP_MAX_BINARY_BYTES"
	if strings.Count(rule, key) != 1 {
		t.Fatal("canonical rule must contain exactly one byte-cap env key")
	}
	m := regexp.MustCompile(`env\s*=\s*\{\s*"GC_NATIVE_DEP_MAX_BINARY_BYTES"\s*:\s*"([1-9][0-9]{0,14})"\s*,?\s*\}`).FindStringSubmatch(rule)
	if m == nil {
		t.Fatal("byte cap must be a positive decimal integer of at most 15 digits")
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNativeDepBudgetCapMatchesProvenance(t *testing.T) {
	before, rule := nativeDepBudgetRule(t)
	capBytes := nativeDepBudgetCap(t, rule)
	const marker = "# native-dep-budget-provenance:\n"
	build := nativeDepBudgetRead(t, "scripts/BUILD.bazel")
	if strings.Count(build, marker) != 1 || !strings.Contains(before, marker) {
		t.Fatal("want one native-dep-budget-provenance block before the canonical sh_test")
	}
	_, block, _ := strings.Cut(before, marker)
	keys := []string{"base_sha", "base_date", "base_bytes", "slope_bytes_per_day", "horizon_days", "cap_bytes", "review_threshold", "review_by", "owner"}
	values := make(map[string]string)
	lines := strings.Split(block, "\n")
	entry := regexp.MustCompile(`^#   ([a-z_]+): (.+)$`)
	i := 0
	for ; i < len(lines); i++ {
		m := entry.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		if _, exists := values[m[1]]; exists {
			t.Fatalf("duplicate provenance key %s", m[1])
		}
		values[m[1]] = m[2]
	}
	for _, line := range lines[i:] {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			t.Fatalf("non-comment between provenance and sh_test: %q", line)
		}
	}
	if len(values) != len(keys) {
		t.Fatalf("want exactly %d provenance keys, got %v", len(keys), values)
	}
	for _, key := range keys {
		if values[key] == "" {
			t.Fatalf("missing provenance key %s", key)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{10,40}$`).MatchString(values["base_sha"]) {
		t.Errorf("invalid base_sha %q", values["base_sha"])
	}
	date := func(key string) time.Time {
		v, err := time.Parse("2006-01-02", values[key])
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return v
	}
	baseDate, reviewDate := date("base_date"), date("review_by")
	number := func(key string) int64 {
		if !regexp.MustCompile(`^[1-9][0-9]{0,14}$`).MatchString(values[key]) {
			t.Fatalf("%s must be a positive decimal integer: %q", key, values[key])
		}
		n, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	base, slope, horizon := number("base_bytes"), number("slope_bytes_per_day"), number("horizon_days")
	if horizon > ((1<<63-1)-base-nativeDepBudgetCapQuantum)/slope || slope > ((1<<63-1)-nativeDepBudgetReviewQuantum)/nativeDepBudgetReviewDays {
		t.Fatal("provenance arithmetic overflows int64")
	}
	ceilTo := func(n, quantum int64) int64 { return ((n + quantum - 1) / quantum) * quantum }
	wantCap := ceilTo(base+horizon*slope, nativeDepBudgetCapQuantum)
	if recorded := number("cap_bytes"); recorded != wantCap || capBytes != wantCap {
		t.Errorf("cap: env=%d provenance=%d recomputed=%d", capBytes, recorded, wantCap)
	}
	wantThreshold := wantCap - ceilTo(nativeDepBudgetReviewDays*slope, nativeDepBudgetReviewQuantum)
	if got := number("review_threshold"); got != wantThreshold {
		t.Errorf("review_threshold=%d, want %d", got, wantThreshold)
	}
	if want := baseDate.AddDate(0, 0, int(horizon)); !reviewDate.Equal(want) {
		t.Errorf("review_by=%s, want %s", reviewDate.Format("2006-01-02"), want.Format("2006-01-02"))
	}
	owners := regexp.MustCompile(`(?m)^/scripts/\s+([^\s#]+)\s*$`).FindAllStringSubmatch(nativeDepBudgetRead(t, ".github/CODEOWNERS"), -1)
	if len(owners) != 1 {
		t.Fatalf("want one /scripts/ CODEOWNERS entry, got %v", owners)
	}
	if values["owner"] != owners[0][1] {
		t.Errorf("budget owner=%q, CODEOWNERS=%q", values["owner"], owners[0][1])
	}
}

func TestNativeDepBudgetCapHasOneSource(t *testing.T) {
	paths := []string{"Makefile", ".bazelrc", "MODULE.bazel"}
	for _, pattern := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml"} {
		files, err := filepath.Glob(filepath.Join(repoRoot(t), pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			rel, err := filepath.Rel(repoRoot(t), file)
			if err != nil {
				t.Fatal(err)
			}
			paths = append(paths, rel)
		}
	}
	for _, path := range paths {
		if strings.Contains(nativeDepBudgetRead(t, path), "GC_NATIVE_DEP_MAX_BINARY_BYTES") {
			t.Errorf("%s contains a second byte-cap source", path)
		}
	}
}

type nativeDepBudgetCase struct {
	name       string
	bytes      int64
	modules    []string
	env        map[string]string
	body       string
	forbidden  string
	missingSum bool
	emptySum   bool
	noExec     bool
	wantCode   int
	want       []string
}

func nativeDepBudgetRun(t *testing.T, tc nativeDepBudgetCase) {
	t.Helper()
	dir := t.TempDir()
	if !tc.missingSum {
		modules := tc.modules
		if modules == nil {
			modules = []string{"github.com/steveyegge/beads"}
		}
		var sum strings.Builder
		if !tc.emptySum {
			for _, mod := range modules {
				fmt.Fprintf(&sum, "%s v1.0.0 h1:x=\n", mod)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte(sum.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := tc.body
	if body == "" {
		body = "printf '%s\\n' 'metrics help'\nexit 0\n"
	}
	binary := filepath.Join(dir, "gc")
	content := "#!/bin/sh\n# " + tc.forbidden + "\n" + body
	if err := os.WriteFile(binary, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	if tc.bytes == 0 {
		tc.bytes = int64(len(content))
	}
	if err := os.Truncate(binary, tc.bytes); err != nil {
		t.Fatalf("create %d-byte sparse gc fixture: %v", tc.bytes, err)
	}
	if tc.noExec {
		if err := os.Chmod(binary, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": dir, "TMPDIR": dir,
		"GC_NATIVE_DEP_MAX_BINARY_BYTES": "1000000",
		"GC_NATIVE_DEP_MAX_MODULES":      "100", "GC_NATIVE_DEP_MAX_AWS_MODULES": "2",
		"GC_NATIVE_DEP_MAX_AZURE_MODULES": "2", "GC_NATIVE_DEP_MAX_DOLTHUB_MODULES": "2",
		"GC_NATIVE_DEP_MAX_GOOGLE_API_MODULES": "1",
	}
	for k, v := range tc.env {
		env[k] = v
	}
	cmd := testCommand("/bin/bash", filepath.Join(repoRoot(t), "scripts/check-native-dependency-surface.sh"), "--gc-binary", binary)
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
	if code != tc.wantCode {
		t.Fatalf("guard exit=%d, want %d: %v\n%s", code, tc.wantCode, err, out)
	}
	for _, want := range tc.want {
		if !strings.Contains(string(out), want) {
			t.Errorf("guard output missing %q:\n%s", want, out)
		}
	}
	if code == 0 && !regexp.MustCompile(fmt.Sprintf(`\bbinary_bytes=%d\b`, tc.bytes)).Match(out) {
		t.Errorf("guard did not report actual %d bytes:\n%s", tc.bytes, out)
	}
}

func TestNativeDepGuardBehavior(t *testing.T) {
	_, rule := nativeDepBudgetRule(t)
	realCap := nativeDepBudgetCap(t, rule)
	cases := []nativeDepBudgetCase{
		{name: "S1_at_cap", bytes: 1_000_000},
		{name: "S2_over_cap", bytes: 1_000_001, wantCode: 1, want: []string{"1000001 bytes", "max is 1000000"}},
		{name: "S3_below_cap", bytes: 999_999},
		{name: "S4_at_real_cap", bytes: realCap, env: map[string]string{"GC_NATIVE_DEP_MAX_BINARY_BYTES": strconv.FormatInt(realCap, 10)}},
		{name: "S5_over_real_cap", bytes: realCap + 1, env: map[string]string{"GC_NATIVE_DEP_MAX_BINARY_BYTES": strconv.FormatInt(realCap, 10)}, wantCode: 1, want: []string{fmt.Sprintf("%d bytes", realCap+1), fmt.Sprintf("max is %d", realCap)}},
		{name: "M1_at_module_cap", modules: []string{"github.com/steveyegge/beads", "example.com/m1"}, env: map[string]string{"GC_NATIVE_DEP_MAX_MODULES": "3"}},
		{name: "M2_over_module_cap", modules: []string{"github.com/steveyegge/beads", "example.com/m1", "example.com/m2"}, env: map[string]string{"GC_NATIVE_DEP_MAX_MODULES": "3"}, wantCode: 1, want: []string{"module graph has 4", "max is 3"}},
		{name: "E0_no_beads", modules: []string{"example.com/m1"}, wantCode: 1, want: []string{"expected exactly one", "got 0"}},
		{name: "E1_duplicate_beads_rows", modules: []string{"github.com/steveyegge/beads", "github.com/steveyegge/beads"}},
		{name: "N1_missing_sum", missingSum: true, wantCode: 1, want: []string{"go.sum not found"}},
		{name: "N2_empty_sum", emptySum: true, wantCode: 1, want: []string{"go.sum not found"}},
		{name: "T1_help_failure", body: "echo 'help failed'\nexit 7\n", wantCode: 1, want: []string{"metrics --help failed"}},
		{name: "T2_runtime_hook", body: "printf '%s%s\\n' '__testhook-' 'record-help'\nexit 0\n", wantCode: 1, want: []string{"metrics --help exposes"}},
		{name: "C1_coverage", bytes: 1_000_001, env: map[string]string{"COVERAGE": "1"}, want: []string{"skipping binary size cap (1000001 bytes)"}},
		{name: "C2_coverage_dir", bytes: 1_000_001, env: map[string]string{"COVERAGE_DIR": "/coverage"}, want: []string{"skipping binary size cap (1000001 bytes)"}},
		{name: "C3_coverage_zero", bytes: 1_000_001, env: map[string]string{"COVERAGE": "0"}, wantCode: 1, want: []string{"max is 1000000"}},
		{name: "C4_coverage_dir_empty", bytes: 1_000_001, env: map[string]string{"COVERAGE_DIR": ""}, wantCode: 1, want: []string{"max is 1000000"}},
		{name: "C5_coverage_symbol", forbidden: "main.runProductMetricsTesthookChild", env: map[string]string{"COVERAGE": "1"}, wantCode: 1, want: []string{"testhook symbol"}},
		{name: "C6_coverage_runtime_hook", body: "printf '%s%s\\n' '__testhook-' 'record-help'\nexit 0\n", env: map[string]string{"COVERAGE": "1"}, wantCode: 1, want: []string{"metrics --help exposes"}},
		{name: "C7_coverage_modules", env: map[string]string{"COVERAGE": "1", "GC_NATIVE_DEP_MAX_MODULES": "1"}, wantCode: 1, want: []string{"module graph has 2"}},
		{name: "X1_non_executable", noExec: true, wantCode: 1, want: []string{"not executable"}},
	}
	for _, family := range []struct{ path, key, message string }{
		{"github.com/aws/aws-sdk-go-v2/service/", "GC_NATIVE_DEP_MAX_AWS_MODULES", "AWS SDK"},
		{"github.com/Azure/azure-sdk-for-go/sdk/", "GC_NATIVE_DEP_MAX_AZURE_MODULES", "Azure SDK"},
		{"github.com/dolthub/", "GC_NATIVE_DEP_MAX_DOLTHUB_MODULES", "DoltHub"},
	} {
		for _, count := range []int{2, 3} {
			tc := nativeDepBudgetCase{name: fmt.Sprintf("G_%s_%d", family.message, count), env: map[string]string{family.key: "2"}, modules: []string{"github.com/steveyegge/beads"}}
			for i := 0; i < count; i++ {
				tc.modules = append(tc.modules, fmt.Sprintf("%sm%d", family.path, i))
			}
			if count == 3 {
				tc.wantCode, tc.want = 1, []string{family.message, "exceeds 2"}
			}
			cases = append(cases, tc)
		}
	}
	for _, cap := range []string{"1", "0"} {
		tc := nativeDepBudgetCase{name: "G_googleapi_cap_" + cap, modules: []string{"github.com/steveyegge/beads", "google.golang.org/api"}, env: map[string]string{"GC_NATIVE_DEP_MAX_GOOGLE_API_MODULES": cap}}
		if cap == "0" {
			tc.wantCode, tc.want = 1, []string{"google.golang.org/api", "exceeds 0"}
		}
		cases = append(cases, tc)
	}
	for i, token := range []string{"main.runProductMetricsTesthookChild", "main.newProductMetricsTesthookRecordHelpCommand", "internal/productmetrics.OpenTesthook", "internal/productmetrics.testhookLoopbackHost", "GC_PRODUCT_METRICS_TESTHOOK_ENDPOINT", "GC_PRODUCT_METRICS_TESTHOOK_CA_FILE", "__testhook-record-help"} {
		cases = append(cases, nativeDepBudgetCase{name: fmt.Sprintf("F%d_forbidden", i+1), forbidden: token, wantCode: 1, want: []string{token}})
	}
	started := time.Now()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { nativeDepBudgetRun(t, tc) })
	}
	t.Logf("%d real-script fixture cases completed in %s", len(cases), time.Since(started))
}
