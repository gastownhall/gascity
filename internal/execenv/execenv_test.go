package execenv

import (
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestProductMetricsChildEnvCanonicalizesOnlyItsOwnKey(t *testing.T) {
	environ := []string{
		"PATH=/bin",
		"GC_DISABLE_USAGE_METRICS=0",
		"BD_DISABLE_METRICS=1",
		"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.invalid",
		"API_TOKEN=preserve-inherited-value",
		"DUPLICATE=first",
		"GC_DISABLE_USAGE_METRICS=true",
		"DUPLICATE=second",
		"GC_DISABLE_USAGE_METRICS",
		"GC_DISABLE_USAGE_METRICS_EXTRA=keep",
	}
	original := slices.Clone(environ)
	want := []string{
		"PATH=/bin",
		"BD_DISABLE_METRICS=1",
		"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.invalid",
		"API_TOKEN=preserve-inherited-value",
		"DUPLICATE=first",
		"DUPLICATE=second",
		"GC_DISABLE_USAGE_METRICS_EXTRA=keep",
		UsageMetricsDisabledEntry,
	}

	got := WithUsageMetricsDisabled(environ)
	if !slices.Equal(got, want) {
		t.Fatalf("WithUsageMetricsDisabled() = %#v, want %#v", got, want)
	}
	if !slices.Equal(environ, original) {
		t.Fatalf("WithUsageMetricsDisabled mutated input to %#v, want %#v", environ, original)
	}
	if again := WithUsageMetricsDisabled(got); !slices.Equal(again, want) {
		t.Fatalf("WithUsageMetricsDisabled is not idempotent: second result = %#v, want %#v", again, want)
	}
}

func TestProductMetricsChildEnvNilEnvironment(t *testing.T) {
	want := []string{UsageMetricsDisabledEntry}
	if got := WithUsageMetricsDisabled(nil); !slices.Equal(got, want) {
		t.Fatalf("WithUsageMetricsDisabled(nil) = %#v, want %#v", got, want)
	}
}

func TestProductMetricsChildEnvPlatformKeyComparison(t *testing.T) {
	environ := []string{
		"BEFORE=1",
		"gc_disable_usage_metrics=lowercase",
		"Gc_Disable_Usage_Metrics=mixed-case",
		UsageMetricsDisableEnv + "=canonical",
		"BD_DISABLE_METRICS=keep-beads-setting",
		"OTEL_SERVICE_NAME=keep-otel-setting",
		"AFTER=2",
	}
	tests := []struct {
		name string
		goos string
		want []string
	}{
		{
			name: "windows is case insensitive",
			goos: "windows",
			want: []string{
				"BEFORE=1",
				"BD_DISABLE_METRICS=keep-beads-setting",
				"OTEL_SERVICE_NAME=keep-otel-setting",
				"AFTER=2",
				UsageMetricsDisabledEntry,
			},
		},
		{
			name: "unix is case sensitive",
			goos: "linux",
			want: []string{
				"BEFORE=1",
				"gc_disable_usage_metrics=lowercase",
				"Gc_Disable_Usage_Metrics=mixed-case",
				"BD_DISABLE_METRICS=keep-beads-setting",
				"OTEL_SERVICE_NAME=keep-otel-setting",
				"AFTER=2",
				UsageMetricsDisabledEntry,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := withUsageMetricsDisabledForGOOS(environ, tc.goos)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("withUsageMetricsDisabledForGOOS(%q) = %#v, want %#v", tc.goos, got, tc.want)
			}
			if again := withUsageMetricsDisabledForGOOS(got, tc.goos); !slices.Equal(again, tc.want) {
				t.Fatalf("withUsageMetricsDisabledForGOOS(%q) is not idempotent: %#v", tc.goos, again)
			}
		})
	}
}

func TestProductMetricsChildEnvCanonicalConstants(t *testing.T) {
	if UsageMetricsDisableValue != "1" {
		t.Fatalf("UsageMetricsDisableValue = %q, want 1", UsageMetricsDisableValue)
	}
	want := UsageMetricsDisableEnv + "=" + UsageMetricsDisableValue
	if UsageMetricsDisabledEntry != want {
		t.Fatalf("UsageMetricsDisabledEntry = %q, want %q", UsageMetricsDisabledEntry, want)
	}
}

func TestProductMetricsChildEnvImportBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read execenv package: %v", err)
	}
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import %s in %s: %v", spec.Path.Value, name, err)
			}
			if isGasCityModuleImport(path) {
				t.Fatalf("neutral internal/execenv production file %s imports Gas City package %q", name, path)
			}
		}
	}
	if parsed == 0 {
		t.Fatal("import-boundary guard parsed no internal/execenv production files")
	}
}

func isGasCityModuleImport(path string) bool {
	const module = "github.com/gastownhall/gascity"
	return path == module || strings.HasPrefix(path, module+"/")
}

func TestProductMetricsChildEnvGasCityImportClassification(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "github.com/gastownhall/gascity", want: true},
		{path: "github.com/gastownhall/gascity/internal/productmetrics", want: true},
		{path: "github.com/gastownhall/gascity-fork", want: false},
		{path: "runtime", want: false},
	}
	for _, tc := range tests {
		if got := isGasCityModuleImport(tc.path); got != tc.want {
			t.Errorf("isGasCityModuleImport(%q) = %t, want %t", tc.path, got, tc.want)
		}
	}
}

func TestFilterInheritedStripsSensitiveEnv(t *testing.T) {
	got := FilterInherited([]string{
		"PATH=/bin",
		"GITHUB_TOKEN=ghs_secret",
		"OPENAI_API_KEY=sk-secret",
		"GC_INSTANCE_TOKEN=fence",
		"HOME=/tmp/home",
	})
	joined := strings.Join(got, "\n")
	for _, secret := range []string{"GITHUB_TOKEN", "OPENAI_API_KEY", "GC_INSTANCE_TOKEN", "ghs_secret", "sk-secret", "fence"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("FilterInherited leaked %q in %q", secret, joined)
		}
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/tmp/home") {
		t.Fatalf("FilterInherited dropped non-sensitive env: %q", joined)
	}
}

func TestMergeMapPreservesExplicitSensitiveOverrides(t *testing.T) {
	got := MergeMap([]string{
		"PATH=/bin",
		"GC_DOLT_PASSWORD=stale",
		"GITHUB_TOKEN=ambient",
	}, map[string]string{
		"GC_DOLT_PASSWORD": "required",
		"BEADS_DIR":        "/city/.beads",
	})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "ambient") || strings.Contains(joined, "stale") {
		t.Fatalf("MergeMap leaked inherited secret: %q", joined)
	}
	if !strings.Contains(joined, "GC_DOLT_PASSWORD=required") {
		t.Fatalf("MergeMap did not preserve explicit secret override: %q", joined)
	}
}

func TestRedactTextRedactsEnvValuesAndAssignments(t *testing.T) {
	got := RedactText(
		"token=literal-secret GITHUB_TOKEN=ghs_secret output ghs_secret --password hunter2",
		[]string{"GITHUB_TOKEN=ghs_secret"},
	)
	for _, secret := range []string{"literal-secret", "ghs_secret", "hunter2"} {
		if strings.Contains(got, secret) {
			t.Fatalf("RedactText leaked %q in %q", secret, got)
		}
	}
	if strings.Count(got, Redacted) < 3 {
		t.Fatalf("RedactText redactions = %q, want at least three", got)
	}
}
