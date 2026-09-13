package api

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

const (
	defaultWait = 30 * time.Second
	maxWait     = 5 * time.Minute
)

// BlockingParams holds parsed blocking query parameters.
type BlockingParams struct {
	Index    uint64
	Wait     time.Duration
	HasIndex bool // true if the "index" query param was provided
}

// parseBlockingParams extracts index and wait from the request.
func parseBlockingParams(r *http.Request) BlockingParams {
	bp := BlockingParams{Wait: defaultWait}
	if v := r.URL.Query().Get("index"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			bp.Index = n
			bp.HasIndex = true
		}
	}
	if v := r.URL.Query().Get("wait"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			bp.Wait = d
		}
	}
	if bp.Wait > maxWait {
		bp.Wait = maxWait
	}
	return bp
}

// isBlocking reports whether the request is a blocking query.
// Index 0 is valid — it means "wait for the first event."
func (bp BlockingParams) isBlocking() bool {
	return bp.HasIndex
}

// waitForChange blocks until the event index exceeds bp.Index or timeout.
// Returns the current index after waiting.
func waitForChange(ctx context.Context, ep events.Provider, bp BlockingParams) uint64 {
	if ep == nil {
		return 0
	}

	seq, _ := ep.LatestSeq()
	if seq > bp.Index {
		return seq
	}

	// Add jitter: wait/16 (guard against zero for tiny wait values).
	var jitter time.Duration
	if jmax := int64(bp.Wait / 16); jmax > 0 {
		jitter = time.Duration(rand.Int64N(jmax))
	}
	deadline := bp.Wait + jitter

	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	w, err := ep.Watch(ctx, bp.Index)
	if err != nil {
		return seq
	}
	defer w.Close() //nolint:errcheck // best-effort

	// Wait for one event or timeout.
	if _, err := w.Next(); err == nil {
		seq, _ = ep.LatestSeq()
	}
	return seq
}
