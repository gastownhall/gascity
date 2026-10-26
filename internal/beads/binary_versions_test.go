package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseBDVersion(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "version subcommand", in: "bd version 1.0.4 (ce242a879: HEAD@ce242a879678)", want: "1.0.4"},
		{name: "version flag", in: "bd version 1.0.4 (ce242a879)", want: "1.0.4"},
		{name: "v prefix", in: "bd version v1.2.0", want: "1.2.0"},
		{name: "trailing newline", in: "bd version 1.0.4\n", want: "1.0.4"},
		{name: "multiline takes first", in: "bd version 1.0.4\nschema 7", want: "1.0.4"},
		{name: "warning line first", in: "warning: BD_OTEL_* environment variables are deprecated\nbd version 1.1.0 (abc1234)\n", want: "1.1.0"},
		{name: "digit-led log line first", in: "2026/10/06 12:00:00 notice\nbd version 1.3.1\n", want: "1.3.1"},
		{name: "bare token", in: "1.0.4", want: "1.0.4"},
		{name: "empty", in: "", wantErr: true},
		{name: "prefix only", in: "bd version ", wantErr: true},
		{name: "non-version garbage", in: "garbage output with no version", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBDVersion(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseBDVersion(%q) err = nil, want error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBDVersion(%q) unexpected err: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseBDVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestProbeVersionsAgainstHostBinaries is a best-effort smoke test: when bd and
// dolt are on PATH (as in CI and dev shells), the probes return a digit-led
// version. Skips cleanly when a binary is absent so the suite stays hermetic.
func TestProbeVersionsAgainstHostBinaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func() (string, error)
	}{
		{"bd", func() (string, error) { return ProbeBDVersion("") }},
		{"dolt", ProbeDoltVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := tc.probe()
			if err != nil {
				t.Skipf("%s not probeable in this environment: %v", tc.name, err)
			}
			if v == "" || v[0] < '0' || v[0] > '9' {
				t.Errorf("%s version = %q, want a digit-led version string", tc.name, v)
			}
		})
	}
}

// TestProbeBDVersionRespectsPreferredBin pins that a city's pinned BD_BIN is
// the binary whose version is reported, not whichever "bd" happens to resolve
// first on the caller's PATH.
func TestProbeBDVersionRespectsPreferredBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	dir := t.TempDir()
	fakeBD := filepath.Join(dir, "fake-bd")
	script := "#!/bin/sh\necho 'bd version 9.9.9 (fakepin)'\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable
		t.Fatalf("write fake bd: %v", err)
	}

	got, err := ProbeBDVersion(fakeBD)
	if err != nil {
		t.Fatalf("ProbeBDVersion(%q) error = %v", fakeBD, err)
	}
	if got != "9.9.9" {
		t.Errorf("ProbeBDVersion(%q) = %q, want %q (the pinned binary's version, not PATH's bd)", fakeBD, got, "9.9.9")
	}
}

// TestProbeBDVersionEmptyPreferredBinFallsBackToPATH pins that an empty
// preferredBin probes the "bd" PATH resolves, and that a warning that bd
// prints ahead of its version line does not hide the version.
func TestProbeBDVersionEmptyPreferredBinFallsBackToPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'warning: BD_OTEL_* environment variables are deprecated' >&2\necho 'bd version 9.8.7 (fakepath)'\n"
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", dir)

	got, err := ProbeBDVersion("")
	if err != nil {
		t.Fatalf("ProbeBDVersion(\"\") error = %v", err)
	}
	if got != "9.8.7" {
		t.Errorf("ProbeBDVersion(\"\") = %q, want %q (the version of the bd on PATH)", got, "9.8.7")
	}
}

// TestProbeBDVersionRunsWithoutBdTelemetryEnv pins that `bd version` runs
// without the BD_OTEL_* names that switch on bd's telemetry, so the probe
// never exports or waits on a collector, while the rest of the environment
// still reaches bd.
func TestProbeBDVersionRunsWithoutBdTelemetryEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	dir := t.TempDir()
	envLog := filepath.Join(dir, "probe-env")
	fakeBD := filepath.Join(dir, "fake-bd")
	script := fmt.Sprintf("#!/bin/sh\nexport -p > '%s'\necho 'bd version 1.3.1 (fake)'\n", envLog)
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv(bdLegacyOTELMetricsURLKey, "http://127.0.0.1:4318/v1/metrics")
	t.Setenv(bdLegacyOTELLogsURLKey, "http://127.0.0.1:4318/v1/logs")
	t.Setenv(bdLegacyOTELStdoutKey, "true")
	t.Setenv(bdOTELEnabledEnvKey, "true")
	t.Setenv("GC_BD_PROBE_ENV_SENTINEL", "kept")

	got, err := ProbeBDVersion(fakeBD)
	if err != nil {
		t.Fatalf("ProbeBDVersion(%q) error = %v", fakeBD, err)
	}
	if got != "1.3.1" {
		t.Errorf("ProbeBDVersion(%q) = %q, want %q", fakeBD, got, "1.3.1")
	}
	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("read the env the probe ran with: %v", err)
	}
	// `export -p` prints "export NAME=..." (dash, POSIX-mode bash) or
	// "declare -x NAME=..." (bash).
	for _, line := range strings.Split(string(data), "\n") {
		name := strings.TrimPrefix(strings.TrimPrefix(line, "declare -x "), "export ")
		if strings.HasPrefix(name, "BD_OTEL_") {
			t.Errorf("bd version probe env carries %q, want no BD_OTEL_* names", line)
		}
	}
	if !strings.Contains(string(data), "GC_BD_PROBE_ENV_SENTINEL") {
		t.Errorf("bd version probe env lost GC_BD_PROBE_ENV_SENTINEL, want the rest of the env passed through:\n%s", data)
	}
}

// TestBdVersionReadsStandardOTELEnv pins which bd releases read
// BD_OTEL_ENABLED plus the standard OTEL endpoint names. v1.2.2 was cut from
// the v1.1 line without the change, so a plain minimum-version check would
// strip the only names it reads.
func TestBdVersionReadsStandardOTELEnv(t *testing.T) {
	for _, tt := range []struct {
		version string
		want    bool
	}{
		{version: "0.62.0", want: false},
		{version: "1.0.4", want: false},
		{version: "1.1.0", want: false},
		{version: "1.2.0", want: true},
		{version: "1.2.1", want: true},
		{version: "1.2.2-rc.1", want: false},
		{version: "1.2.2", want: false},
		{version: "1.3.0-rc.1", want: true},
		{version: "1.3.0", want: true},
		{version: "1.3.1", want: true},
		{version: "1.3.2-rc.2", want: true},
		{version: "2.0.0", want: true},
		{version: "", want: false},
	} {
		if got := bdVersionReadsStandardOTELEnv(tt.version); got != tt.want {
			t.Errorf("bdVersionReadsStandardOTELEnv(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

func TestBdReadsStandardOTELEnvReprobesReplacedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	bin, oldProbes := writeOTELEnvFakeBd(t, "1.2.2")
	if bdReadsStandardOTELEnv(context.Background(), bin) {
		t.Fatal("bd 1.2.2 reported as reading the standard OTEL names")
	}

	// Install a same-size upgrade over the binary the way an installer
	// does, so only the modification time tells the two binaries apart.
	upgrade, newProbes := writeOTELEnvFakeBd(t, "1.3.1")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(upgrade, later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(upgrade, bin); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if !bdReadsStandardOTELEnv(context.Background(), bin) {
			t.Fatal("upgraded bd 1.3.1 still reported as reading only the legacy names")
		}
	}
	if got := versionProbeCount(t, oldProbes) + versionProbeCount(t, newProbes); got != 2 {
		t.Fatalf("bd version probes = %d, want one per installed binary", got)
	}
}

func TestBdReadsStandardOTELEnvCachesFailedProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary script uses a POSIX shebang")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	probeLog := filepath.Join(dir, "version-probes")
	script := "#!/bin/sh\necho probe >> '" + probeLog + "'\necho 'unknown command \"version\"' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if bdReadsStandardOTELEnv(context.Background(), bin) {
			t.Fatal("bd with an unreadable version reported as reading the standard OTEL names")
		}
	}
	if got := versionProbeCount(t, probeLog); got != 1 {
		t.Fatalf("bd version probes = %d, want 1 (a failed probe is not retried per command)", got)
	}
}

func TestBdReadsStandardOTELEnvUnresolvableBinary(t *testing.T) {
	if bdReadsStandardOTELEnv(context.Background(), filepath.Join(t.TempDir(), "missing-bd")) {
		t.Fatal("missing bd reported as reading the standard OTEL names")
	}
}
