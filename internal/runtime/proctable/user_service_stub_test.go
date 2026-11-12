//go:build !linux

package proctable

import "testing"

// TestDetachedUserServiceUnitStubIsEmpty pins the non-Linux stub: there is
// no procfs cgroup record to read, so no detached user service is recognized
// and the kill paths keep their prior behavior on every supported non-Linux
// platform build.
func TestDetachedUserServiceUnitStubIsEmpty(t *testing.T) {
	if got := DetachedUserServiceUnit(1234); got != "" {
		t.Fatalf("DetachedUserServiceUnit(1234) = %q, want empty off Linux", got)
	}
}
