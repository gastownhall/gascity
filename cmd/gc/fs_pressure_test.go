//go:build linux

package main

import (
	"bytes"
	"strings"
	"testing"
)

// withFakePressureFile swaps fsPressureReadFile / fsPressurePath for the
// duration of the test. Tests MUST be hermetic and never touch real /proc.
func withFakePressureFile(t *testing.T, path string, content []byte, readErr error) {
	t.Helper()
	origRead := fsPressureReadFile
	origPath := fsPressurePath
	fsPressurePath = path
	fsPressureReadFile = func(p string) ([]byte, error) {
		if p != path {
			t.Fatalf("readFile called with unexpected path %q (want %q)", p, path)
		}
		if readErr != nil {
			return nil, readErr
		}
		return content, nil
	}
	t.Cleanup(func() {
		fsPressureReadFile = origRead
		fsPressurePath = origPath
	})
}

const samplePressureLow = `some avg10=0.00 avg60=1.23 avg300=0.50 total=12345
full avg10=0.00 avg60=0.11 avg300=0.05 total=2345
`

const samplePressureHigh = `some avg10=80.12 avg60=75.45 avg300=60.10 total=999999
full avg10=40.00 avg60=30.00 avg300=20.00 total=77777
`

func TestFSPressureReadAvg60_Low(t *testing.T) {
	withFakePressureFile(t, "/fake/pressure/io", []byte(samplePressureLow), nil)
	v, err := readFSPressureAvg60(fsPressurePath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 1.23 {
		t.Fatalf("expected 1.23, got %v", v)
	}
}

func TestFSPressureReadAvg60_High(t *testing.T) {
	withFakePressureFile(t, "/fake/pressure/io", []byte(samplePressureHigh), nil)
	v, err := readFSPressureAvg60(fsPressurePath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 75.45 {
		t.Fatalf("expected 75.45, got %v", v)
	}
}

func TestFSPressureReadAvg60_MalformedNoSomeLine(t *testing.T) {
	withFakePressureFile(t, "/fake/pressure/io", []byte("garbage\nmore garbage\n"), nil)
	if _, err := readFSPressureAvg60(fsPressurePath); err == nil {
		t.Fatal("expected error for malformed file, got nil")
	}
}

func TestFSPressureReadAvg60_MalformedNoAvg60(t *testing.T) {
	withFakePressureFile(t, "/fake/pressure/io", []byte("some avg10=0.00 total=1\n"), nil)
	if _, err := readFSPressureAvg60(fsPressurePath); err == nil {
		t.Fatal("expected error when avg60 missing, got nil")
	}
}

func TestFSPressureReadAvg60_UnparseableNumber(t *testing.T) {
	withFakePressureFile(t, "/fake/pressure/io", []byte("some avg10=0.00 avg60=NOT_A_NUM total=1\n"), nil)
	if _, err := readFSPressureAvg60(fsPressurePath); err == nil {
		t.Fatal("expected error when avg60 unparseable, got nil")
	}
}

func TestShouldSkipTickForFSPressure_Below(t *testing.T) {
	withFakePressureFile(t, "/fake/pressure/io", []byte(samplePressureLow), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	var buf bytes.Buffer
	if shouldSkipTickForFSPressure(&buf) {
		t.Fatal("expected to NOT skip tick when avg60 below threshold")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log output when not skipping, got %q", buf.String())
	}
}

func TestShouldSkipTickForFSPressure_Above(t *testing.T) {
	withFakePressureFile(t, "/fake/pressure/io", []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	var buf bytes.Buffer
	if !shouldSkipTickForFSPressure(&buf) {
		t.Fatal("expected to skip tick when avg60 above threshold")
	}
	out := buf.String()
	if !strings.Contains(out, "supervisor: FS pressure high") {
		t.Fatalf("expected warning log, got %q", out)
	}
	if !strings.Contains(out, "avg60=75.45") {
		t.Fatalf("expected avg60=75.45 in log, got %q", out)
	}
	if !strings.Contains(out, "threshold=50.0") {
		t.Fatalf("expected threshold=50.0 in log, got %q", out)
	}
	if !strings.Contains(out, "skipping tick") {
		t.Fatalf("expected 'skipping tick' in log, got %q", out)
	}
}

func TestShouldSkipTickForFSPressure_MalformedProceeds(t *testing.T) {
	// Malformed file -> readFSPressureAvg60 returns error -> gate fails open.
	withFakePressureFile(t, "/fake/pressure/io", []byte("not a PSI file"), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	var buf bytes.Buffer
	if shouldSkipTickForFSPressure(&buf) {
		t.Fatal("expected to proceed (fail open) on malformed pressure file")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log output when failing open, got %q", buf.String())
	}
}

func TestShouldSkipTickForFSPressure_MissingFileProceeds(t *testing.T) {
	withFakePressureFile(t, "/fake/pressure/io", nil, errFakeMissing{})
	t.Setenv(fsPressureThresholdEnv, "")
	var buf bytes.Buffer
	if shouldSkipTickForFSPressure(&buf) {
		t.Fatal("expected to proceed when pressure file cannot be read")
	}
}

type errFakeMissing struct{}

func (errFakeMissing) Error() string { return "fake: file not found" }

func TestFSPressureThreshold_EnvOverride(t *testing.T) {
	// With low pressure but a very low threshold, we should now skip.
	withFakePressureFile(t, "/fake/pressure/io", []byte(samplePressureLow), nil)
	t.Setenv(fsPressureThresholdEnv, "1.0") // 1.23 > 1.0 -> skip
	var buf bytes.Buffer
	if !shouldSkipTickForFSPressure(&buf) {
		t.Fatal("expected to skip when env threshold overridden below measured value")
	}
	if !strings.Contains(buf.String(), "threshold=1.0") {
		t.Fatalf("expected threshold=1.0 in log, got %q", buf.String())
	}
}

func TestFSPressureThreshold_EnvInvalidFallsBackToDefault(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "not-a-number")
	if got := fsPressureThreshold(); got != defaultFSPressureThreshold {
		t.Fatalf("expected default %v on invalid env, got %v", defaultFSPressureThreshold, got)
	}
}

func TestFSPressureThreshold_EnvValidOverride(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "25.5")
	if got := fsPressureThreshold(); got != 25.5 {
		t.Fatalf("expected 25.5, got %v", got)
	}
}

func TestFSPressureThreshold_EnvUnsetUsesDefault(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "")
	if got := fsPressureThreshold(); got != defaultFSPressureThreshold {
		t.Fatalf("expected default %v, got %v", defaultFSPressureThreshold, got)
	}
}
