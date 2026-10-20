package runtime_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func mirrorFailure(cause error) error {
	return &runtime.MetadataNotMirroredError{Session: "worker", Key: "GC_DRAIN", Cause: cause}
}

func TestMetadataNotMirroredErrorMatchesSentinelAndCause(t *testing.T) {
	cause := fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, errors.New("connection refused"))
	err := mirrorFailure(cause)
	if !errors.Is(err, runtime.ErrMetadataNotMirrored) {
		t.Fatalf("errors.Is(%v, ErrMetadataNotMirrored) = false", err)
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("errors.Is(%v, ErrRuntimeUnavailable) = false, want the cause reachable", err)
	}
}

func TestOnlyMetadataNotMirrored(t *testing.T) {
	unavailable := fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, errors.New("connection refused"))
	hard := errors.New("disk full")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain failure", hard, false},
		{"typed mirror failure with a multi-wrapped cause", mirrorFailure(unavailable), true},
		{"wrapped typed mirror failure", fmt.Errorf("setting drain: %w", mirrorFailure(unavailable)), true},
		{"bare sentinel", runtime.ErrMetadataNotMirrored, true},
		{"sentinel wrapped once", fmt.Errorf("%w: GC_DRAIN stored", runtime.ErrMetadataNotMirrored), true},
		{"join of mirror failures", errors.Join(mirrorFailure(unavailable), mirrorFailure(nil)), true},
		{"join with a hard failure", errors.Join(mirrorFailure(unavailable), hard), false},
		{"sentinel multi-wrapped beside an unrelated error", fmt.Errorf("%w: %w", runtime.ErrMetadataNotMirrored, hard), false},
		{"empty join", errors.Join(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtime.OnlyMetadataNotMirrored(tc.err); got != tc.want {
				t.Fatalf("OnlyMetadataNotMirrored(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
