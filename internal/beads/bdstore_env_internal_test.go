package beads

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envValues(env []string, key string) []string {
	var out []string
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = append(out, strings.TrimPrefix(e, prefix))
		}
	}
	return out
}

// standardOTELBd and legacyOTELOnlyBd stand in for the per-binary version
// probe execCommandRunner hands execEnvFor: a bd that reads BD_OTEL_ENABLED
// plus the standard OTEL names, and one that reads only the legacy names.
func standardOTELBd() bool   { return true }
func legacyOTELOnlyBd() bool { return false }

// otelProbeUnused is the probe for an execEnvFor call with no legacy OTEL name
// to translate; consulting it fails the test, since every bd command would
// otherwise pay for a version probe.
func otelProbeUnused(t *testing.T) func() bool {
	return func() bool {
		t.Helper()
		t.Error("bd version probe consulted with no legacy OTEL name to translate")
		return false
	}
}

func TestExecEnvForBd_InjectsAutoBackupOptOut(t *testing.T) {
	// Every bd subprocess spawned through the runner must carry the
	// auto-backup opt-out (ga-yfbs28): bd's PersistentPostRun backup_export
	// sync has no retention and stuck-loops on broken remote state (the
	// 2026-06-08 town-wide wedge, ga-0eq). The projected-env opt-out only
	// covers gc env projections; this is the choke point for everything
	// else (hook claim, store bridge, t3bridge, libstore, provider
	// lifecycle).
	base := []string{"PATH=/usr/bin"}
	got := execEnvFor("bd", base, nil, otelProbeUnused(t))
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [false]", vals)
	}
}

func TestExecEnvForBd_OverridesInheritedEnable(t *testing.T) {
	// A BD_BACKUP_ENABLED=true inherited from the parent process must not
	// leak through: gc policy forces the opt-out on gc-managed bd calls,
	// matching applyBdAutoBackupOptOut's unconditional projection.
	base := []string{"PATH=/usr/bin", "BD_BACKUP_ENABLED=true"}
	got := execEnvFor("bd", base, nil, otelProbeUnused(t))
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [false] (inherited true must be replaced)", vals)
	}
}

func TestExecEnvForBd_ExplicitCallerOverrideWins(t *testing.T) {
	// An explicit per-call override is a deliberate caller decision (e.g. a
	// backup-focused test fixture) and must beat the injected baseline.
	base := []string{"PATH=/usr/bin"}
	got := execEnvFor("bd", base, map[string]string{"BD_BACKUP_ENABLED": "true"}, otelProbeUnused(t))
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [true] (explicit override wins)", vals)
	}
}

func TestExecEnvForBd_MergesOtherOverrides(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/u"}
	got := execEnvFor("bd", base, map[string]string{"BEADS_DIR": "/x/.beads"}, otelProbeUnused(t))
	if vals := envValues(got, "BEADS_DIR"); len(vals) != 1 || vals[0] != "/x/.beads" {
		t.Errorf("BEADS_DIR values = %v, want [/x/.beads]", vals)
	}
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want [false] alongside other overrides", vals)
	}
	if vals := envValues(got, "HOME"); len(vals) != 1 || vals[0] != "/home/u" {
		t.Errorf("HOME values = %v, want [/home/u] preserved", vals)
	}
}

func TestExecEnvForNonBd_LeavesEnvAlone(t *testing.T) {
	// The runner also execs dolt directly; non-bd commands keep the
	// caller-visible environment untouched.
	base := []string{"PATH=/usr/bin", "BD_BACKUP_ENABLED=true"}
	got := execEnvFor("dolt", base, nil, otelProbeUnused(t))
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want [true] untouched for non-bd commands", vals)
	}
}

func TestExecEnvForBd_TranslatesLegacyOTELMetricsAndLogsURLs(t *testing.T) {
	// gc's SetProcessOTELAttrs sets the legacy BD_OTEL_METRICS_URL /
	// BD_OTEL_LOGS_URL names process-wide; bd v1.2.0, v1.2.1 and v1.3.0+
	// deprecate those in favor of the standard OTEL_EXPORTER_OTLP_* names
	// gated by BD_OTEL_ENABLED=true, and warn on stderr whenever a legacy
	// name is still present. Translate for the bd child only so the warning
	// disappears without setting the standard (spec-wide) names on any other
	// child gc spawns.
	base := []string{
		"PATH=/usr/bin",
		"BD_OTEL_METRICS_URL=http://localhost:8428/opentelemetry/api/v1/push",
		"BD_OTEL_LOGS_URL=http://localhost:9428/insert/opentelemetry/v1/logs",
	}
	got := execEnvFor("bd", base, nil, standardOTELBd)

	if vals := envValues(got, "BD_OTEL_METRICS_URL"); len(vals) != 0 {
		t.Errorf("legacy BD_OTEL_METRICS_URL leaked through: %v", vals)
	}
	if vals := envValues(got, "BD_OTEL_LOGS_URL"); len(vals) != 0 {
		t.Errorf("legacy BD_OTEL_LOGS_URL leaked through: %v", vals)
	}
	if vals := envValues(got, "BD_OTEL_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_OTEL_ENABLED values = %v, want exactly [true]", vals)
	}
	if vals := envValues(got, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); len(vals) != 1 || vals[0] != "http://localhost:8428/opentelemetry/api/v1/push" {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT values = %v", vals)
	}
	if vals := envValues(got, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); len(vals) != 1 || vals[0] != "http://localhost:9428/insert/opentelemetry/v1/logs" {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT values = %v", vals)
	}
}

func TestExecEnvForBd_TranslatesLegacyOTELMetricsOnlyWhenLogsUnset(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"BD_OTEL_METRICS_URL=http://localhost:8428/opentelemetry/api/v1/push",
	}
	got := execEnvFor("bd", base, nil, standardOTELBd)

	if vals := envValues(got, "BD_OTEL_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_OTEL_ENABLED values = %v, want exactly [true]", vals)
	}
	if vals := envValues(got, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); len(vals) != 1 || vals[0] != "http://localhost:8428/opentelemetry/api/v1/push" {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT values = %v", vals)
	}
	if vals := envValues(got, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); len(vals) != 0 {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT values = %v, want none (logs URL was never set)", vals)
	}
}

func TestExecEnvForBd_LegacyMetricsURLLeavesInheritedLogsEndpoint(t *testing.T) {
	// A legacy URL replaces only its own signal's endpoint, as bd's own
	// translation does; the other signal's endpoint has no legacy
	// replacement and must reach bd untouched.
	base := []string{
		"PATH=/usr/bin",
		"BD_OTEL_METRICS_URL=http://localhost:8428/opentelemetry/api/v1/push",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://operator.example/v1/logs",
	}
	got := execEnvFor("bd", base, nil, standardOTELBd)

	if vals := envValues(got, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); len(vals) != 1 || vals[0] != "http://operator.example/v1/logs" {
		t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT values = %v, want the inherited endpoint kept", vals)
	}
	if vals := envValues(got, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); len(vals) != 1 || vals[0] != "http://localhost:8428/opentelemetry/api/v1/push" {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT values = %v", vals)
	}
}

func TestExecEnvForBd_LegacyOTELURLWinsOverStandardEndpoint(t *testing.T) {
	// bd's own startup translation lets a legacy URL overwrite its signal's
	// standard endpoint, so that is where bd exported before this
	// translation existed. Keep that precedence for an inherited endpoint and
	// for an explicit per-call one alike, so translating changes no export
	// destination and the layered and exact-env runners agree.
	const legacyURL = "http://localhost:8428/opentelemetry/api/v1/push"
	for _, tc := range []struct {
		name      string
		base      []string
		overrides map[string]string
	}{
		{
			name: "inherited endpoint",
			base: []string{
				"BD_OTEL_METRICS_URL=" + legacyURL,
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://operator.example/v1/metrics",
			},
		},
		{
			name:      "override endpoint",
			base:      []string{"BD_OTEL_METRICS_URL=" + legacyURL},
			overrides: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://override.example/v1/metrics"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := execEnvFor("bd", tc.base, tc.overrides, standardOTELBd)
			if vals := envValues(got, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); len(vals) != 1 || vals[0] != legacyURL {
				t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT values = %v, want exactly [%s]", vals, legacyURL)
			}
		})
	}
}

func TestExecEnvForBd_TranslatesLegacyOTELOverrides(t *testing.T) {
	// Translation runs on the merged env, so a legacy name passed as an
	// override is translated like an inherited one, and an override that
	// blanks the inherited name leaves nothing to translate.
	t.Run("override URL", func(t *testing.T) {
		got := execEnvFor("bd", []string{"PATH=/usr/bin"}, map[string]string{
			"BD_OTEL_LOGS_URL": "http://localhost:9428/insert/opentelemetry/v1/logs",
		}, standardOTELBd)
		if vals := envValues(got, "BD_OTEL_LOGS_URL"); len(vals) != 0 {
			t.Errorf("legacy BD_OTEL_LOGS_URL leaked through: %v", vals)
		}
		if vals := envValues(got, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); len(vals) != 1 || vals[0] != "http://localhost:9428/insert/opentelemetry/v1/logs" {
			t.Errorf("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT values = %v", vals)
		}
		if vals := envValues(got, "BD_OTEL_ENABLED"); len(vals) != 1 || vals[0] != "true" {
			t.Errorf("BD_OTEL_ENABLED values = %v, want exactly [true]", vals)
		}
	})
	t.Run("override blanks inherited URL", func(t *testing.T) {
		got := execEnvFor("bd", []string{"BD_OTEL_METRICS_URL=http://localhost:8428/opentelemetry/api/v1/push"},
			map[string]string{"BD_OTEL_METRICS_URL": ""}, otelProbeUnused(t))
		if vals := envValues(got, "BD_OTEL_ENABLED"); len(vals) != 0 {
			t.Errorf("BD_OTEL_ENABLED values = %v, want none once the override blanks the legacy URL", vals)
		}
	})
}

func TestExecEnvForBd_KeepsLegacyOTELForBdWithoutStandardNames(t *testing.T) {
	// bd v1.0.x, v1.1.x and v1.2.2 read only the legacy names: translating
	// would stop their export, so their env keeps the legacy names (and the
	// deprecation warning they do not print).
	base := []string{
		"PATH=/usr/bin",
		"BD_OTEL_METRICS_URL=http://localhost:8428/opentelemetry/api/v1/push",
		"BD_OTEL_LOGS_URL=http://localhost:9428/insert/opentelemetry/v1/logs",
	}
	got := execEnvFor("bd", base, nil, legacyOTELOnlyBd)

	if vals := envValues(got, "BD_OTEL_METRICS_URL"); len(vals) != 1 || vals[0] != "http://localhost:8428/opentelemetry/api/v1/push" {
		t.Errorf("BD_OTEL_METRICS_URL values = %v, want the legacy URL kept", vals)
	}
	if vals := envValues(got, "BD_OTEL_LOGS_URL"); len(vals) != 1 || vals[0] != "http://localhost:9428/insert/opentelemetry/v1/logs" {
		t.Errorf("BD_OTEL_LOGS_URL values = %v, want the legacy URL kept", vals)
	}
	for _, key := range []string{"BD_OTEL_ENABLED", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"} {
		if vals := envValues(got, key); len(vals) != 0 {
			t.Errorf("%s values = %v, want none for a bd that reads only the legacy names", key, vals)
		}
	}
}

func TestExecEnvForBd_LeavesStdoutExporterSelectionToBd(t *testing.T) {
	// BD_OTEL_STDOUT=true (any case) selects console exporters that bd maps
	// itself; the translation leaves that env, legacy URL included, to bd.
	for _, tc := range []struct {
		name string
		base []string
	}{
		{name: "with metrics URL", base: []string{"BD_OTEL_STDOUT=True", "BD_OTEL_METRICS_URL=http://localhost:8428/opentelemetry/api/v1/push"}},
		{name: "without URL", base: []string{"BD_OTEL_STDOUT=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := execEnvFor("bd", tc.base, nil, otelProbeUnused(t))
			for _, entry := range tc.base {
				key, value, _ := strings.Cut(entry, "=")
				if vals := envValues(got, key); len(vals) != 1 || vals[0] != value {
					t.Errorf("%s values = %v, want [%s] untouched", key, vals, value)
				}
			}
			if vals := envValues(got, "BD_OTEL_ENABLED"); len(vals) != 0 {
				t.Errorf("BD_OTEL_ENABLED values = %v, want none", vals)
			}
		})
	}
}

func TestExecEnvForBd_EnablesTranslatedTelemetryOverInheritedDisable(t *testing.T) {
	// To bd a legacy URL enables telemetry whatever BD_OTEL_ENABLED says, so
	// the translated env must say true, or removing the legacy selector would
	// switch the export off.
	base := []string{
		"BD_OTEL_ENABLED=false",
		"BD_OTEL_METRICS_URL=http://localhost:8428/opentelemetry/api/v1/push",
	}
	got := execEnvFor("bd", base, nil, standardOTELBd)

	if vals := envValues(got, "BD_OTEL_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_OTEL_ENABLED values = %v, want exactly [true]", vals)
	}
}

func TestExecEnvForBd_NoOTELEnvWhenNeitherLegacyURLSet(t *testing.T) {
	// A bd child with telemetry not configured must not be force-activated:
	// bd's own Enabled() check requires BD_OTEL_ENABLED=true or a legacy
	// selector, so we must never inject BD_OTEL_ENABLED=true unconditionally.
	base := []string{"PATH=/usr/bin"}
	got := execEnvFor("bd", base, nil, otelProbeUnused(t))

	for _, key := range []string{"BD_OTEL_ENABLED", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"} {
		if vals := envValues(got, key); len(vals) != 0 {
			t.Errorf("%s values = %v, want none when telemetry is not configured", key, vals)
		}
	}
}

func TestExecEnvForNonBd_LeavesLegacyOTELEnvAlone(t *testing.T) {
	// Non-bd commands (e.g. direct dolt queries) must never see the
	// translated standard OTEL names injected on their behalf — only the bd
	// exec path is scoped for this translation.
	base := []string{"PATH=/usr/bin", "BD_OTEL_METRICS_URL=http://localhost:8428/opentelemetry/api/v1/push"}
	got := execEnvFor("dolt", base, nil, otelProbeUnused(t))

	if vals := envValues(got, "BD_OTEL_METRICS_URL"); len(vals) != 1 || vals[0] != "http://localhost:8428/opentelemetry/api/v1/push" {
		t.Errorf("legacy BD_OTEL_METRICS_URL values = %v, want untouched for non-bd commands", vals)
	}
	if vals := envValues(got, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); len(vals) != 0 {
		t.Errorf("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT values = %v, want none for non-bd commands", vals)
	}
}

func TestExecCommandRunnerWithoutAmbientBeadsWithholdsNamespaceBeforeOverrides(t *testing.T) {
	t.Setenv("BEADS_DB", "ambient-database")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/ambient/helper")

	runner := ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"BEADS_DIR":                     "/selected/.beads",
		"BEADS_DOLT_CREDENTIAL_COMMAND": "/selected/gc internal beads-credential",
	})
	out, err := runner(t.TempDir(), "sh", "-c", `env | sort`)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, forbidden := range []string{
		"BEADS_DB=",
		"BEADS_DOLT_SERVER_HOST=",
		"BEADS_DOLT_CREDENTIAL_COMMAND=/ambient/helper",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("child environment contains withheld entry %q", forbidden)
		}
	}
	for _, want := range []string{
		"BEADS_DIR=/selected/.beads",
		"BEADS_DOLT_CREDENTIAL_COMMAND=/selected/gc internal beads-credential",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("child environment does not contain explicit override %q", want)
		}
	}
}

func TestExecCommandRunnerWithEnv_AbsoluteBDBinKeepsLogicalBdPolicy(t *testing.T) {
	// BD_BIN selects the physical executable for an otherwise logical `bd`
	// command. The logical identity must remain intact so the runner keeps the
	// bd-only environment policy rather than treating the pinned path as an
	// unrelated program.
	pinned := filepath.Join(t.TempDir(), "workspace-bd")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\nprintf 'pinned:%s\\n' \"$BD_BACKUP_ENABLED\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": pinned})(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("run workspace-pinned bd: %v", err)
	}
	if got, want := string(out), "pinned:false\n"; got != want {
		t.Fatalf("workspace-pinned bd output = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithEnv_InheritedAbsoluteBDBin(t *testing.T) {
	pinned := filepath.Join(t.TempDir(), "workspace-bd")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\nprintf 'pinned:%s\\n' \"$BD_BACKUP_ENABLED\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ambientDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ambientDir, "bd"), []byte("#!/bin/sh\nprintf 'ambient\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", pinned)
	t.Setenv("PATH", ambientDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := ExecCommandRunnerWithEnv(nil)(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("run inherited workspace-pinned bd: %v", err)
	}
	if got, want := string(out), "pinned:false\n"; got != want {
		t.Fatalf("inherited workspace-pinned bd output = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithEnv_ExplicitEmptyBDBinOverridesInheritedPin(t *testing.T) {
	pinned := filepath.Join(t.TempDir(), "workspace-bd")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\nprintf 'pinned\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ambientDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ambientDir, "bd"), []byte("#!/bin/sh\nprintf 'ambient:%s\\n' \"$BD_BACKUP_ENABLED\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", pinned)
	t.Setenv("PATH", ambientDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": ""})(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("run ambient bd after explicit pin clear: %v", err)
	}
	if got, want := string(out), "ambient:false\n"; got != want {
		t.Fatalf("ambient bd output = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithEnv_RelativeBDBinUsesAmbientBd(t *testing.T) {
	ambientDir := t.TempDir()
	ambient := filepath.Join(ambientDir, "bd")
	if err := os.WriteFile(ambient, []byte("#!/bin/sh\nprintf 'ambient:%s\\n' \"$BD_BACKUP_ENABLED\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ambientDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": "workspace-bd"})(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("run ambient bd: %v", err)
	}
	if got, want := string(out), "ambient:false\n"; got != want {
		t.Fatalf("ambient bd output = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithExactEnvContextExcludesParentValues(t *testing.T) {
	t.Setenv("BEADS_DB", "ambient.db")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "ambient-credential-helper")

	pinned := filepath.Join(t.TempDir(), "workspace-bd")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\nprintf '%s|%s|%s\\n' \"${BEADS_DB-unset}\" \"${BEADS_DOLT_CREDENTIAL_COMMAND-unset}\" \"$BEADS_DIR\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	runner := ExecCommandRunnerWithExactEnvContext(context.Background(), map[string]string{
		"BD_BIN":    pinned,
		"BEADS_DIR": "/selected/.beads",
	})
	out, err := runner(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("run workspace-pinned bd: %v", err)
	}
	if got, want := string(out), "unset|unset|/selected/.beads\n"; got != want {
		t.Fatalf("exact runner output = %q, want %q", got, want)
	}
}

// clearAmbientBdOTELEnv blanks every telemetry variable the bd OTEL
// translation reads or writes, so a runner test sees only the env it sets up
// even on a host that exports gc telemetry itself.
func clearAmbientBdOTELEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BD_OTEL_METRICS_URL",
		"BD_OTEL_LOGS_URL",
		"BD_OTEL_STDOUT",
		"BD_OTEL_ENABLED",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	} {
		t.Setenv(key, "")
	}
}

// writeOTELEnvFakeBd writes a fake bd that answers `bd version` with version,
// appending a line to probeLog for each such probe, and otherwise prints the
// telemetry environment it was started with ("-" for unset or empty).
func writeOTELEnvFakeBd(t *testing.T, version string) (bin, probeLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "bd")
	probeLog = filepath.Join(dir, "version-probes")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = version ]; then
	echo probe >> '%s'
	echo 'bd version %s (fake)'
	exit 0
fi
printf 'metrics_url=%%s logs_url=%%s enabled=%%s metrics_endpoint=%%s logs_endpoint=%%s\n' \
	"${BD_OTEL_METRICS_URL:--}" "${BD_OTEL_LOGS_URL:--}" "${BD_OTEL_ENABLED:--}" \
	"${OTEL_EXPORTER_OTLP_METRICS_ENDPOINT:--}" "${OTEL_EXPORTER_OTLP_LOGS_ENDPOINT:--}"
`, probeLog, version)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, probeLog
}

// versionProbeCount reports how many `bd version` probes a writeOTELEnvFakeBd
// binary has answered.
func versionProbeCount(t *testing.T, probeLog string) int {
	t.Helper()
	data, err := os.ReadFile(probeLog)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "probe\n")
}

func TestExecCommandRunnerWithEnv_TranslatesLegacyOTELOverrides(t *testing.T) {
	// Overrides are merged after the inherited env, so a legacy name passed
	// as an override must be translated too, not re-added verbatim.
	clearAmbientBdOTELEnv(t)
	bin, _ := writeOTELEnvFakeBd(t, "1.3.1")

	out, err := ExecCommandRunnerWithEnv(map[string]string{
		"BD_BIN":              bin,
		"BD_OTEL_METRICS_URL": "http://collector:4318/v1/metrics",
	})(t.TempDir(), "bd", "list")
	if err != nil {
		t.Fatalf("run bd: %v", err)
	}
	want := "metrics_url=- logs_url=- enabled=true metrics_endpoint=http://collector:4318/v1/metrics logs_endpoint=-\n"
	if got := string(out); got != want {
		t.Fatalf("bd env = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithExactEnvContext_TranslatesLegacyOTEL(t *testing.T) {
	// gc hook --claim runs its mutation in the exact query env, copied key by
	// key into the override map (hookClaimEnvMap), so the process-wide legacy
	// names arrive as overrides with no inherited env beneath them.
	bin, _ := writeOTELEnvFakeBd(t, "1.3.1")

	runner := ExecCommandRunnerWithExactEnvContext(context.Background(), map[string]string{
		"BD_BIN":              bin,
		"BEADS_DIR":           "/selected/.beads",
		"BD_OTEL_METRICS_URL": "http://collector:4318/v1/metrics",
		"BD_OTEL_LOGS_URL":    "http://collector:4318/v1/logs",
	})
	out, err := runner(t.TempDir(), "bd", "update")
	if err != nil {
		t.Fatalf("run bd: %v", err)
	}
	want := "metrics_url=- logs_url=- enabled=true metrics_endpoint=http://collector:4318/v1/metrics logs_endpoint=http://collector:4318/v1/logs\n"
	if got := string(out); got != want {
		t.Fatalf("bd env = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithEnv_KeepsLegacyOTELForBdWithoutStandardNames(t *testing.T) {
	// bd v1.2.2 reads only the legacy names (as do v1.0.x and v1.1.x):
	// translating them away would silently stop its telemetry export.
	clearAmbientBdOTELEnv(t)
	t.Setenv("BD_OTEL_METRICS_URL", "http://collector:4318/v1/metrics")
	bin, _ := writeOTELEnvFakeBd(t, "1.2.2")

	out, err := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": bin})(t.TempDir(), "bd", "list")
	if err != nil {
		t.Fatalf("run bd: %v", err)
	}
	want := "metrics_url=http://collector:4318/v1/metrics logs_url=- enabled=- metrics_endpoint=- logs_endpoint=-\n"
	if got := string(out); got != want {
		t.Fatalf("bd env = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithEnv_ProbesBdVersionOncePerBinary(t *testing.T) {
	clearAmbientBdOTELEnv(t)
	t.Setenv("BD_OTEL_METRICS_URL", "http://collector:4318/v1/metrics")
	bin, probeLog := writeOTELEnvFakeBd(t, "1.3.1")

	runner := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": bin})
	for range 3 {
		if _, err := runner(t.TempDir(), "bd", "list"); err != nil {
			t.Fatalf("run bd: %v", err)
		}
	}
	if got := versionProbeCount(t, probeLog); got != 1 {
		t.Fatalf("bd version probes = %d, want 1 across three commands", got)
	}
}

func TestExecCommandRunnerWithEnv_SkipsBdVersionProbeWithoutLegacyOTEL(t *testing.T) {
	// Without a legacy URL there is nothing to translate, so a city without
	// telemetry never pays for the version probe.
	clearAmbientBdOTELEnv(t)
	bin, probeLog := writeOTELEnvFakeBd(t, "1.3.1")

	if _, err := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": bin})(t.TempDir(), "bd", "list"); err != nil {
		t.Fatalf("run bd: %v", err)
	}
	if got := versionProbeCount(t, probeLog); got != 0 {
		t.Fatalf("bd version probes = %d, want 0 without legacy OTEL names", got)
	}
}

func TestBdStorePurgeEnvUsesBdExecEnvPolicy(t *testing.T) {
	// bd purge used to build its child env by hand, bypassing execEnvFor, so
	// it missed the auto-backup opt-out (and any later bd env policy such as
	// legacy OTEL name translation). It must get exactly the env every other
	// bd exec gets, plus its BEADS_DIR selection.
	clearAmbientBdOTELEnv(t)
	t.Setenv("BD_BACKUP_ENABLED", "true")
	t.Setenv("BEADS_DIR", "/ambient/.beads")

	const beadsDir = "/city/rigs/fe/.beads"
	var gotEnv []string
	s := NewBdStore("/city", nil)
	s.SetPurgeRunner(func(_ string, env []string, _ ...string) ([]byte, error) {
		gotEnv = env
		return []byte(`{"purged_count": 0}`), nil
	})
	if _, err := s.Purge(beadsDir, true); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	if vals := envValues(gotEnv, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("purge BD_BACKUP_ENABLED values = %v, want exactly [false]", vals)
	}
	if vals := envValues(gotEnv, "BEADS_DIR"); len(vals) != 1 || vals[0] != beadsDir {
		t.Errorf("purge BEADS_DIR values = %v, want exactly [%s]", vals, beadsDir)
	}
	want := execEnvFor("bd", processEnvSnapshotExcludingNativeDoltOpen(), map[string]string{"BEADS_DIR": beadsDir}, otelProbeUnused(t))
	if strings.Join(gotEnv, "\n") != strings.Join(want, "\n") {
		t.Errorf("purge env diverges from execEnvFor(bd) policy: got %d entries, want %d (BD_BACKUP_ENABLED got %v)",
			len(gotEnv), len(want), envValues(gotEnv, "BD_BACKUP_ENABLED"))
	}
}

func TestBdStorePurgeEnvGatesLegacyOTELOnPathBd(t *testing.T) {
	// execPurge runs the bd on PATH, so Purge probes that binary: the legacy
	// BD_OTEL_* names are translated for a bd that reads the standard OTEL
	// names and kept for one that reads only the legacy names.
	const url = "http://collector:4318/v1/metrics"
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"1.3.1", `metrics_url="" enabled="true" metrics_endpoint="` + url + `"`},
		{"1.2.2", `metrics_url="` + url + `" enabled="" metrics_endpoint=""`},
	} {
		t.Run(tc.version, func(t *testing.T) {
			clearAmbientBdOTELEnv(t)
			t.Setenv("BD_OTEL_METRICS_URL", url)
			bin, probeLog := writeOTELEnvFakeBd(t, tc.version)
			t.Setenv("PATH", filepath.Dir(bin))

			var gotEnv []string
			s := NewBdStore("/city", nil)
			s.SetPurgeRunner(func(_ string, env []string, _ ...string) ([]byte, error) {
				gotEnv = env
				return []byte(`{"purged_count": 0}`), nil
			})
			if _, err := s.Purge("/city/.beads", true); err != nil {
				t.Fatalf("Purge: %v", err)
			}

			got := fmt.Sprintf("metrics_url=%q enabled=%q metrics_endpoint=%q",
				lastEnvValue(gotEnv, "BD_OTEL_METRICS_URL"),
				lastEnvValue(gotEnv, "BD_OTEL_ENABLED"),
				lastEnvValue(gotEnv, "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"))
			if got != tc.want {
				t.Errorf("purge OTEL env = %s, want %s", got, tc.want)
			}
			if got := versionProbeCount(t, probeLog); got != 1 {
				t.Errorf("bd version probes = %d, want 1 of the PATH bd", got)
			}
		})
	}
}
