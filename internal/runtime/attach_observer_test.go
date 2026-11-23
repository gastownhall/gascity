package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// boolAttachProvider hides every optional capability of the wrapped provider:
// only the Provider method set is promoted, so it models a legacy provider
// that reports attachment through IsAttached alone.
type boolAttachProvider struct{ Provider }

func TestIsAttachedWithErrorFallsBackWithoutCapability(t *testing.T) {
	f := NewFake()
	f.SetAttached("attached", true)
	// Invisible through the bool-only wrapper: the fallback must not
	// manufacture an error for a provider that cannot report one.
	f.AttachedErrors["attached"] = fmt.Errorf("probe: %w", ErrRuntimeUnavailable)
	sp := boolAttachProvider{f}
	if _, ok := Provider(sp).(AttachmentObserverWithError); ok {
		t.Fatal("test provider unexpectedly implements AttachmentObserverWithError")
	}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"attached", true},
		{"detached", false},
	} {
		got, err := IsAttachedWithError(sp, tc.name)
		if err != nil || got != tc.want {
			t.Errorf("IsAttachedWithError(%q) = (%v, %v), want (%v, nil)", tc.name, got, err, tc.want)
		}
	}
	if got := f.CountCalls("IsAttached", "attached"); got != 1 {
		t.Errorf("IsAttached calls = %d, want 1 (the fallback answers through IsAttached)", got)
	}
}

func TestIsAttachedWithErrorPrefersCapability(t *testing.T) {
	f := NewFake()
	f.SetAttached("worker", true)
	probeErr := fmt.Errorf("probe timed out: %w", ErrRuntimeUnavailable)
	f.AttachedErrors["worker"] = probeErr

	got, err := IsAttachedWithError(f, "worker")
	if !errors.Is(err, probeErr) || got {
		t.Fatalf("IsAttachedWithError = (%v, %v), want (false, %v)", got, err, probeErr)
	}
	if n := f.CountCalls("IsAttached", "worker"); n != 0 {
		t.Errorf("IsAttached calls = %d, want 0 (the capability was bypassed)", n)
	}
}

func TestIsAttachedWithErrorNilProvider(t *testing.T) {
	got, err := IsAttachedWithError(nil, "worker")
	if got || err != nil {
		t.Fatalf("IsAttachedWithError(nil) = (%v, %v), want (false, nil)", got, err)
	}
}

// A blank name never reaches the provider: tmux resolves an empty target to
// some other session, so any answer would be about the wrong session.
func TestIsAttachedWithErrorBlankNameSkipsProvider(t *testing.T) {
	f := NewFake()
	for _, name := range []string{"", "  "} {
		f.SetAttached(name, true)
		f.AttachedErrors[name] = fmt.Errorf("probe: %w", ErrRuntimeUnavailable)
		got, err := IsAttachedWithError(f, name)
		if got || err != nil {
			t.Errorf("IsAttachedWithError(%q) = (%v, %v), want (false, nil)", name, got, err)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %v, want none", f.Calls)
	}
}

// Only a vanished session reads as not attached; an unavailable probe, an
// untyped error, or one whose text merely says "not found" holds.
func TestAttachProbeHolds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attached bool
		err      error
		want     bool
	}{
		{name: "attached", attached: true, want: true},
		{name: "detached", want: false},
		{name: "session not found", err: fmt.Errorf("probe: %w", ErrSessionNotFound), want: false},
		{name: "unavailable", err: fmt.Errorf("probe: %w", ErrRuntimeUnavailable), want: true},
		{name: "untyped", err: errors.New("probe failed"), want: true},
		{name: "not found text only", err: errors.New("can't find session: not found"), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AttachProbeHolds(tc.attached, tc.err); got != tc.want {
				t.Fatalf("AttachProbeHolds(%v, %v) = %v, want %v", tc.attached, tc.err, got, tc.want)
			}
		})
	}
}
