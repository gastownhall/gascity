package exec //nolint:revive // internal package, always imported with alias

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// batchTestCallTimeout is the per-call timeout the batch tests set through
	// the s.timeout seam; the batch budget is execBatchBudgetMultiple times it.
	batchTestCallTimeout = 150 * time.Millisecond
	// batchTestBudget is the whole-batch deadline the tests expect with a
	// four-call multiple.
	batchTestBudget = 4 * batchTestCallTimeout
	// batchTestSentinel is a recognizable metadata value that must never reach
	// an error message.
	batchTestSentinel = "SENTINEL-VALUE-7f3a91"
)

var appliedRE = regexp.MustCompile(`after (\d+) of (\d+) keys applied`)

// batchLogScript returns a stub provider that appends one line per
// invocation to logPath ("<op> <id> <key> <stdin>") once the call completes.
// pre runs first and may sleep, exit non-zero or exec away.
func batchLogScript(logPath, pre string) string {
	return fmt.Sprintf(`
v=$(cat)
%s
printf '%%s %%s %%s %%s\n' "$1" "$2" "$3" "$v" >> %q
`, pre, logPath)
}

// readBatchLog returns the stub's log lines.
func readBatchLog(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// newBatchStore builds a Store over a stub script and returns it with the
// stub's log path.
func newBatchStore(t *testing.T, pre string, perCall time.Duration) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	s := NewStore(writeScript(t, dir, batchLogScript(logPath, pre)))
	s.timeout = perCall
	return s, logPath
}

// numberedKVs returns n keys that all carry value.
func numberedKVs(n int, value string) map[string]string {
	kvs := make(map[string]string, n)
	for i := 0; i < n; i++ {
		kvs[fmt.Sprintf("kk-%02d", i)] = value
	}
	return kvs
}

// slowSuccessPre makes every call succeed after 100 ms, without leaving a
// grandchild holding the provider's pipes (so a kill returns promptly).
const slowSuccessPre = `sleep 0.1 >/dev/null 2>&1 </dev/null`

// AT-E1: a slow-but-succeeding script across many keys is bounded by one
// deadline over the whole loop, and the error says how many keys were applied.
func TestSetMetadataBatch_DeadlineOverWholeLoop(t *testing.T) {
	const keys = 20
	s, logPath := newBatchStore(t, slowSuccessPre, batchTestCallTimeout)

	start := time.Now()
	err := s.SetMetadataBatch("ga-1", numberedKVs(keys, "v"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("SetMetadataBatch() = nil, want a batch deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, DeadlineExceeded) = false; err = %v", err)
	}
	if limit := batchTestBudget + 800*time.Millisecond; elapsed > limit {
		t.Errorf("SetMetadataBatch took %v, want about the %v budget (limit %v)", elapsed, batchTestBudget, limit)
	}
	m := appliedRE.FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("error %q has no %q text", err, "after N of M keys applied")
	}
	applied, _ := strconv.Atoi(m[1])
	total, _ := strconv.Atoi(m[2])
	if total != keys {
		t.Errorf("error reports %d total keys, want %d", total, keys)
	}
	// Every counted key's call completed (so it logged); the key in flight at
	// the deadline may or may not have logged before it was killed.
	logged := len(readBatchLog(t, logPath))
	if logged < applied || logged > applied+1 {
		t.Errorf("error reports %d keys applied but the stub logged %d completed calls", applied, logged)
	}
	if applied == 0 || applied >= keys {
		t.Errorf("keys applied = %d, want some but not all of %d", applied, keys)
	}
	if !strings.Contains(err.Error(), `"ga-1"`) {
		t.Errorf("error %q does not name the bead id", err)
	}
}

// AT-E2: keys are applied in sorted order, whatever the map order.
func TestSetMetadataBatch_AppliesKeysInSortedOrder(t *testing.T) {
	want := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	for run := 0; run < 4; run++ {
		s, logPath := newBatchStore(t, ``, 5*time.Second)
		kvs := make(map[string]string, len(want))
		for _, k := range want {
			kvs[k] = "v-" + k
		}
		if err := s.SetMetadataBatch("ga-1", kvs); err != nil {
			t.Fatalf("run %d: SetMetadataBatch() = %v", run, err)
		}
		var got []string
		for _, line := range readBatchLog(t, logPath) {
			f := strings.Fields(line)
			if len(f) < 3 {
				t.Fatalf("run %d: malformed log line %q", run, line)
			}
			got = append(got, f[2])
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d: keys applied in order %v, want %v", run, got, want)
		}
	}
}

// AT-E3: a key that fails on its own returns at once with today's error and
// the later keys never run.
func TestSetMetadataBatch_KeyFailureStopsWithUnchangedError(t *testing.T) {
	s, logPath := newBatchStore(t, `[ "$3" = charlie ] && { echo boom >&2; exit 1; }`, 5*time.Second)

	kvs := map[string]string{"echo": "5", "delta": "4", "charlie": "3", "bravo": "2", "alpha": "1"}
	err := s.SetMetadataBatch("ga-1", kvs)
	if err == nil {
		t.Fatal("SetMetadataBatch() = nil, want the failing key's error")
	}
	if !strings.HasPrefix(err.Error(), `setting metadata on "ga-1": exec beads `) {
		t.Errorf("error = %q, want today's %q prefix", err, `setting metadata on "ga-1": exec beads `)
	}
	if !strings.HasSuffix(err.Error(), "set-metadata ga-1 charlie: boom") {
		t.Errorf("error = %q, want the script's own message for key charlie", err)
	}
	if strings.Contains(err.Error(), "keys applied") {
		t.Errorf("a plain key failure must not read as a batch deadline: %q", err)
	}
	for _, line := range readBatchLog(t, logPath) {
		if strings.Contains(line, " delta") || strings.Contains(line, " echo") {
			t.Errorf("key after the failing one ran: log line %q", line)
		}
	}
}

// AT-E4: a script that hangs on one call hits the per-call timeout while the
// batch deadline is still live, so the error is today's, not the batch one.
func TestSetMetadataBatch_PerCallTimeoutKeepsTodaysError(t *testing.T) {
	s, _ := newBatchStore(t, `exec sleep 60`, 50*time.Millisecond)

	start := time.Now()
	err := s.SetMetadataBatch("ga-1", map[string]string{"alpha": "1", "bravo": "2"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("SetMetadataBatch() = nil, want a per-call timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("SetMetadataBatch took %v, want about the 50ms per-call timeout", elapsed)
	}
	if !strings.HasPrefix(err.Error(), `setting metadata on "ga-1": exec beads `) {
		t.Errorf("error = %q, want today's per-call error", err)
	}
	if strings.Contains(err.Error(), "keys applied") {
		t.Errorf("per-call timeout with a live batch deadline must not read as a batch error: %q", err)
	}
}

// AT-E5: a healthy batch applies every key through set-metadata only, with
// each value on stdin.
func TestSetMetadataBatch_HealthyBatchUsesOnlySetMetadata(t *testing.T) {
	s, logPath := newBatchStore(t, ``, 5*time.Second)
	kvs := map[string]string{"alpha": "value one", "bravo": "value-two", "charlie": ""}

	if err := s.SetMetadataBatch("ga-1", kvs); err != nil {
		t.Fatalf("SetMetadataBatch() = %v", err)
	}
	want := []string{
		"set-metadata ga-1 alpha value one",
		"set-metadata ga-1 bravo value-two",
		"set-metadata ga-1 charlie ",
	}
	if got := readBatchLog(t, logPath); !slices.Equal(got, want) {
		t.Errorf("stub log = %q, want %q", got, want)
	}
}

// AT-E6: the batch error holds no metadata value and says the in-flight key's
// outcome is unknown (ExecStore never claims a timed-out write did not land).
func TestSetMetadataBatch_DeadlineErrorLeaksNoValue(t *testing.T) {
	s, _ := newBatchStore(t, slowSuccessPre, batchTestCallTimeout)

	err := s.SetMetadataBatch("ga-1", numberedKVs(20, batchTestSentinel))

	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SetMetadataBatch() = %v, want a batch deadline error", err)
	}
	if strings.Contains(err.Error(), batchTestSentinel) {
		t.Errorf("error %q leaks a metadata value", err)
	}
	if strings.Contains(err.Error(), "kk-") {
		t.Errorf("error %q leaks a metadata key", err)
	}
	if !strings.Contains(err.Error(), "the outcome of the key in flight, if any, is unknown") {
		t.Errorf("error %q must say a killed script may already have written", err)
	}
}
