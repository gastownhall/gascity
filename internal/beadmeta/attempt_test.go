package beadmeta

import "testing"

func TestRetryAttemptValuePrefersTheRetryKeyAndFallsBackForLegacyBeads(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want string
	}{
		{"new bead in a loop", map[string]string{"gc.retry_attempt": "1", "gc.attempt": "3", "gc.iteration": "3"}, "1"},
		{"v1.4.2 bead", map[string]string{"gc.attempt": "2"}, "2"},
		{"pre-key v1.5.0 body bead", map[string]string{"gc.attempt": "1", "gc.iteration": "3"}, "1"},
		{"no counters", map[string]string{}, ""},
	}
	for _, tc := range cases {
		if got := RetryAttemptValue(tc.meta); got != tc.want {
			t.Errorf("%s: RetryAttemptValue = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := RetryAttemptNumber(map[string]string{"gc.attempt": "x"}); got != 0 {
		t.Errorf("RetryAttemptNumber(non-numeric) = %d, want 0", got)
	}
}

func TestIsRetryAttemptSubject(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"v2 attempt", map[string]string{"gc.logical_bead_id": "gc-1", "gc.attempt": "1", "gc.retry_attempt": "1"}, true},
		{"v2 attempt with only the retry counter", map[string]string{"gc.logical_bead_id": "gc-1", "gc.retry_attempt": "2"}, true},
		{"v1.4.2 attempt with only gc.attempt", map[string]string{"gc.logical_bead_id": "gc-1", "gc.attempt": "2"}, true},
		{"v1 retry-run", map[string]string{"gc.logical_bead_id": "gc-1", "gc.kind": "retry-run"}, true},
		{"v1 retry-eval", map[string]string{"gc.logical_bead_id": "gc-1", "gc.kind": "retry-eval"}, true},
		{"logical bead id alone", map[string]string{"gc.logical_bead_id": "gc-1"}, false},
		{"logical bead id on another kind", map[string]string{"gc.logical_bead_id": "gc-1", "gc.kind": "retry"}, false},
		{"counters without a logical bead id", map[string]string{"gc.attempt": "1", "gc.retry_attempt": "1", "gc.kind": "retry-run"}, false},
		{"empty logical bead id", map[string]string{"gc.logical_bead_id": "", "gc.attempt": "1"}, false},
		{"no metadata", nil, false},
	}
	for _, tc := range cases {
		if got := IsRetryAttemptSubject(tc.meta); got != tc.want {
			t.Errorf("%s: IsRetryAttemptSubject(%v) = %t, want %t", tc.name, tc.meta, got, tc.want)
		}
	}
}

func TestIsAttemptRoot(t *testing.T) {
	type attemptRootCase struct {
		name string
		meta map[string]string
		want bool
	}
	cases := []attemptRootCase{
		{"attempt subject", map[string]string{"gc.logical_bead_id": "gc-1", "gc.attempt": "1"}, true},
		{"compiled attempt naming only its control", map[string]string{"gc.control_for": "work", "gc.attempt": "1", "gc.retry_attempt": "1"}, true},
		{"ralph iteration naming only its control", map[string]string{"gc.control_for": "loop", "gc.attempt": "1", "gc.iteration": "1"}, true},
		{"padded control_for", map[string]string{"gc.control_for": " work "}, true},
		{"blank control_for", map[string]string{"gc.control_for": " \t"}, false},
		{"no lineage", map[string]string{"gc.attempt": "1", "gc.retry_attempt": "1"}, false},
		{"no metadata", nil, false},
	}
	for _, kind := range ControlKinds {
		cases = append(cases, attemptRootCase{"control " + kind + " naming the step it serves", map[string]string{"gc.control_for": "work", "gc.kind": kind}, false})
	}
	for _, tc := range cases {
		if got := IsAttemptRoot(tc.meta); got != tc.want {
			t.Errorf("%s: IsAttemptRoot(%v) = %t, want %t", tc.name, tc.meta, got, tc.want)
		}
	}
}

func TestStampRetryAttemptKeepsTheIterationInGCAttempt(t *testing.T) {
	inLoop := map[string]string{"gc.iteration": "3"}
	StampRetryAttempt(inLoop, 2)
	if inLoop["gc.retry_attempt"] != "2" || inLoop["gc.attempt"] != "3" {
		t.Errorf("in a loop: got retry_attempt=%q attempt=%q, want 2 and 3", inLoop["gc.retry_attempt"], inLoop["gc.attempt"])
	}
	topLevel := map[string]string{}
	StampRetryAttempt(topLevel, 2)
	if topLevel["gc.retry_attempt"] != "2" || topLevel["gc.attempt"] != "2" {
		t.Errorf("top level: got retry_attempt=%q attempt=%q, want 2 and 2", topLevel["gc.retry_attempt"], topLevel["gc.attempt"])
	}
}
