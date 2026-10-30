//go:build windows

package genshell

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type overlappingWriteRecorder struct {
	active    atomic.Int32
	maxActive atomic.Int32
	writes    atomic.Int64
}

func (w *overlappingWriteRecorder) Write(p []byte) (int, error) {
	active := w.active.Add(1)
	for {
		maxActive := w.maxActive.Load()
		if active <= maxActive || w.maxActive.CompareAndSwap(maxActive, active) {
			break
		}
	}
	time.Sleep(20 * time.Microsecond)
	w.writes.Add(1)
	w.active.Add(-1)
	return len(p), nil
}

func TestRunSerializesPipelineStageStderrWrites(t *testing.T) {
	prog := Program{Script: `bd one | bd two`}
	stderr := &overlappingWriteRecorder{}
	run := func(ctx context.Context, p Process) error {
		for i := 0; i < 2000; i++ {
			fmt.Fprintf(p.Stderr, "%s warning %d\n", p.Args[1], i)
		}
		if p.Stdin != nil {
			_, _ = io.Copy(io.Discard, p.Stdin)
		}
		return nil
	}

	if err := prog.Run(context.Background(), Options{Stdout: io.Discard, Stderr: stderr, Exec: run}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("stderr writes=%d, max concurrent Write calls=%d", stderr.writes.Load(), stderr.maxActive.Load())
	if stderr.maxActive.Load() > 1 {
		t.Fatalf("pipeline stages wrote Options.Stderr concurrently")
	}
}
