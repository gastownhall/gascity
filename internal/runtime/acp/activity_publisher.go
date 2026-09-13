package acp

import (
	"sync"
	"time"
)

// activityPublishInterval bounds durable activity-stamp write amplification.
// Activity remains exact in memory; the cross-process sidecar trails by at
// most this interval while updates continue.
const activityPublishInterval = 5 * time.Second

// activityPublishRetryInterval keeps a transient sidecar failure from
// suppressing publication for a full activity interval.
const activityPublishRetryInterval = time.Second

// activityPublisher serializes, coalesces, and throttles durable activity
// writes. offer never performs I/O and never waits for the worker.
type activityPublisher struct {
	interval time.Duration
	publish  func(time.Time) error
	onError  func(error)

	mu      sync.Mutex
	latest  time.Time
	pending bool
	stopped bool

	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newActivityPublisher(
	interval time.Duration,
	lastWrite time.Time,
	publish func(time.Time) error,
	onError func(error),
) *activityPublisher {
	if interval <= 0 {
		interval = activityPublishInterval
	}
	ap := &activityPublisher{
		interval: interval,
		publish:  publish,
		onError:  onError,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go ap.run(lastWrite)
	return ap
}

// offer records the newest observed timestamp and wakes the publisher without
// waiting for filesystem I/O. Older timestamps are ignored so the durable
// value cannot move backwards even if callers race.
func (ap *activityPublisher) offer(stamp time.Time) {
	ap.mu.Lock()
	if ap.stopped || (!ap.latest.IsZero() && !stamp.After(ap.latest)) {
		ap.mu.Unlock()
		return
	}
	ap.latest = stamp
	ap.pending = true
	ap.mu.Unlock()

	select {
	case ap.wake <- struct{}{}:
	default:
	}
}

// close stops the worker and waits until no publication can still be in
// flight. Stop uses this before removing sidecars, preventing a late write
// from recreating activity metadata for a removed session.
func (ap *activityPublisher) close() {
	ap.stopOnce.Do(func() {
		ap.mu.Lock()
		ap.stopped = true
		ap.mu.Unlock()
		close(ap.stop)
	})
	<-ap.done
}

func (ap *activityPublisher) run(lastWrite time.Time) {
	defer close(ap.done)

	retrying := false
	var retryAt time.Time
	reportedFailure := false
	for {
		_, ok := ap.pendingStamp()
		if !ok {
			select {
			case <-ap.wake:
				continue
			case <-ap.stop:
				ap.flushOnStop(reportedFailure)
				return
			}
		}

		delay := time.Until(lastWrite.Add(ap.interval))
		if retrying {
			delay = time.Until(retryAt)
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ap.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				continue
			case <-ap.stop:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				ap.flushOnStop(reportedFailure)
				return
			}
		}

		// Re-snapshot after the throttle wait so a burst becomes one write of
		// the newest timestamp rather than one write of the first timestamp.
		stamp, ok := ap.pendingStamp()
		if !ok {
			continue
		}
		if err := ap.publish(stamp); err != nil {
			if !reportedFailure && ap.onError != nil {
				ap.onError(err)
				reportedFailure = true
			}
			retrying = true
			retryDelay := activityPublishRetryInterval
			if ap.interval < retryDelay {
				retryDelay = ap.interval
			}
			retryAt = time.Now().Add(retryDelay)
			continue
		}

		lastWrite = time.Now()
		retrying = false
		reportedFailure = false
		ap.markPublished(stamp)
	}
}

// flushOnStop makes one final best-effort attempt for a coalesced update that
// was still inside the throttle or retry window. close waits for this attempt,
// so no write can recreate metadata after lifecycle cleanup proceeds.
func (ap *activityPublisher) flushOnStop(failureAlreadyReported bool) {
	ap.mu.Lock()
	stamp, pending := ap.latest, ap.pending
	ap.mu.Unlock()
	if !pending {
		return
	}
	if err := ap.publish(stamp); err != nil {
		if !failureAlreadyReported && ap.onError != nil {
			ap.onError(err)
		}
		return
	}
	ap.markPublished(stamp)
}

func (ap *activityPublisher) pendingStamp() (time.Time, bool) {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if ap.stopped {
		return time.Time{}, false
	}
	return ap.latest, ap.pending
}

func (ap *activityPublisher) markPublished(stamp time.Time) {
	ap.mu.Lock()
	if !ap.latest.After(stamp) {
		ap.pending = false
	}
	ap.mu.Unlock()
}
