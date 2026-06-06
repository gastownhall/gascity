package coordstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEstimateSoakOutputBytesAccountsForBackendsSamplesAndChaos(t *testing.T) {
	cfg := SoakConfig{
		SoakDuration:   10 * time.Second,
		SampleInterval: time.Second,
		KillCadence:    2 * time.Second,
	}

	withoutChaos := EstimateSoakOutputBytes(4, SoakConfig{
		SoakDuration:   cfg.SoakDuration,
		SampleInterval: cfg.SampleInterval,
	})
	withChaos := EstimateSoakOutputBytes(4, cfg)

	if withoutChaos < 4*10*600 {
		t.Fatalf("estimate = %d, want at least samples*600", withoutChaos)
	}
	if withChaos <= withoutChaos {
		t.Fatalf("chaos estimate = %d, want > non-chaos estimate %d", withChaos, withoutChaos)
	}
}

func TestSoakPreflightAbortsAndWritesWarningWhenDiskTooSmall(t *testing.T) {
	resultsDir := t.TempDir()
	cfg := SoakConfig{
		ResultsDir:     resultsDir,
		SoakDuration:   time.Minute,
		SampleInterval: time.Second,
		KillCadence:    10 * time.Second,
	}
	estimated := EstimateSoakOutputBytes(4, cfg)

	result, err := CheckSoakPreflight(context.Background(), SoakPreflightConfig{
		ResultsDir:    resultsDir,
		SoakConfig:    cfg,
		BackendCount:  4,
		AvailableFunc: func(string) (uint64, error) { return estimated*2 - 1, nil },
	})
	if err == nil {
		t.Fatalf("CheckSoakPreflight succeeded, want low-disk error")
	}
	if result.RequiredBytes != estimated*2 {
		t.Fatalf("required bytes = %d, want %d", result.RequiredBytes, estimated*2)
	}
	if result.ReportPath != filepath.Join(resultsDir, "preflight-check.txt") {
		t.Fatalf("report path = %q", result.ReportPath)
	}
	data, readErr := os.ReadFile(result.ReportPath)
	if readErr != nil {
		t.Fatalf("read preflight report: %v", readErr)
	}
	report := string(data)
	for _, want := range []string{"WARNING", "available_bytes", "estimated_bytes", "required_bytes"} {
		if !strings.Contains(report, want) {
			t.Fatalf("preflight report missing %q:\n%s", want, report)
		}
	}
}
