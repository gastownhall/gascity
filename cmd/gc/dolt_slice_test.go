package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/systemdscope"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestManagedDoltSliceFor(t *testing.T) {
	tests := []struct {
		name     string
		testMode bool
		envValue string
		envSet   bool
		want     string
	}{
		{name: "unset uses the built-in default", want: managedDoltDefaultSlice},
		{name: "explicit slice wins", envValue: "custom.slice", envSet: true, want: "custom.slice"},
		{name: "explicit empty resolves empty for fail-closed validation", envValue: "", envSet: true, want: ""},
		{name: "whitespace is trimmed", envValue: "  s.slice  ", envSet: true, want: "s.slice"},
		{name: "test mode drops the implicit default", testMode: true, want: ""},
		{
			name:     "test mode still honors an explicit slice",
			testMode: true, envValue: "s.slice", envSet: true, want: "s.slice",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltSliceFor(tc.testMode, tc.envValue, tc.envSet); got != tc.want {
				t.Errorf("managedDoltSliceFor(%v, %q, %v) = %q, want %q",
					tc.testMode, tc.envValue, tc.envSet, got, tc.want)
			}
		})
	}
}

// TestManagedDoltDefaultSliceEscapesTheAgentMemcg is the regression guard for
// the mistake this fix exists to correct. systemd derives slice nesting from
// the unit name: "a-b.slice" is a child of "a.slice". A default named
// "gascity-dolt.slice" would therefore place the shared bead store back inside
// gascity.slice — the exact memcg whose MemoryMax breach OOM-killed dolt four
// times on 2026-07-21 (oom_memcg=.../gascity.slice, CONSTRAINT_MEMCG). The
// dolt slice must be a sibling of the agent slice, never a descendant.
func TestManagedDoltDefaultSliceEscapesTheAgentMemcg(t *testing.T) {
	if !strings.HasSuffix(managedDoltDefaultSlice, ".slice") {
		t.Fatalf("default slice %q is not a .slice unit", managedDoltDefaultSlice)
	}
	if strings.HasPrefix(managedDoltDefaultSlice, "gascity-") {
		t.Fatalf("default slice %q nests under gascity.slice, which is the memcg that kills dolt",
			managedDoltDefaultSlice)
	}
	if strings.Contains(strings.TrimSuffix(managedDoltDefaultSlice, ".slice"), "-") {
		t.Fatalf("default slice %q nests under a parent slice; it must be top-level",
			managedDoltDefaultSlice)
	}
}

// TestManagedDoltOOMScoreAdjNeedsLowering pins the direction of the
// adjustment: only ever downward, and never below zero. The rationale for the
// target lives on managedDoltOOMScoreAdj; what this pins is that an operator
// value at or below it survives untouched.
func TestManagedDoltOOMScoreAdjNeedsLowering(t *testing.T) {
	tests := []struct {
		name    string
		current int
		want    bool
	}{
		{name: "inherited user-manager default is lowered", current: 200, want: true},
		{name: "already neutral is left alone", current: 0, want: false},
		{name: "operator-set negative is preserved", current: -500, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltOOMScoreAdjNeedsLowering(tc.current); got != tc.want {
				t.Errorf("managedDoltOOMScoreAdjNeedsLowering(%d) = %v, want %v", tc.current, got, tc.want)
			}
		})
	}
}

// TestManagedDoltPlacementOffUnderTestWithoutProbing guards the CI contract:
// with no explicit slice the managed-dolt suite must not touch systemd at all,
// so it neither depends on a user manager nor pays the probe timeout on hosts
// without one.
func TestManagedDoltPlacementOffUnderTestWithoutProbing(t *testing.T) {
	if got := managedDoltSlice(); got != "" {
		t.Fatalf("managedDoltSlice() = %q inside the test binary, want no placement", got)
	}
	argv := []string{"dolt", "sql-server", "--config", "/tmp/c.yaml"}
	type result struct {
		argv []string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		got, err := wrapManagedDoltArgv(argv)
		done <- result{argv: got, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("wrapManagedDoltArgv: %v", got.err)
		}
		if strings.Join(got.argv, "\x00") != strings.Join(argv, "\x00") {
			t.Fatalf("wrapManagedDoltArgv = %q, want unwrapped %q", got.argv, argv)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("wrapManagedDoltArgv blocked; it must not probe systemd when placement is off")
	}
}

func TestPrepareManagedDoltPlacementAppliesBoundedPolicy(t *testing.T) {
	var gotSlice string
	var gotPolicy systemdscope.SlicePolicy
	previousEnsure := managedDoltEnsureSlice
	managedDoltEnsureSlice = func(_ context.Context, slice string, policy systemdscope.SlicePolicy) error {
		gotSlice = slice
		gotPolicy = policy
		return nil
	}
	t.Cleanup(func() { managedDoltEnsureSlice = previousEnsure })

	if err := prepareManagedDoltPlacement(context.Background(), "custom.slice"); err != nil {
		t.Fatalf("prepareManagedDoltPlacement: %v", err)
	}
	if gotSlice != "custom.slice" {
		t.Fatalf("slice = %q, want custom.slice", gotSlice)
	}
	wantPolicy := systemdscope.SlicePolicy{
		MemoryMax:            managedDoltMemoryMaxBytes,
		MemoryLow:            managedDoltMemoryLowBytes,
		ManagedOOMPreference: managedDoltManagedOOMPreference,
	}
	if !reflect.DeepEqual(gotPolicy, wantPolicy) {
		t.Fatalf("policy = %#v, want %#v", gotPolicy, wantPolicy)
	}
}

func TestPrepareManagedDoltPlacementRejectsNestedSlice(t *testing.T) {
	calls := 0
	previousEnsure := managedDoltEnsureSlice
	managedDoltEnsureSlice = func(context.Context, string, systemdscope.SlicePolicy) error {
		calls++
		return nil
	}
	t.Cleanup(func() { managedDoltEnsureSlice = previousEnsure })

	err := prepareManagedDoltPlacement(context.Background(), "gascity-dolt.slice")
	if err == nil || !strings.Contains(err.Error(), "not top-level") {
		t.Fatalf("prepareManagedDoltPlacement error = %v, want nested-slice rejection", err)
	}
	if calls != 0 {
		t.Fatalf("systemd policy called %d times for nested slice", calls)
	}
}

func TestWrapManagedDoltArgvFailsClosedWhenPlacementCannotBePrepared(t *testing.T) {
	previousEnsure := managedDoltEnsureSlice
	managedDoltEnsureSlice = func(context.Context, string, systemdscope.SlicePolicy) error {
		return errors.New("policy unavailable")
	}
	t.Cleanup(func() { managedDoltEnsureSlice = previousEnsure })

	got, err := wrapManagedDoltArgvFor([]string{"dolt", "sql-server"}, "required.slice", true)
	if err == nil {
		t.Fatalf("wrapManagedDoltArgv error = nil, got %q", got)
	}
	if len(got) != 0 {
		t.Fatalf("wrapManagedDoltArgv returned unsafe fallback %q", got)
	}
	if !strings.Contains(err.Error(), "policy unavailable") {
		t.Fatalf("error = %q, want policy cause", err)
	}
}

func TestWrapManagedDoltArgvRejectsExplicitEmptySlice(t *testing.T) {
	got, err := wrapManagedDoltArgvFor([]string{"dolt", "sql-server"}, "", true)
	if err == nil {
		t.Fatalf("wrapManagedDoltArgv error = nil, got %q", got)
	}
	if len(got) != 0 {
		t.Fatalf("wrapManagedDoltArgv returned unsafe fallback %q", got)
	}
	if !strings.Contains(err.Error(), managedDoltSliceEnv+" is empty") {
		t.Fatalf("error = %q, want explicit empty-slice cause", err)
	}
}

func TestWrapManagedDoltArgvFailsClosedWhenRequiredProbeFails(t *testing.T) {
	previousEnsure := managedDoltEnsureSlice
	managedDoltEnsureSlice = func(context.Context, string, systemdscope.SlicePolicy) error { return nil }
	t.Cleanup(func() { managedDoltEnsureSlice = previousEnsure })

	previousWrapper := managedDoltSliceWrapper
	managedDoltSliceWrapper = &systemdscope.Wrapper{
		Probe: func(string) error { return errors.New("user bus unavailable") },
		Warn:  &strings.Builder{},
		Label: managedDoltSliceEnv,
	}
	t.Cleanup(func() { managedDoltSliceWrapper = previousWrapper })

	got, err := wrapManagedDoltArgvFor([]string{"dolt", "sql-server"}, "required.slice", true)
	if err == nil {
		t.Fatalf("wrapManagedDoltArgv error = nil, got %q", got)
	}
	if len(got) != 0 {
		t.Fatalf("wrapManagedDoltArgv returned unsafe fallback %q", got)
	}
}

// TestManagedDoltSliceWrapperIsLabeled pins the one cmd/gc-side property of the
// shared wrapper: a probe failure must name the knob an operator would change.
// The fallback behavior itself is covered in the systemdscope package.
func TestManagedDoltSliceWrapperIsLabeled(t *testing.T) {
	if managedDoltSliceWrapper.Label != managedDoltSliceEnv {
		t.Errorf("wrapper label = %q, want %q", managedDoltSliceWrapper.Label, managedDoltSliceEnv)
	}
}
