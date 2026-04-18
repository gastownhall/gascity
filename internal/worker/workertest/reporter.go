package workertest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const reportDirEnv = "GC_WORKER_REPORT_DIR"

// SuiteReporter collects conformance results for one suite and flushes a JSON
// report artifact at test cleanup when GC_WORKER_REPORT_DIR is configured.
type SuiteReporter struct {
	suite     string
	startedAt time.Time
	reportDir string
	metadata  map[string]string

	mu      sync.Mutex
	results []Result
}

// NewSuiteReporter installs a cleanup hook that flushes a suite report.
func NewSuiteReporter(t *testing.T, suite string, metadata map[string]string) *SuiteReporter {
	t.Helper()

	reporter := &SuiteReporter{
		suite:     suite,
		startedAt: time.Now().UTC(),
		reportDir: strings.TrimSpace(os.Getenv(reportDirEnv)),
		metadata:  reportMetadata(suite, metadata),
	}
	t.Cleanup(func() {
		reporter.flush(t)
	})
	return reporter
}

// Require records a result and fails the test when it is non-passing.
func (r *SuiteReporter) Require(t *testing.T, result Result) {
	t.Helper()

	r.Record(result)
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
}

// Record appends a conformance result without failing the caller.
func (r *SuiteReporter) Record(result Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
}

func (r *SuiteReporter) flush(t *testing.T) {
	t.Helper()

	if r.reportDir == "" {
		return
	}

	r.mu.Lock()
	results := append([]Result(nil), r.results...)
	r.mu.Unlock()

	if err := os.MkdirAll(r.reportDir, 0o755); err != nil {
		t.Fatalf("create worker report dir: %v", err)
	}

	suiteFailure := suiteFailureDetail(t.Failed(), results)
	report := NewRunReport(ReportInput{
		RunID:         reportRunID(r.suite),
		Suite:         r.suite,
		StartedAt:     r.startedAt,
		CompletedAt:   time.Now().UTC(),
		Metadata:      r.metadata,
		SuiteFailed:   suiteFailure != "",
		FailureDetail: suiteFailure,
		Results:       results,
	})
	data, err := MarshalReport(report)
	if err != nil {
		t.Fatalf("marshal worker report: %v", err)
	}

	path := filepath.Join(r.reportDir, reportFileName(r.suite))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write worker report %s: %v", path, err)
	}
	t.Logf("worker conformance report: %s", path)
}

func reportMetadata(suite string, metadata map[string]string) map[string]string {
	out := copyMetadata(metadata)
	if out == nil {
		out = make(map[string]string)
	}
	out["suite"] = suite
	if profile := strings.TrimSpace(os.Getenv("PROFILE")); profile != "" {
		out["profile_filter"] = profile
	}
	if job := strings.TrimSpace(os.Getenv("GITHUB_JOB")); job != "" {
		out["github_job"] = job
	}
	if runID := strings.TrimSpace(os.Getenv("GITHUB_RUN_ID")); runID != "" {
		out["github_run_id"] = runID
	}
	return out
}

func reportRunID(suite string) string {
	filter := strings.TrimSpace(os.Getenv("PROFILE"))
	if filter == "" {
		filter = "all-profiles"
	}
	return fmt.Sprintf("%s-%s", sanitizeReportSegment(suite), sanitizeReportSegment(filter))
}

func reportFileName(suite string) string {
	return reportRunID(suite) + ".json"
}

func sanitizeReportSegment(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastDash = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

func suiteFailureDetail(suiteFailed bool, results []Result) string {
	if !suiteFailed || hasFailingResult(results) {
		return ""
	}
	return "suite failed outside recorded conformance results"
}

func hasFailingResult(results []Result) bool {
	for _, result := range results {
		if result.Status != ResultPass {
			return true
		}
	}
	return false
}
