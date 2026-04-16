package api

import (
	"fmt"
	"net/http"
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

