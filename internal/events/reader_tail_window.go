package events

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/platform/livefile"
)

// TailWindow is what ReadTailWindow saw of a log: the matching events inside the
// newest part of the active file, and enough about the rest to tell a caller
// what it has NOT seen.
type TailWindow struct {
	// Events holds the matching events in the window, in chronological order.
	Events []Event
	// Oldest is the timestamp of the oldest event the walk visited, whatever its
	// type, or the zero time when it visited none. Events older than the window
	// have a timestamp at or before it, because the log is append-only and each
	// event is stamped as it is appended; callers rely on that ordering.
	Oldest time.Time
	// Complete is true only when the window is the whole history: the walk
	// covered the active file from byte 0 and no archive or rotating segment
	// holds older events.
	Complete bool
}

// ReadTailWindow reads the active event log at path newest-first, decoding at
// most filter.MaxScanBytes bytes (zero or negative = the whole file), and
// returns the events matching filter along with the window's horizon.
//
// Unlike ReadFilteredTail it takes no event-count limit, never opens an
// archive, and honors ctx. A limit on matches would let a busy event type push
// a rare one out of the window; a byte bound keeps the cost fixed whatever the
// mix. Archives and rotating segments are only listed, never read, so Complete
// can say whether older history exists.
func ReadTailWindow(ctx context.Context, path string, filter Filter) (TailWindow, error) {
	older, err := listBackfillSources(filepath.Dir(path), 0)
	if err != nil {
		return TailWindow{}, fmt.Errorf("listing event archives for tail window: %w", err)
	}
	hasOlder := len(older) > 0

	f, err := livefile.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return TailWindow{Complete: !hasOlder}, nil
		}
		return TailWindow{}, fmt.Errorf("reading events tail window: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	info, err := f.Stat()
	if err != nil {
		return TailWindow{}, fmt.Errorf("stat events tail window: %w", err)
	}

	var reversed []Event
	var oldest time.Time
	if _, err := walkBackward(ctx, f, info.Size(), filter.MaxScanBytes, func(e Event) bool {
		oldest = e.Ts
		if matchesFilter(e, filter) {
			reversed = append(reversed, e)
		}
		return false
	}); err != nil {
		return TailWindow{}, fmt.Errorf("reading events tail window: %w", err)
	}

	coveredAll := filter.MaxScanBytes <= 0 || filter.MaxScanBytes >= info.Size()
	return TailWindow{
		Events:   ascending(reversed, 0),
		Oldest:   oldest,
		Complete: coveredAll && !hasOlder,
	}, nil
}
