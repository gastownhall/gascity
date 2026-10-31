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
