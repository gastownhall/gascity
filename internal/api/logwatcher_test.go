package api

import (
	"context"
	"testing"
	"time"
)

func TestLogFileWatcherWakeResetsStallTimer(t *testing.T) {
	lw := &logFileWatcher{
		fallbackPoll: time.NewTicker(time.Hour),
	}
	defer lw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wake := make(chan struct{}, 1)
	emits := make(chan struct{}, 4)
	stalls := make(chan struct{}, 2)

	go lw.Run(
		ctx,
		func() bool {
			select {
			case emits <- struct{}{}:
			default:
			}
			return true
		},
		func() {},
		RunOpts{
			Wake:         wake,
			StallTimeout: 250 * time.Millisecond,
			OnStall: func() {
				select {
				case stalls <- struct{}{}:
				default:
				}
			},
		},
	)

	select {
	case <-emits:
	case <-time.After(time.Second):
		t.Fatal("initial readAndEmit did not run")
	}

	time.Sleep(150 * time.Millisecond)
	wake <- struct{}{}

	select {
	case <-emits:
	case <-time.After(time.Second):
		t.Fatal("wake-driven readAndEmit did not run")
	}

	select {
	case <-stalls:
		t.Fatal("stall fired before wake-driven progress had a full timeout window")
	case <-time.After(120 * time.Millisecond):
	}

	select {
	case <-stalls:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("stall did not fire after wake-reset timeout elapsed")
	}
}

func TestLogFileWatcherPollingWithoutProgressStillFiresStall(t *testing.T) {
	lw := &logFileWatcher{
		fallbackPoll: time.NewTicker(50 * time.Millisecond),
	}
	defer lw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stalls := make(chan struct{}, 2)

	go lw.Run(
		ctx,
		func() bool {
			return false
		},
		func() {},
		RunOpts{
			StallTimeout: 150 * time.Millisecond,
			OnStall: func() {
				select {
				case stalls <- struct{}{}:
				default:
				}
			},
		},
	)

	select {
	case <-stalls:
	case <-time.After(time.Second):
		t.Fatal("stall did not fire while fallback polling observed no progress")
	}
}
