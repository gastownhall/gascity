//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	adoptionLatencyOutcomeReady         = "ready"
	adoptionLatencyOutcomeIncomplete    = "incomplete"
	adoptionLatencyOutcomeError         = "error"
	adoptionLatencyOutcomeNotAttempted  = "not_attempted"
	adoptionLatencyStoreCensorThreshold = time.Second
)

var (
	adoptionLatencyRequiredStartupPhases = []string{"adoption-barrier", "config-reload", "startup-orders", "startup-route-recovery", "startup", "convergence-startup"}
	adoptionLatencyRequiredBinaries      = []string{"gc", "bd_shim", "bd_payload", "dolt_wrapper", "dolt_payload", "tmux"}
)

type adoptionLatencyBinary struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}
type adoptionLatencyBenchmarkProfile struct {
	Version                  string `json:"version"`
	BeadsProvider            string `json:"beads_provider"`
	ShadowMode               string `json:"shadow_mode"`
	DispatcherMode           string `json:"dispatcher_mode"`
	ReconcilerMode           string `json:"reconciler_mode"`
	PreserveSessionsOnSignal bool   `json:"preserve_sessions_on_signal"`
	MeasurementMethod        string `json:"measurement_method"`
}
type adoptionLatencyProvenance struct {
	Error            string                          `json:"error,omitempty"`
	BenchmarkProfile adoptionLatencyBenchmarkProfile `json:"benchmark_profile"`
	Binaries         []adoptionLatencyBinary         `json:"binaries"`
	GCCommit         string                          `json:"gc_commit"`
	RuntimeProvider  string                          `json:"runtime_provider"`
	RuntimeIdentity  string                          `json:"runtime_identity"`
	HostOS           string                          `json:"host_os"`
	HostArch         string                          `json:"host_arch"`
	CPUCount         int                             `json:"cpu_count"`
}
type adoptionLatencyPhase struct {
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration_ns"`
}
type adoptionLatencySample struct {
	Index             int                    `json:"index"`
	Outcome           string                 `json:"outcome"`
	Duration          time.Duration          `json:"duration_ns,omitempty"`
	Error             string                 `json:"error,omitempty"`
	Phases            []adoptionLatencyPhase `json:"phases,omitempty"`
	StartingBeadStore *time.Duration         `json:"starting_bead_store_ns,omitempty"`
}
type adoptionLatencyPercentiles struct {
	Count int           `json:"count"`
	P50   time.Duration `json:"p50_ns"`
	P95   time.Duration `json:"p95_ns"`
	P99   time.Duration `json:"p99_ns"`
	Max   time.Duration `json:"max_ns"`
}
type adoptionLatencyPhaseStats struct {
	Name                    string                      `json:"name"`
	ObservedCount           int                         `json:"observed_count"`
	CensoredCount           int                         `json:"censored_count"`
	CensorThreshold         time.Duration               `json:"censor_threshold_ns,omitempty"`
	PercentilesObservedOnly bool                        `json:"percentiles_observed_only"`
	Latency                 *adoptionLatencyPercentiles `json:"latency,omitempty"`
}
type adoptionLatencyReport struct {
	SchemaVersion string                      `json:"schema_version"`
	Provenance    adoptionLatencyProvenance   `json:"provenance"`
	Expected      int                         `json:"expected_samples"`
	Warmup        adoptionLatencySample       `json:"warmup"`
	OK            bool                        `json:"ok"`
	FatalError    string                      `json:"fatal_error,omitempty"`
	Samples       []adoptionLatencySample     `json:"samples"`
	Latency       *adoptionLatencyPercentiles `json:"latency,omitempty"`
	PhaseStats    []adoptionLatencyPhaseStats `json:"phase_stats"`
}
type adoptionLatencyRunState struct {
	Requested  int
	Warmup     adoptionLatencySample
	Samples    []adoptionLatencySample
	FatalError string
}

func buildAdoptionLatencyReport(p adoptionLatencyProvenance, run adoptionLatencyRunState) (adoptionLatencyReport, error) {
	report := adoptionLatencyReport{SchemaVersion: "1", Provenance: p, Expected: run.Requested, Warmup: run.Warmup, FatalError: strings.TrimSpace(run.FatalError), Samples: run.Samples}
	var durations, stores []time.Duration
	phaseDurations := make([][]time.Duration, len(adoptionLatencyRequiredStartupPhases))
	for i, sample := range run.Samples {
		if sample.Index != i+1 {
			return adoptionLatencyReport{}, fmt.Errorf("sample %d: index must be %d", i, i+1)
		}
		if sample.Outcome != adoptionLatencyOutcomeReady {
			continue
		}
		if !adoptionLatencyReadySampleValid(sample) {
			return adoptionLatencyReport{}, fmt.Errorf("sample %d: invalid ready measurement", sample.Index)
		}
		durations = append(durations, sample.Duration)
		for j, phase := range sample.Phases {
			phaseDurations[j] = append(phaseDurations[j], phase.Duration)
		}
		if sample.StartingBeadStore != nil {
			stores = append(stores, *sample.StartingBeadStore)
		}
	}
	report.Latency = adoptionLatencyPercentileStats(durations)
	for i, name := range adoptionLatencyRequiredStartupPhases {
		report.PhaseStats = append(report.PhaseStats, adoptionLatencyPhaseStats{Name: name, ObservedCount: len(phaseDurations[i]), Latency: adoptionLatencyPercentileStats(phaseDurations[i])})
	}
	report.PhaseStats = append(report.PhaseStats, adoptionLatencyPhaseStats{Name: "starting_bead_store", ObservedCount: len(stores), CensoredCount: len(durations) - len(stores), CensorThreshold: adoptionLatencyStoreCensorThreshold, PercentilesObservedOnly: true, Latency: adoptionLatencyPercentileStats(stores)})
	provenanceOK := p.Error == "" && p.BenchmarkProfile == adoptionLatencyBenchmarkProfileV1() &&
		p.GCCommit != "" && p.RuntimeProvider == "tmux" && p.RuntimeIdentity != "" && p.HostOS != "" && p.HostArch != "" && p.CPUCount > 0 && slices.EqualFunc(p.Binaries, adoptionLatencyRequiredBinaries, func(binary adoptionLatencyBinary, name string) bool {
		return binary.Name == name && binary.Path != "" && binary.SHA256 != "" && binary.Version != ""
	})
	report.OK = provenanceOK && len(run.Samples) == run.Requested && len(durations) == run.Requested && adoptionLatencyReadySampleValid(run.Warmup) && report.FatalError == ""
	return report, nil
}

func adoptionLatencyBenchmarkProfileV1() adoptionLatencyBenchmarkProfile {
	return adoptionLatencyBenchmarkProfile{
		Version:                  "1",
		BeadsProvider:            "bd",
		ShadowMode:               "required",
		DispatcherMode:           "supervisor",
		ReconcilerMode:           "off",
		PreserveSessionsOnSignal: true,
		MeasurementMethod:        "sigterm_to_api_city_running_session_active_and_exact_tmux_identity",
	}
}

func adoptionLatencyReadySampleValid(sample adoptionLatencySample) bool {
	return sample.Outcome == adoptionLatencyOutcomeReady && sample.Duration > 0 &&
		slices.EqualFunc(sample.Phases, adoptionLatencyRequiredStartupPhases, func(phase adoptionLatencyPhase, name string) bool {
			return phase.Name == name && phase.Duration >= 0
		}) && (sample.StartingBeadStore == nil || *sample.StartingBeadStore >= adoptionLatencyStoreCensorThreshold)
}

func adoptionLatencyPercentileStats(values []time.Duration) *adoptionLatencyPercentiles {
	if len(values) == 0 {
		return nil
	}
	values = append([]time.Duration(nil), values...)
	slices.Sort(values)
	rank := func(percent int) time.Duration { return values[(len(values)*percent+99)/100-1] }
	return &adoptionLatencyPercentiles{Count: len(values), P50: rank(50), P95: rank(95), P99: rank(99), Max: values[len(values)-1]}
}

func writeAdoptionLatencyReport(path string, p adoptionLatencyProvenance, run adoptionLatencyRunState) error {
	report, err := buildAdoptionLatencyReport(p, run)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write adoption latency report: %w", err)
	}
	return nil
}

func parseAdoptionStartupPhases(log []byte) ([]adoptionLatencyPhase, *time.Duration, error) {
	found := make(map[string]time.Duration, len(adoptionLatencyRequiredStartupPhases))
	var store *time.Duration
	for _, line := range strings.Split(string(log), "\n") {
		if marker := strings.Index(line, "starting_bead_store took "); marker >= 0 {
			d, err := time.ParseDuration(strings.TrimSpace(line[marker+len("starting_bead_store took "):]))
			if err != nil || d < adoptionLatencyStoreCensorThreshold || store != nil {
				return nil, nil, fmt.Errorf("invalid starting_bead_store observation in %q", line)
			}
			store = &d
			continue
		}
		marker := strings.Index(line, "startup phase=")
		if marker < 0 {
			continue
		}
		fields := strings.Fields(line[marker+len("startup phase="):])
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "elapsed=") {
			return nil, nil, fmt.Errorf("malformed startup phase line %q", line)
		}
		name := fields[0]
		if !slices.Contains(adoptionLatencyRequiredStartupPhases, name) {
			continue
		}
		d, err := time.ParseDuration(strings.TrimPrefix(fields[1], "elapsed="))
		if err != nil || d < 0 {
			return nil, nil, fmt.Errorf("invalid startup phase duration in %q", line)
		}
		if _, duplicate := found[name]; duplicate {
			return nil, nil, fmt.Errorf("duplicate startup phase %q", name)
		}
		found[name] = d
	}
	phases := make([]adoptionLatencyPhase, len(adoptionLatencyRequiredStartupPhases))
	for i, name := range adoptionLatencyRequiredStartupPhases {
		d, ok := found[name]
		if !ok {
			return nil, nil, fmt.Errorf("missing required startup phase %q", name)
		}
		phases[i] = adoptionLatencyPhase{Name: name, Duration: d}
	}
	return phases, store, nil
}
