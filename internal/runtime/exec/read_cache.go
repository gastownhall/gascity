package exec

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// readCacheTTLEnv names the environment variable that sets how long the exec
// provider reuses the answer of a plain liveness or metadata read; see
// [readCache]. Zero or a negative duration turns the cache off.
const readCacheTTLEnv = "GC_EXEC_CACHE_TTL"

// defaultReadCacheTTL is the read cache's default time-to-live, the same as
// the tmux provider's state cache (GC_TMUX_CACHE_TTL).
const defaultReadCacheTTL = 2 * time.Second

// readCacheSweepAt is the entry count past which a store first drops the
// entries that have expired, so a city whose session names churn does not
// grow the map without bound.
const readCacheSweepAt = 1024

// readCache keeps, for a short TTL, the stdout of the exec provider's plain
// reads (is-running, process-alive, get-meta, get-last-activity), keyed by
// the op's full argv and stdin.
//
// Every read is a fork/exec of the pack's script, and one controller pass
// asks the same question about the same session several times: the seams
// re-check a box's liveness before each attachment read, and a pass reads a
// session's liveness, activity and metadata from separate places. Against
// the tmux provider those repeats are answered from its state cache; against
// an exec pack each is another process. The cache answers the repeats from
// the first read, the way the tmux cache does.
//
// It changes how often the script is asked, never what an answer means:
//   - only a read that succeeded is kept; an error is asked again;
//   - concurrent identical reads share one op (singleflight);
//   - every other op clears the cache before and after it runs, so a read
//     never answers from before a write gc made, and a read that a write
//     overtook is not kept;
//   - the reads that decide what gc does to a box (a start's vacancy check,
//     a driving verb's presence check) ask the script and refresh the cache.
//
// A change made outside this provider — the agent exiting, another gc
// process writing metadata — is seen within one TTL, as with tmux. The exec
// bool reads were never fresh reads in the runtime contract
// ([runtime.FreshReadable] is false for them), so callers that need a fresh
// answer already do not take it from these reads.
//
// A nil *readCache reads through; its methods are safe to call.
type readCache struct {
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	gen   uint64
	items map[string]readCacheEntry

	group singleflight.Group
}

type readCacheEntry struct {
	out string
	at  time.Time
}

// newReadCache returns a read cache with the given TTL; a TTL at or below
// zero returns nil, which reads through.
func newReadCache(ttl time.Duration) *readCache {
	if ttl <= 0 {
		return nil
	}
	return &readCache{ttl: ttl, now: time.Now, items: make(map[string]readCacheEntry)}
}

// readCacheKey joins an op's argv and stdin into one cache key.
func readCacheKey(stdin []byte, args ...string) string {
	return strings.Join(args, "\x00") + "\x01" + string(stdin)
}

// read answers key from the cache when an entry younger than the TTL is
// there, and otherwise runs fetch once for every concurrent reader of the
// same key, keeping a successful answer.
func (c *readCache) read(key string, fetch func() (string, error)) (string, error) {
	if c == nil {
		return fetch()
	}
	c.mu.Lock()
	gen := c.gen
	if e, ok := c.items[key]; ok && c.now().Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.out, nil
	}
	c.mu.Unlock()

	// The flight is keyed by generation too: a reader that comes after a
	// write never joins a read the write may have overtaken.
	v, err, _ := c.group.Do(strconv.FormatUint(gen, 10)+"\x02"+key, func() (any, error) {
		return c.fetchAndStore(key, gen, fetch)
	})
	out, _ := v.(string)
	return out, err
}

// fresh runs fetch without consulting the cache and keeps a successful
// answer for the plain reads that follow.
func (c *readCache) fresh(key string, fetch func() (string, error)) (string, error) {
	if c == nil {
		return fetch()
	}
	c.mu.Lock()
	gen := c.gen
	c.mu.Unlock()
	return c.fetchAndStore(key, gen, fetch)
}

// fetchAndStore runs fetch and keeps its answer, stamped with the time the
// read began, unless it failed or a write cleared the cache meanwhile.
func (c *readCache) fetchAndStore(key string, gen uint64, fetch func() (string, error)) (string, error) {
	at := c.now()
	out, err := fetch()
	if err != nil {
		return out, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return out, nil
	}
	if len(c.items) >= readCacheSweepAt {
		now := c.now()
		for k, e := range c.items {
			if now.Sub(e.at) >= c.ttl {
				delete(c.items, k)
			}
		}
	}
	c.items[key] = readCacheEntry{out: out, at: at}
	return out, nil
}

// invalidate drops every kept answer and every read in flight from being
// kept.
func (c *readCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.gen++
	clear(c.items)
	c.mu.Unlock()
}

// readCacheOps are the ops whose answers the cache keeps.
var readCacheOps = map[string]bool{
	"is-running":        true,
	"process-alive":     true,
	"get-meta":          true,
	"get-last-activity": true,
}

// opLeavesStateAlone reports the ops that only read, so running one does
// not clear the cache. Any op not listed, including one added later, is
// taken as a write.
func opLeavesStateAlone(op string) bool {
	if readCacheOps[op] {
		return true
	}
	switch op {
	case "protocol", "list-running", "inventory", "observe", "peek", "is-attached", "check-image":
		return true
	}
	return false
}

// readCacheTTLFromEnv reads GC_EXEC_CACHE_TTL as a Go duration ("2s",
// "500ms") or as integer milliseconds ("2000"). Unset or empty is
// defaultReadCacheTTL; zero or a negative duration turns the cache off; a
// value that does not parse logs and keeps the default.
func readCacheTTLFromEnv() time.Duration {
	v := os.Getenv(readCacheTTLEnv)
	if v == "" {
		return defaultReadCacheTTL
	}
	if d, err := time.ParseDuration(v); err == nil {
		return max(d, 0)
	}
	if strings.TrimSpace(v) == v {
		if ms, err := strconv.Atoi(v); err == nil {
			return max(time.Duration(ms)*time.Millisecond, 0)
		}
	}
	log.Printf("exec read cache: invalid %s=%q, using default %v", readCacheTTLEnv, v, defaultReadCacheTTL)
	return defaultReadCacheTTL
}
