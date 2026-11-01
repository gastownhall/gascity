package tap

import "time"

// config is the tap's timing policy (design §4.2). Run uses
// defaultConfig; tests shrink the waits.
type config struct {
	readSize     int           // bytes per read of pane output
	minInterval  time.Duration // least time between publishes
	maxInterval  time.Duration // cap of the churn backoff
	churnRate    int           // record changes per second above which output churns
	churnWindow  time.Duration // churn lasting this long starts the backoff
	calmWindow   time.Duration // calm lasting this long resets it
	writeTimeout time.Duration // bound on one tmux write
	retryMin     time.Duration // first retry delay after a failed write
	retryMax     time.Duration // cap of the retry delay
	maxFailures  int           // consecutive failed writes before the tap exits
}

func defaultConfig() config {
	return config{
		readSize:     32 << 10,
		minInterval:  25 * time.Millisecond,
		maxInterval:  time.Second,
		churnRate:    10,
		churnWindow:  5 * time.Second,
		calmWindow:   5 * time.Second,
		writeTimeout: 2 * time.Second,
		retryMin:     100 * time.Millisecond,
		retryMax:     time.Second,
		maxFailures:  5,
	}
}

// retryDelay is the wait after the n-th consecutive failed write: retryMin
// doubling per failure, capped at retryMax.
func (c config) retryDelay(n int) time.Duration {
	d := c.retryMin
	for i := 1; i < n && d < c.retryMax; i++ {
		d *= 2
	}
	return min(d, c.retryMax)
}

// rateBucket is the span over which the record change rate is measured.
const rateBucket = time.Second

// coalescer chooses the least interval between publishes. It measures the
// rate of record changes (demand, not publishes, which the interval itself
// throttles) over buckets of at least one second, observed at each publish.
// Changes above churnRate per second for churnWindow double the interval
// once per bucket up to maxInterval; calm for calmWindow resets it to
// minInterval.
type coalescer struct {
	cfg      config
	interval time.Duration

	bucketStart   time.Time
	bucketChanges uint64
	hotSince      time.Time // start of the current churn; zero when calm
	calmSince     time.Time // start of the current calm; zero when churning
}

func newCoalescer(cfg config) *coalescer {
	return &coalescer{cfg: cfg, interval: cfg.minInterval}
}

// observe records a publish at now, when changes record changes had been
// read in total, and updates the interval.
func (c *coalescer) observe(now time.Time, changes uint64) {
	if c.bucketStart.IsZero() {
		c.bucketStart, c.bucketChanges = now, changes
		return
	}
	elapsed := now.Sub(c.bucketStart)
	if elapsed < rateBucket {
		return
	}
	start := c.bucketStart
	hot := float64(changes-c.bucketChanges) > float64(c.cfg.churnRate)*elapsed.Seconds()
	c.bucketStart, c.bucketChanges = now, changes
	if hot {
		c.calmSince = time.Time{}
		if c.hotSince.IsZero() {
			c.hotSince = start
		}
		if now.Sub(c.hotSince) >= c.cfg.churnWindow {
			c.interval = min(2*c.interval, c.cfg.maxInterval)
		}
		return
	}
	c.hotSince = time.Time{}
	if c.calmSince.IsZero() {
		c.calmSince = start
	}
	if now.Sub(c.calmSince) >= c.cfg.calmWindow {
		c.interval = c.cfg.minInterval
	}
}
