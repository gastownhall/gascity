package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
