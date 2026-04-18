//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

const (
	beadEventScanInterval       = 250 * time.Millisecond
	beadFallbackRefreshInterval = 200 * time.Millisecond
)

func waitForBeadCondition(t *testing.T, cityDir, beadID string, timeout time.Duration, predicate func(graphBead) bool) (graphBead, error) {
	t.Helper()

	eventLog := filepath.Join(cityDir, ".gc", "events.jsonl")
	offset := eventLogOffset(eventLog)

	if bead, err := tryShowBead(cityDir, beadID); err == nil {
		if predicate(bead) {
			return bead, nil
		}
	} else {
		t.Logf("bd show error before waiting for %s (retrying via events): %v", beadID, err)
	}

	eventTicker := time.NewTicker(beadEventScanInterval)
	defer eventTicker.Stop()
	refreshTicker := time.NewTicker(beadFallbackRefreshInterval)
	defer refreshTicker.Stop()
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	for {
		shouldRefresh := false
		select {
		case <-timeoutTimer.C:
			return graphBead{}, context.DeadlineExceeded
		case <-refreshTicker.C:
			shouldRefresh = true
		case <-eventTicker.C:
			matched, newOffset, err := beadEventsMentionSubject(eventLog, offset, beadID)
			if err != nil {
				t.Logf("event log read error while waiting for %s (falling back to periodic refresh): %v", beadID, err)
				continue
			}
			offset = newOffset
			shouldRefresh = matched
		}
		if !shouldRefresh {
			continue
		}

		bead, err := tryShowBead(cityDir, beadID)
		if err != nil {
			t.Logf("bd show error while waiting for %s (retrying): %v", beadID, err)
			continue
		}
		if predicate(bead) {
			return bead, nil
		}
	}
}

func waitForBeadMetadataValue(t *testing.T, cityDir, beadID, key string, timeout time.Duration) (graphBead, string, error) {
	t.Helper()

	bead, err := waitForBeadCondition(t, cityDir, beadID, timeout, func(bead graphBead) bool {
		return metaValue(bead, key) != ""
	})
	if err != nil {
		return graphBead{}, "", err
	}
	return bead, metaValue(bead, key), nil
}

func beadEventsMentionSubject(path string, offset int64, subject string) (bool, int64, error) {
	evts, newOffset, err := events.ReadFrom(path, offset)
	if err != nil {
		return false, newOffset, err
	}
	for _, evt := range evts {
		if evt.Subject != subject {
			continue
		}
		switch evt.Type {
		case events.BeadCreated, events.BeadUpdated, events.BeadClosed:
			return true, newOffset, nil
		}
	}
	return false, newOffset, nil
}

func eventLogOffset(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
