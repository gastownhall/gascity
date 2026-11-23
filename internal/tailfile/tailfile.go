// Package tailfile writes the end of a file and, optionally, what is appended
// to it afterwards, as `tail -n N [-f] FILE` does, so a caller needs no tail
// program on the host.
package tailfile

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/gastownhall/gascity/internal/platform/livefile"
)

// pollInterval bounds how long a follow waits for an append it was not
// notified of. It is the safety net under file-change notification, which
// can miss or delay events (Windows may not report an append until the
// writer flushes metadata), and the only wake-up where notification is
// unavailable. One second is tail's own default --sleep-interval.
const pollInterval = time.Second

// scanChunk is how much of the file one backward read examines while looking
// for the start of the last lines.
var scanChunk = 64 * 1024

// Tail writes the last lines lines of the file at path to out, as tail -n
// does: a final line without a trailing newline counts as a line, and a
// negative count means its magnitude, as tail's -n -N does. What it writes is
// byte for byte what GNU tail writes; a failure is reported as a Go error, not
// in tail's words.
//
// With follow set it then writes everything appended to the file until ctx
// ends, and returns nil. It follows the file it opened, as tail -f does: a
// rename or removal of path does not switch it to a new file, and on Windows,
// as on Unix, the open file can still be renamed and removed. A file truncated
// below what has been read is read again from its start, after onTruncate
// (when non-nil) is called, which is where tail -f prints "file truncated".
// Where the platform can tell (Unix), a follow whose out is a pipe whose
// reader has closed ends with an error wrapping syscall.EPIPE, as tail -f
// does, within a poll interval even when the file stays quiet.
func Tail(ctx context.Context, path string, lines int, follow bool, out io.Writer, onTruncate func()) error {
	f, err := livefile.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle

	start, err := lastLinesOffset(f, lines)
	if err != nil {
		return err
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	n, err := io.Copy(out, f)
	if err != nil || !follow {
		return err
	}
	return followAppends(ctx, f, path, start+n, out, onTruncate)
}

// lastLinesOffset returns the offset at which the last lines lines of f begin,
// scanning backwards from the end so a large file is not read in full.
func lastLinesOffset(f *os.File, lines int) (int64, error) {
	if lines < 0 {
		lines = -lines
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if lines == 0 || size == 0 {
		return size, nil
	}
	buf := make([]byte, scanChunk)
	newlines := 0
	for pos := size; pos > 0; {
		n := int64(len(buf))
		if pos < n {
			n = pos
		}
		pos -= n
		if _, err := f.ReadAt(buf[:n], pos); err != nil {
			if errors.Is(err, io.EOF) {
				// The file shrank under the scan: what is left is shorter
				// than what was asked for, so all of it is the tail.
				return 0, nil
			}
			return 0, err
		}
		for i := n - 1; i >= 0; i-- {
			// A newline that ends the file terminates the last line rather
			// than separating it from another.
			if buf[i] != '\n' || pos+i == size-1 {
				continue
			}
			newlines++
			if newlines == lines {
				return pos + i + 1, nil
			}
		}
	}
	return 0, nil
}

// followAppends copies what is appended to f from offset onwards to out until
// ctx ends or out's reader is gone (outputGone). It wakes on change
// notifications for path and, regardless of them, every pollInterval.
func followAppends(ctx context.Context, f *os.File, path string, offset int64, out io.Writer, onTruncate func()) error {
	var events <-chan fsnotify.Event
	var watchErrs <-chan error
	if watcher, err := fsnotify.NewWatcher(); err == nil {
		defer watcher.Close() //nolint:errcheck // best-effort wake-up source
		if watcher.Add(path) == nil {
			events, watchErrs = watcher.Events, watcher.Errors
		}
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if err := outputGone(out); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case _, ok := <-events:
			if !ok {
				events = nil
			}
		case _, ok := <-watchErrs:
			// A notification error leaves the poll to find the append.
			if !ok {
				watchErrs = nil
			}
		}
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() < offset {
			if onTruncate != nil {
				onTruncate()
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			offset = 0
		}
		n, err := io.Copy(out, f)
		offset += n
		if err != nil {
			return err
		}
	}
}
