package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storehealth"
)

// statusStoreHealthTimeout bounds the store-health row count so a live city
// with a large closed-history table cannot stall `gc status` for minutes. The
// count drives only the on-disk size ratio and is best-effort: a timeout
// means the count is UNMEASURED, not zero. The API server's
// countBeadStoreRows (internal/api/store_health.go, statusStoreReadTimeout)
// returns an error on the same failure modes rather than fabricating a
// count; liveRowCount mirrors that by returning measured=false instead of a
// bare 0. It matches the server's 1s bound.
const statusStoreHealthTimeout = time.Second

// storeHealthFromInputs assembles a CLI-facing *StoreHealth from the raw
// measurements. rowsMeasured distinguishes a real liveRows count from a
// count that failed or timed out — see storehealth.Compute.
func storeHealthFromInputs(cityPath string, sizeBytes int64, liveRows int, rowsMeasured bool) *StoreHealth {
	h := storehealth.Compute(cityPath, sizeBytes, liveRows, rowsMeasured)
	return &StoreHealth{
		Path:            h.Path,
		SizeBytes:       h.SizeBytes,
		LiveRows:        h.LiveRows,
		LiveRowsUnknown: !h.RowsMeasured,
		RatioMB:         h.RatioMB,
		Warning:         h.Warning,
		ThresholdMB:     h.ThresholdMB,
	}
}

// collectStoreHealth measures the Dolt store at cityPath and returns a
// populated *StoreHealth. liveRowCount provides the live row count and
// whether it was actually measured; callers without a store pass nil and
// the count is unmeasured.
func collectStoreHealth(cityPath string, store beads.Store) *StoreHealth {
	size := storehealth.WalkSize(storehealth.StorePath(cityPath))
	rows, measured := liveRowCount(store)
	return storeHealthFromInputs(cityPath, size, rows, measured)
}

// liveRowCount returns the number of beads known to store and whether that
// count is real. measured is false — and rows is meaningless — when store is
// nil, the count fails, or it does not finish within statusStoreHealthTimeout.
// A caller MUST NOT treat rows as a real zero when measured is false: that
// conflation renders a timed-out count byte-identically to a healthy,
// genuinely empty store. Counts all statuses (including closed) because the
// ratio is about
// on-disk row footprint, not actionable work — but that closed-inclusive scan
// is never cache-answerable and hydrates the whole history from the backend,
// so it is bounded to keep `gc status` responsive. A Counter-capable store
// (Dolt / CachingStore) answers from the catalog without hydrating rows;
// otherwise a bounded full scan is the fallback.
func liveRowCount(store beads.Store) (rows int, measured bool) {
	if store == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusStoreHealthTimeout)
	defer cancel()
	query := beads.ListQuery{AllowScan: true, IncludeClosed: true}
	if counter, ok := store.(beads.Counter); ok {
		if n, err := counter.Count(ctx, query); err == nil {
			return n, true
		}
	}
	list, err := listBeadsWithTimeout(ctx, store, query)
	if err != nil {
		return 0, false
	}
	return len(list), true
}

// listBeadsWithTimeout runs store.List on a goroutine and returns its result,
// or ctx.Err() if the deadline fires first. beads.Store.List takes no context,
// so a stalled scan cannot be canceled — the goroutine is left to finish on
// its own (harmless in the short-lived `gc status` process). Mirrors the API
// server's statusListStoreWithTimeout.
func listBeadsWithTimeout(ctx context.Context, store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	type listResult struct {
		list []beads.Bead
		err  error
	}
	done := make(chan listResult, 1)
	go func() {
		list, err := store.List(query)
		done <- listResult{list: list, err: err}
	}()
	select {
	case r := <-done:
		return r.list, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// renderStoreHealthBlock prints the human-readable "Store health:"
// block that follows the summary of gc status. No-op when h is nil.
func renderStoreHealthBlock(w io.Writer, h *StoreHealth) {
	if h == nil {
		return
	}
	fmt.Fprintln(w)                                                        //nolint:errcheck // best-effort stdout
	fmt.Fprintln(w, "Store health:")                                       //nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "  Path:        %s\n", h.Path)                          //nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "  Size:        %s\n", storeHealthSIBytes(h.SizeBytes)) //nolint:errcheck // best-effort stdout
	if h.LiveRowsUnknown {
		// The ratio line is deliberately omitted rather than printed as
		// 0.0 MB/row: with no row count there is no ratio, and rendering
		// one would restate the defect this branch exists to fix. The
		// cause is not named because the caller does not know it \u2014 a nil
		// store, a scan error and a timeout are all unmeasured.
		fmt.Fprintln(w, "  Live rows:   unknown (count unavailable)") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(w, "  Live rows:   %d\n", h.LiveRows) //nolint:errcheck // best-effort stdout
		suffix := ""
		if h.Warning {
			suffix = "  \u26a0 size-to-row ratio exceeds threshold"
		}
		fmt.Fprintf(w, "  Ratio:       %.1f MB/row  (threshold %.1f MB/row)%s\n", h.RatioMB, h.ThresholdMB, suffix) //nolint:errcheck // best-effort stdout
	}
}

// storeHealthSIBytes formats n with SI prefixes (1 KB = 1000 B, 1 MB =
// 10^6 B, 1 GB = 10^9 B) to match the MB-per-row ratio (which is SI).
func storeHealthSIBytes(n int64) string {
	const (
		kb = int64(1_000)
		mb = int64(1_000_000)
		gb = int64(1_000_000_000)
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
