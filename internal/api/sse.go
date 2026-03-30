package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const sseKeepalive = 15 * time.Second

// writeSSE writes a single SSE event to w and flushes.
func writeSSE(w http.ResponseWriter, eventType string, id uint64, data []byte) {
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", eventType, id, data) //nolint:errcheck
	// Use ResponseController to flush through wrapped writers (e.g., logging middleware).
	if err := http.NewResponseController(w).Flush(); err != nil {
		// Flushing not supported; best-effort.
		_ = err
	}
}

func writeSSEWithStringID(w http.ResponseWriter, eventType, id string, data []byte) {
	fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", eventType, id, data) //nolint:errcheck
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}
}

// writeSSEComment writes a keepalive comment line and flushes.
func writeSSEComment(w http.ResponseWriter) {
	fmt.Fprintf(w, ": keepalive\n\n") //nolint:errcheck
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}
}

// parseAfterSeq reads the reconnect position from Last-Event-ID or ?after_seq.
func parseAfterSeq(r *http.Request) uint64 {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	if v := r.URL.Query().Get("after_seq"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}
