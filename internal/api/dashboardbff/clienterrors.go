package dashboardbff

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// clientErrorReport is the POST /api/client-errors body, matching
// shared/src/operator.ts ClientErrorReport. It is a telemetry sink: fields are
// logged for diagnostics, not persisted as domain state.
type clientErrorReport struct {
	Component string `json:"component"`
	Operation string `json:"operation"`
	Message   string `json:"message"`
}

// maxClientErrorBody caps the request body so a runaway client cannot flood the
// host with a huge telemetry payload (~64KB).
const maxClientErrorBody = 64 << 10

// maxClientErrorField bounds each logged field length, mirroring the Node
// route's MAX_FIELD_LENGTH so a single report cannot dominate the log line.
const maxClientErrorField = 240

// maxClientErrorReports caps how many entries one request can emit, so a single
// 64KB array body cannot amplify into thousands of log lines.
const maxClientErrorReports = 32

// registerClientLog wires POST /api/client-errors onto the plane mux. It is a
// telemetry sink: it accepts a single report or an array of reports, logs one
// structured line per entry, and answers 204 No Content. As a mutation it
// passes through the plane's X-GC-Request guard, which is expected.
func (p *Plane) registerClientLog() {
	p.mux.HandleFunc("POST /api/client-errors", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxClientErrorBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not read body")
			return
		}
		reports, err := decodeClientErrorReports(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid client error report")
			return
		}
		if len(reports) > maxClientErrorReports {
			reports = reports[:maxClientErrorReports]
		}
		for _, rep := range reports {
			log.Printf("client-error: %s %s: %s",
				clientErrorField(rep.Component),
				clientErrorField(rep.Operation),
				clientErrorField(rep.Message),
			)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusNoContent)
	})
}

// decodeClientErrorReports decodes the request body, tolerating either a single
// object or an array of objects.
func decodeClientErrorReports(body []byte) ([]clientErrorReport, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var reports []clientErrorReport
		if err := json.Unmarshal(body, &reports); err != nil {
			return nil, err
		}
		return reports, nil
	}
	var one clientErrorReport
	if err := json.Unmarshal(body, &one); err != nil {
		return nil, err
	}
	return []clientErrorReport{one}, nil
}

// clientErrorField normalizes a single field for logging: terminal/control
// sequences are stripped (no log injection from untrusted browser input),
// whitespace is collapsed, and the result is capped at maxClientErrorField.
func clientErrorField(v string) string {
	v = strings.Join(strings.Fields(sanitizeTerminalOutput(v)), " ")
	if len(v) > maxClientErrorField {
		return v[:maxClientErrorField]
	}
	return v
}
