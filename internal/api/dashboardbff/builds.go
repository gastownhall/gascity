package dashboardbff

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// deployRecord is one parsed deploy-log line, matching shared/src/activity.ts
// DeployRecord. detail carries "old-sha -> new-sha" on ok, "stage: X" context
// on failure, or the raw line otherwise.
type deployRecord struct {
	At     string `json:"at"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// deployList is the GET /api/builds response, matching shared/src/activity.ts
// DeployList. source is null when the log file is absent; items is always an
// explicit array.
type deployList struct {
	Items        []deployRecord `json:"items"`
	Source       *string        `json:"source"`
	FailedMarker bool           `json:"failed_marker"`
}

// Recent activity only — 200 records covers roughly a month at typical
// dev-deploy cadence without turning this into a log browser (MAX_RECORDS in
// builds.ts).
const maxDeployRecords = 200

// lineRE matches "[ISO-TS] <rest>" deploy-log lines (LINE_RE in builds.ts).
var lineRE = regexp.MustCompile(`^\[([^\]]+)\]\s+(.*)$`)

// deployLogPath returns the deploy-log path: $HOME/.dev-deploy-log, falling
// back to a bare relative name when HOME is unset (mirrors DEFAULT_LOG_PATH).
func deployLogPath() string {
	if home := os.Getenv("HOME"); home != "" {
		return home + "/.dev-deploy-log"
	}
	return ".dev-deploy-log"
}

// deployMarkerPath returns the failure-marker path: $HOME/.dev-deploy-FAILED,
// falling back to a bare relative name when HOME is unset.
func deployMarkerPath() string {
	if home := os.Getenv("HOME"); home != "" {
		return home + "/.dev-deploy-FAILED"
	}
	return ".dev-deploy-FAILED"
}

// registerBuilds wires GET /api/builds onto the plane mux.
func (p *Plane) registerBuilds() {
	p.mux.HandleFunc("GET /api/builds", func(w http.ResponseWriter, _ *http.Request) {
		logPath := deployLogPath()
		markerPath := deployMarkerPath()

		items := []deployRecord{}
		var source *string
		if data, err := readDeployLogTail(logPath); err == nil {
			src := logPath
			source = &src
			items = parseDeployLog(string(data))
		}

		failedMarker := false
		if _, err := os.Stat(markerPath); err == nil {
			failedMarker = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			// A non-missing error (e.g. permission) is not a present marker.
			failedMarker = false
		}

		writeJSON(w, http.StatusOK, deployList{
			Items:        items,
			Source:       source,
			FailedMarker: failedMarker,
		})
	})
}

// maxDeployLogTailBytes bounds how much of the deploy log GET /api/builds reads
// per request. Only the newest maxDeployRecords are ever returned, so reading
// the entire append-only (never-rotated) log on every dashboard poll would be an
// unbounded per-request allocation; a fixed tail window caps it while still
// covering far more than maxDeployRecords lines.
const maxDeployLogTailBytes = 256 << 10

// readDeployLogTail reads at most the last maxDeployLogTailBytes of the deploy
// log. Starting the window mid-line is harmless: parseDeployLog walks newest
// line first and skips any line that is not a well-formed "[ts] rest" entry, so
// a partial leading line is dropped and the newest records (the ones actually
// returned) are intact at the end of the file.
func readDeployLogTail(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if size := info.Size(); size > maxDeployLogTailBytes {
		if _, err := f.Seek(size-maxDeployLogTailBytes, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}

// parseDeployLog parses deploy-log text newest-first, capping at
// maxDeployRecords. Lines that do not match the "[ts] rest" shape are skipped,
// matching builds.ts (reverse, trim, LINE_RE, classify).
func parseDeployLog(text string) []deployRecord {
	items := []deployRecord{}
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if len(items) >= maxDeployRecords {
			break
		}
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		m := lineRE.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		ts, rest := m[1], m[2]
		if ts == "" || rest == "" {
			continue
		}
		items = append(items, deployRecord{
			At:     ts,
			Status: classifyDeploy(rest),
			Detail: rest,
		})
	}
	return items
}

// classifyDeploy maps a deploy-log line body to a DeployStatus, matching
// builds.ts::classify.
func classifyDeploy(rest string) string {
	switch {
	case strings.HasPrefix(rest, "deploy OK"):
		return "ok"
	case strings.HasPrefix(rest, "DEPLOY FAILED"):
		return "failed"
	case strings.HasPrefix(rest, "deploying "):
		return "in-progress"
	default:
		return "unknown"
	}
}
