package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRuntimeTokenVerdictClassifies(t *testing.T) {
	cases := []struct {
		name   string
		actual string
		err    error
		want   runtimeTokenVerdict
	}{
		{name: "match", actual: "tok-a", want: runtimeTokenMatch},
		{name: "match_with_whitespace", actual: " tok-a\n", want: runtimeTokenMatch},
		{name: "mismatch", actual: "tok-b", want: runtimeTokenMismatch},
		{name: "unset", actual: "", want: runtimeTokenAbsent},
		{name: "meta_unsupported", err: fmt.Errorf("exec get-meta: %w", runtime.ErrMetaUnsupported), want: runtimeTokenAbsent},
		{name: "session_not_found", err: fmt.Errorf("show-environment: %w", runtime.ErrSessionNotFound), want: runtimeTokenGone},
		{name: "transport", err: fmt.Errorf("show-environment timed out: %w", runtime.ErrRuntimeUnavailable), want: runtimeTokenUnverifiable},
		// Message text that IsSessionGone would read as gone is not the sentinel.
		{name: "untyped_not_found_text", err: errors.New("kubectl exec: pod not found in cache"), want: runtimeTokenUnverifiable},
		// An error with a stale value beside it is still unverifiable.
		{name: "error_with_value", actual: "tok-a", err: errors.New("read failed"), want: runtimeTokenUnverifiable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRuntimeInstanceToken(tc.actual, tc.err, "tok-a")
			if got != tc.want {
				t.Fatalf("classifyRuntimeInstanceToken(%q, %v) = %v, want %v", tc.actual, tc.err, got, tc.want)
			}
		})
	}
}
