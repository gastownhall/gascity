package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestForceRotateEmptyActiveLogIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Rotated {
		t.Errorf("Rotated = true, want false on empty active log")
	}
	if !strings.Contains(res.Reason, "empty") {
		t.Errorf("Reason = %q, want mention of empty", res.Reason)
	}
	if res.ArchivePath != "" {
		t.Errorf("ArchivePath = %q on no-op, want empty", res.ArchivePath)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".archive-") || strings.Contains(e.Name(), ".rotating-") {
			t.Errorf("unexpected sibling file %q after empty-log rotation", e.Name())
		}
	}
}

func TestForceRotateMovesEventsToArchiveAndWritesAnchor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	for i := 0; i < 5; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human", Subject: fmt.Sprintf("gc-%d", i)})
	}

	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if !res.Rotated {
		t.Fatalf("expected Rotated=true, got %+v", res)
	}
	if res.FirstSeq != 1 || res.LastSeq != 5 {
		t.Errorf("seq window = [%d,%d], want [1,5]", res.FirstSeq, res.LastSeq)
	}
	if res.AnchorSeq != 6 {
		t.Errorf("AnchorSeq = %d, want 6 (one beyond last archived)", res.AnchorSeq)
	}
	if res.AnchorTimestamp.IsZero() {
		t.Error("AnchorTimestamp is zero")
	}
	if !res.CompressionPending {
		t.Error("CompressionPending = false, want true on success")
	}
	if res.Done == nil {
		t.Fatal("Done channel is nil")
	}
	select {
	case <-res.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed within 2s")
	}

	if _, err := os.Stat(res.ArchivePath); err != nil {
		t.Errorf("ArchivePath %q not on disk: %v", res.ArchivePath, err)
	}
	if !strings.HasSuffix(res.ArchivePath, ".gz") {
		t.Errorf("ArchivePath %q does not end in .gz", res.ArchivePath)
	}

	body, err := readGzipFile(res.ArchivePath)
	if err != nil {
		t.Fatalf("readGzipFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 5 {
		t.Errorf("archive has %d lines, want 5", len(lines))
	}

	rec.Record(Event{Type: BeadClosed, Actor: "human", Subject: "post-rotate"})

	// Read the active file directly (bypassing archive walk) and verify
	// it starts with the anchor followed by the post-rotate event.
	active, err := readActiveOnly(path)
	if err != nil {
		t.Fatalf("readActiveOnly: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active log has %d events, want 2 (anchor + post-rotate)", len(active))
	}
	if active[0].Type != EventsRotated {
		t.Errorf("active[0].Type = %q, want %q", active[0].Type, EventsRotated)
	}
	if active[0].Seq != res.AnchorSeq {
		t.Errorf("active[0].Seq = %d, want %d", active[0].Seq, res.AnchorSeq)
	}
	var payload RotatedPayload
	if err := json.Unmarshal(active[0].Payload, &payload); err != nil {
		t.Fatalf("anchor payload unmarshal: %v", err)
	}
	if payload.PriorArchive != filepath.Base(res.ArchivePath) {
		t.Errorf("anchor.PriorArchive = %q, want %q", payload.PriorArchive, filepath.Base(res.ArchivePath))
	}
	if payload.PriorFirstSeq != 1 || payload.PriorLastSeq != 5 {
		t.Errorf("anchor seq window = [%d,%d], want [1,5]", payload.PriorFirstSeq, payload.PriorLastSeq)
	}
	if active[1].Seq != active[0].Seq+1 {
		t.Errorf("post-rotate Seq = %d, want %d", active[1].Seq, active[0].Seq+1)
	}
}

// A second long-lived recorder can hold the pre-rotation inode open. Its first
// write after another recorder rotates must follow the path to the fresh active
// file; otherwise it appends to an unlinked or in-flight rotating file whose
// filename window was already fixed, making the event invisible to cursored
// readers.
func TestForceRotateMovesPeerRecorderToFreshActiveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var rotatingStderr, peerStderr bytes.Buffer
	rotating, err := NewFileRecorder(path, &rotatingStderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rotating.Close() //nolint:errcheck // test cleanup

	peer, err := NewFileRecorder(path, &peerStderr)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close() //nolint:errcheck // test cleanup

	rotating.Record(Event{Type: BeadCreated, Actor: "rotator", Subject: "before"})
	res, err := rotating.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.Done == nil {
		t.Fatal("ForceRotate returned a nil completion channel")
	}

	// Append before compression completes to cover the production interleaving:
	// rotation has fixed the old inode's filename window, while a peer that
	// opened before the rename still holds that inode's descriptor.
	peer.Record(Event{Type: BeadUpdated, Actor: "peer", Subject: "after"})
	select {
	case <-res.Done:
	case <-time.After(10 * time.Second):
		t.Fatal("rotation compression did not complete")
	}
	archiveInfo, err := parseArchiveBasename(filepath.Base(res.ArchivePath))
	if err != nil {
		t.Fatalf("parse archive name: %v", err)
	}
	physicalFirst, physicalLast, err := readGzipSeqWindow(res.ArchivePath)
	if err != nil {
		t.Fatalf("read physical archive window: %v", err)
	}
	if physicalFirst != archiveInfo.FirstSeq || physicalLast != archiveInfo.LastSeq {
		t.Fatalf("physical archive window = [%d,%d], filename window = [%d,%d]",
			physicalFirst, physicalLast, archiveInfo.FirstSeq, archiveInfo.LastSeq)
	}

	active, err := readActiveOnly(path)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active event count = %d, want anchor + peer event; events=%+v; peer stderr=%q",
			len(active), active, peerStderr.String())
	}
	if active[0].Type != EventsRotated || active[0].Seq != 2 {
		t.Fatalf("active anchor = %+v, want events.rotated seq 2", active[0])
	}
	if active[1].Subject != "after" || active[1].Seq != 3 {
		t.Fatalf("peer event = %+v, want subject after seq 3", active[1])
	}

	all, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ReadAll count = %d, want 3; events=%+v", len(all), all)
	}
	for i, event := range all {
		wantSeq := uint64(i + 1)
		if event.Seq != wantSeq {
			t.Fatalf("event[%d].Seq = %d, want %d; events=%+v", i, event.Seq, wantSeq, all)
		}
	}
	after, err := ReadFiltered(path, Filter{AfterSeq: 1})
	if err != nil {
		t.Fatalf("ReadFiltered(AfterSeq=1): %v", err)
	}
	if len(after) != 2 || after[0].Seq != 2 || after[1].Seq != 3 {
		t.Fatalf("ReadFiltered(AfterSeq=1) seqs = %v, want [2 3]", seqsOf(after))
	}
	if rotatingStderr.Len() != 0 || peerStderr.Len() != 0 {
		t.Fatalf("unexpected recorder stderr: rotating=%q peer=%q", rotatingStderr.String(), peerStderr.String())
	}
}

func TestForceRotateWaitsForPeerRecorderLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	recorder, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close() //nolint:errcheck // test cleanup
	recorder.Record(Event{Type: BeadCreated, Subject: "before"})

	peerLock := mustOpenSiblingLock(t, recorderLockPath(path))
	time.AfterFunc(50*time.Millisecond, func() {
		_ = syscall.Flock(int(peerLock.Fd()), syscall.LOCK_UN)
		_ = peerLock.Close()
	})

	started := time.Now()
	result, err := recorder.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("ForceRotate acquired peer-held recorder lock after %v, want at least 40ms", elapsed)
	}
	if !result.Rotated {
		t.Fatalf("ForceRotate result = %+v, want a rotation", result)
	}
	if result.Done != nil {
		<-result.Done
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected recorder stderr: %q", stderr.String())
	}
}

func TestForceRotateConcurrentWithRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	const writers = 4
	const perWriter = 25
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				rec.Record(Event{Type: BeadCreated, Actor: "human"})
			}
		}()
	}

	rotated := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		res, err := rec.ForceRotate()
		if err != nil {
			t.Errorf("ForceRotate: %v", err)
			break
		}
		if res.Rotated {
			rotated++
			if res.Done != nil {
				<-res.Done
			}
		}
	}
	wg.Wait()

	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate(final): %v", err)
	}
	if res.Rotated && res.Done != nil {
		<-res.Done
	}
	if rotated == 0 {
		t.Fatal("no rotations occurred during concurrency test")
	}

	all, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	allSeen := make(map[uint64]bool, len(all))
	var prev uint64
	for _, ev := range all {
		if allSeen[ev.Seq] {
			t.Errorf("duplicate seq %d", ev.Seq)
		}
		if ev.Seq <= prev {
			t.Errorf("seq not monotonic: %d <= %d", ev.Seq, prev)
		}
		prev = ev.Seq
		allSeen[ev.Seq] = true
	}
	if len(allSeen) < writers*perWriter {
		t.Errorf("seen %d unique seqs across archives+active, expected at least %d (writes only — ignoring anchors)",
			len(allSeen), writers*perWriter)
	}
}

func TestForceRotateAfterCloseReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Type: BeadCreated, Actor: "human"})
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := rec.ForceRotate(); err == nil {
		t.Error("expected error from ForceRotate after Close")
	}
}

func TestRotateLockedReadsArchiveSeqWindowFromTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	const n = 1000
	for i := 0; i < n; i++ {
		rec.Record(Event{Type: BeadCreated, Actor: "human"})
	}
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if res.FirstSeq != 1 || res.LastSeq != uint64(n) {
		t.Errorf("seq window = [%d,%d], want [1,%d]", res.FirstSeq, res.LastSeq, n)
	}
	if res.Done != nil {
		<-res.Done
	}

	// Use the watcher Close path implicitly via context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = ctx
}
