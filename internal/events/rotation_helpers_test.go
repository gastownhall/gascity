package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// renameWithoutAnchor does what a rotation does before it writes its anchor:
// it renames the log at path, which holds seqs first..last, to its rotating-*
// name. With fresh it then creates the empty log the rotation reopens, as when
// the anchor write fails; without it there is no log at path, as when the
// reopen fails.
func renameWithoutAnchor(t *testing.T, path string, first, last uint64, fresh bool) {
	t.Helper()
	rotating := filepath.Join(filepath.Dir(path), formatRotatingBasename(time.Now().UTC(), first, last))
	if err := os.Rename(path, rotating); err != nil {
		t.Fatal(err)
	}
	if fresh {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// lockedBuffer is a goroutine-safe stderr sink: recorders write to stderr from
// both the caller's goroutine and their background rotation goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// seedEventLog writes n events with seqs 1..n straight to path, standing in for
// an active log some earlier process filled.
func seedEventLog(t *testing.T, path string, n int) {
	t.Helper()
	var data bytes.Buffer
	for i := 1; i <= n; i++ {
		line, err := json.Marshal(Event{Seq: uint64(i), Type: BeadCreated, Ts: time.Now(), Actor: "seed", Subject: fmt.Sprintf("seed-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(path, data.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
