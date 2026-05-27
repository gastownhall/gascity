package coordstore

import (
	"context"
	"fmt"
	"math"
	"time"

	"golang.org/x/sys/unix"
)

const (
	estimatedTelemetrySampleBytes = 600
	estimatedChaosEventBytes      = 600
)

// SoakPreflightConfig configures the disk-capacity check that runs before
// long soak jobs.
type SoakPreflightConfig struct {
	ResultsDir    string
	SoakConfig    SoakConfig
	BackendCount  int
	AvailableFunc func(path string) (uint64, error)
}

// SoakPreflightResult records the capacity estimate and report artifact path.
type SoakPreflightResult struct {
	EstimatedBytes uint64
	RequiredBytes  uint64
	AvailableBytes uint64
	ReportPath     string
	Message        string
}

// EstimateSoakOutputBytes estimates JSONL output for a soak run.
func EstimateSoakOutputBytes(backendCount int, cfg SoakConfig) uint64 {
	if backendCount < 1 {
		backendCount = 1
	}
	duration := cfg.SoakDuration
	if duration <= 0 {
		duration = RealWorldWorkload.Duration
	}
	interval := cfg.SampleInterval
	if interval <= 0 {
		interval = time.Second
	}
	samples := uint64(math.Ceil(float64(duration) / float64(interval)))
	if samples < 1 {
		samples = 1
	}
	estimate := uint64(backendCount) * samples * estimatedTelemetrySampleBytes
	if cfg.KillCadence > 0 {
		events := uint64(math.Ceil(float64(duration) / float64(cfg.KillCadence)))
		if events < 1 {
			events = 1
		}
		estimate += uint64(backendCount) * events * estimatedChaosEventBytes
	}
	return estimate
}

// CheckSoakPreflight checks whether ResultsDir has enough free space and
// writes preflight-check.txt.
func CheckSoakPreflight(ctx context.Context, cfg SoakPreflightConfig) (SoakPreflightResult, error) {
	if err := ctx.Err(); err != nil {
		return SoakPreflightResult{}, err
	}
	soakCfg := normalizeSoakConfig(cfg.SoakConfig)
	resultsDir := cfg.ResultsDir
	if resultsDir == "" {
		resultsDir = soakCfg.ResultsDir
	}
	availableFunc := cfg.AvailableFunc
	if availableFunc == nil {
		availableFunc = availableDiskBytes
	}
	estimated := EstimateSoakOutputBytes(cfg.BackendCount, soakCfg)
	required := estimated * 2
	available, err := availableFunc(resultsDir)
	if err != nil {
		return SoakPreflightResult{}, fmt.Errorf("checking results dir free space: %w", err)
	}

	result := SoakPreflightResult{
		EstimatedBytes: estimated,
		RequiredBytes:  required,
		AvailableBytes: available,
		ReportPath:     pathJoin(resultsDir, "preflight-check.txt"),
	}
	status := "OK"
	if available < required {
		status = "WARNING"
	}
	result.Message = fmt.Sprintf("%s coordstore soak preflight: estimated_bytes=%d required_bytes=%d available_bytes=%d results_dir=%s",
		status, estimated, required, available, resultsDir)
	body := fmt.Sprintf("%s\nestimated_bytes=%d\nrequired_bytes=%d\navailable_bytes=%d\nresults_dir=%s\n",
		result.Message, estimated, required, available, resultsDir)
	if err := writeFileAtomic(result.ReportPath, []byte(body), 0o644); err != nil {
		return result, err
	}
	if available < required {
		return result, fmt.Errorf("coordstore soak preflight: available space %d is below required %d", available, required)
	}
	return result, nil
}

func availableDiskBytes(path string) (uint64, error) {
	if err := ensureDir(path); err != nil {
		return 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
