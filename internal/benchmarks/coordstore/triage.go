package coordstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Percentile contains latency percentile values expressed in milliseconds.
type Percentile struct {
	P50  float64 `json:"p50_ms"`
	P99  float64 `json:"p99_ms"`
	P999 float64 `json:"p999_ms"`
}

// TriageBackend contains synthesized metrics for one benchmark backend.
type TriageBackend struct {
	Name               string     `json:"name"`
	GetP99Ms           Percentile `json:"get_p99_ms"`
	CreateP99Ms        Percentile `json:"create_p99_ms"`
	MailPollP99Ms      Percentile `json:"mail_poll_p99_ms"`
	RSSCeilingMB       float64    `json:"rss_ceiling_mb"`
	RSSGrowthMBPerHour float64    `json:"rss_growth_mb_per_hour"`
	GoroutineP99       int        `json:"goroutine_p99"`
	KillEvents         int        `json:"kill_events"`
	LostRecords        int        `json:"lost_records"`
	RecoveryP99Ms      float64    `json:"recovery_p99_ms"`
	ForeverTax         string     `json:"forever_tax"`
}

// TriageReport contains synthesized metrics for a coordstore soak run.
type TriageReport struct {
	Backends    []TriageBackend `json:"backends"`
	RunID       string          `json:"run_id"`
	GeneratedAt time.Time       `json:"generated_at"`
}

var foreverTaxByBackend = map[string]string{
	"hqstore":    "~9d build + ~15-20d/yr maintain + own crash-recovery bugs",
	"boltdb":     "~3d + own WAL risk (reversible)",
	"bbolt":      "~3d + own WAL risk (reversible)",
	"sqlite-cgo": "~3d (fully reversible)",
	"sqlite":     "~3d (fully reversible)",
	"badger":     "~3d (fully reversible)",
}

type triageAccumulator struct {
	runIDs    map[string]struct{}
	samples   []TelemetrySample
	killEvent []KillEvent
}

// Synthesize reads timeseries and kill-event JSONL artifacts under resultsDir.
func (r *TriageReport) Synthesize(ctx context.Context, resultsDir string) error {
	if resultsDir == "" {
		return fmt.Errorf("triage results dir is required")
	}
	accumulators := make(map[string]*triageAccumulator)
	err := filepath.WalkDir(resultsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base != "timeseries.jsonl" && base != "kill-events.jsonl" {
			return nil
		}
		runID, backend, ok := artifactIdentity(resultsDir, path)
		if !ok {
			return nil
		}
		acc := accumulators[backend]
		if acc == nil {
			acc = &triageAccumulator{runIDs: make(map[string]struct{})}
			accumulators[backend] = acc
		}
		acc.runIDs[runID] = struct{}{}
		switch base {
		case "timeseries.jsonl":
			samples, err := readTelemetrySamples(path)
			if err != nil {
				return err
			}
			acc.samples = append(acc.samples, samples...)
		case "kill-events.jsonl":
			events, err := readKillEvents(path)
			if err != nil {
				return err
			}
			acc.killEvent = append(acc.killEvent, events...)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking triage results %s: %w", resultsDir, err)
	}
	if len(accumulators) == 0 {
		return fmt.Errorf("triage found no timeseries or kill-event artifacts under %s", resultsDir)
	}

	names := make([]string, 0, len(accumulators))
	runIDs := make(map[string]struct{})
	for name, acc := range accumulators {
		names = append(names, name)
		for runID := range acc.runIDs {
			runIDs[runID] = struct{}{}
		}
	}
	sort.Strings(names)
	backends := make([]TriageBackend, 0, len(names))
	for _, name := range names {
		backends = append(backends, synthesizeBackend(name, accumulators[name]))
	}
	r.Backends = backends
	r.RunID = joinSortedKeys(runIDs)
	r.GeneratedAt = time.Now().UTC()
	return nil
}

// PrintMarkdown writes a Markdown triage table and interpretation summary.
func (r TriageReport) PrintMarkdown(w io.Writer) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Coordstore Soak Triage\n\n")                                                                                                                                                     //nolint:errcheck
	fmt.Fprintf(&buf, "Run ID: %s\n\n", r.RunID)                                                                                                                                                          //nolint:errcheck
	fmt.Fprintf(&buf, "| Backend | Get p99 ms | Create p99 ms | Mail poll p99 ms | RSS ceiling MB | RSS growth MB/hour | Goroutine p99 | Kill events | Lost records | Recovery p99 ms | Forever tax |\n") //nolint:errcheck
	fmt.Fprintf(&buf, "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")                                                                                                   //nolint:errcheck
	for _, b := range r.Backends {
		fmt.Fprintf(&buf, "| %s | %.3f | %.3f | %.3f | %.1f | %.1f | %d | %d | %d | %.3f | %s |\n",
			b.Name, b.GetP99Ms.P99, b.CreateP99Ms.P99, b.MailPollP99Ms.P99,
			b.RSSCeilingMB, b.RSSGrowthMBPerHour, b.GoroutineP99, b.KillEvents,
			b.LostRecords, b.RecoveryP99Ms, b.ForeverTax) //nolint:errcheck
	}
	fmt.Fprintf(&buf, "\n## Interpretation\n\n") //nolint:errcheck
	interpretation := r.axisWinners()
	for _, line := range interpretation.lines {
		fmt.Fprintf(&buf, "- %s\n", line) //nolint:errcheck
	}
	if interpretation.dominant == "" {
		fmt.Fprintf(&buf, "- No clear winner: no backend dominates every measured axis.\n") //nolint:errcheck
	} else {
		fmt.Fprintf(&buf, "- Dominant backend: %s\n", interpretation.dominant) //nolint:errcheck
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// WriteJSON writes the report as machine-readable JSON.
func (r TriageReport) WriteJSON(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling triage report: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("writing triage report: %w", err)
	}
	return nil
}

type triageInterpretation struct {
	lines    []string
	dominant string
}

func (r TriageReport) axisWinners() triageInterpretation {
	axes := []struct {
		label string
		value func(TriageBackend) float64
	}{
		{"Get p99 ms", func(b TriageBackend) float64 { return b.GetP99Ms.P99 }},
		{"Create p99 ms", func(b TriageBackend) float64 { return b.CreateP99Ms.P99 }},
		{"Mail poll p99 ms", func(b TriageBackend) float64 { return b.MailPollP99Ms.P99 }},
		{"RSS ceiling MB", func(b TriageBackend) float64 { return b.RSSCeilingMB }},
		{"RSS growth MB/hour", func(b TriageBackend) float64 { return b.RSSGrowthMBPerHour }},
		{"Goroutine p99", func(b TriageBackend) float64 { return float64(b.GoroutineP99) }},
		{"Kill events", func(b TriageBackend) float64 { return float64(b.KillEvents) }},
		{"Lost records", func(b TriageBackend) float64 { return float64(b.LostRecords) }},
		{"Recovery p99 ms", func(b TriageBackend) float64 { return b.RecoveryP99Ms }},
	}
	counts := make(map[string]int)
	var lines []string
	for _, axis := range axes {
		winner, tied := r.minAxisWinner(axis.value)
		if winner == "" {
			continue
		}
		if tied {
			lines = append(lines, fmt.Sprintf("%s winner: tie", axis.label))
			continue
		}
		counts[winner]++
		lines = append(lines, fmt.Sprintf("%s winner: %s", axis.label, winner))
	}
	dominant := ""
	for name, count := range counts {
		if count == len(axes) {
			dominant = name
			break
		}
	}
	return triageInterpretation{lines: lines, dominant: dominant}
}

func (r TriageReport) minAxisWinner(value func(TriageBackend) float64) (string, bool) {
	best := math.Inf(1)
	winner := ""
	tied := false
	for _, b := range r.Backends {
		v := value(b)
		if v < best {
			best = v
			winner = b.Name
			tied = false
			continue
		}
		if v == best {
			tied = true
		}
	}
	return winner, tied
}

func synthesizeBackend(name string, acc *triageAccumulator) TriageBackend {
	sort.Slice(acc.samples, func(i, j int) bool {
		return acc.samples[i].Timestamp.Before(acc.samples[j].Timestamp)
	})
	return TriageBackend{
		Name:               name,
		GetP99Ms:           latestOperationPercentile(acc.samples, "Get"),
		CreateP99Ms:        latestOperationPercentile(acc.samples, "Create"),
		MailPollP99Ms:      latestOperationPercentile(acc.samples, "MailPoll", "EphemeralFilterScan"),
		RSSCeilingMB:       rssCeilingMB(acc.samples),
		RSSGrowthMBPerHour: rssGrowthMBPerHour(acc.samples),
		GoroutineP99:       goroutineP99(acc.samples),
		KillEvents:         len(acc.killEvent),
		LostRecords:        lostRecordCount(acc.killEvent),
		RecoveryP99Ms:      recoveryP99Ms(acc.killEvent),
		ForeverTax:         foreverTaxByBackend[name],
	}
}

func latestOperationPercentile(samples []TelemetrySample, names ...string) Percentile {
	var out Percentile
	for _, sample := range samples {
		for _, name := range names {
			op, ok := sample.Operations[name]
			if !ok {
				continue
			}
			out = Percentile{
				P50:  nanosToMillis(op.P50Nanos),
				P99:  nanosToMillis(op.P99Nanos),
				P999: nanosToMillis(op.P999Nanos),
			}
			break
		}
	}
	return out
}

func rssCeilingMB(samples []TelemetrySample) float64 {
	var ceilingBytes uint64
	for _, sample := range samples {
		if sample.RSSBytes > ceilingBytes {
			ceilingBytes = sample.RSSBytes
		}
	}
	return bytesToMiB(ceilingBytes)
}

func rssGrowthMBPerHour(samples []TelemetrySample) float64 {
	if len(samples) < 2 {
		return 0
	}
	start := samples[0].Timestamp
	var sumX, sumY float64
	points := 0
	for _, sample := range samples {
		if sample.Timestamp.IsZero() {
			continue
		}
		x := sample.Timestamp.Sub(start).Hours()
		y := bytesToMiB(sample.RSSBytes)
		sumX += x
		sumY += y
		points++
	}
	if points < 2 {
		return 0
	}
	meanX := sumX / float64(points)
	meanY := sumY / float64(points)
	var numerator, denominator float64
	for _, sample := range samples {
		if sample.Timestamp.IsZero() {
			continue
		}
		x := sample.Timestamp.Sub(start).Hours()
		y := bytesToMiB(sample.RSSBytes)
		numerator += (x - meanX) * (y - meanY)
		denominator += (x - meanX) * (x - meanX)
	}
	if denominator == 0 {
		return 0
	}
	return numerator / denominator
}

func goroutineP99(samples []TelemetrySample) int {
	values := make([]int, 0, len(samples))
	for _, sample := range samples {
		if sample.Goroutines > 0 {
			values = append(values, sample.Goroutines)
		}
	}
	return intPercentile(values, 99)
}

func lostRecordCount(events []KillEvent) int {
	var lost int
	for _, event := range events {
		lost += len(event.MissingIDs)
	}
	return lost
}

func recoveryP99Ms(events []KillEvent) float64 {
	values := make([]int64, 0, len(events))
	for _, event := range events {
		nanos := event.RecoveryNanos
		if nanos == 0 {
			nanos = event.Recovery.Nanoseconds()
		}
		if nanos > 0 {
			values = append(values, nanos)
		}
	}
	return nanosToMillis(int64Percentile(values, 99))
}

func readTelemetrySamples(path string) ([]TelemetrySample, error) {
	var samples []TelemetrySample
	if err := readJSONL(path, func(data []byte) error {
		var sample TelemetrySample
		if err := json.Unmarshal(data, &sample); err != nil {
			return err
		}
		samples = append(samples, sample)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reading telemetry samples %s: %w", path, err)
	}
	return samples, nil
}

func readKillEvents(path string) ([]KillEvent, error) {
	var events []KillEvent
	if err := readJSONL(path, func(data []byte) error {
		var event KillEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		events = append(events, event)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reading kill events %s: %w", path, err)
	}
	return events, nil
}

func readJSONL(path string, decode func([]byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck
	dec := json.NewDecoder(file)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := decode(raw); err != nil {
			return err
		}
	}
}

func artifactIdentity(root, path string) (string, string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 4 {
		return "", "", false
	}
	return parts[len(parts)-4], parts[len(parts)-3], true
}

func joinSortedKeys(values map[string]struct{}) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func intPercentile(values []int, percentile float64) int {
	if len(values) == 0 {
		return 0
	}
	sort.Ints(values)
	idx := percentileIndex(len(values), percentile)
	return values[idx]
}

func int64Percentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	idx := percentileIndex(len(values), percentile)
	return values[idx]
}

func percentileIndex(n int, percentile float64) int {
	idx := int(math.Ceil(percentile/100*float64(n))) - 1
	if idx < 0 {
		return 0
	}
	if idx >= n {
		return n - 1
	}
	return idx
}

func nanosToMillis(nanos int64) float64 {
	return float64(nanos) / float64(time.Millisecond)
}

func bytesToMiB(bytes uint64) float64 {
	return float64(bytes) / (1024 * 1024)
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", path, err)
	}
	return nil
}

func pathJoin(elem ...string) string {
	return filepath.Join(elem...)
}
