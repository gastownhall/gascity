//go:build !linux

package session

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

type unsupportedNativeCapacityHost struct{}

func newNativeCapacityHost() nativeCapacityHost { return unsupportedNativeCapacityHost{} }
func (unsupportedNativeCapacityHost) Identity() (nativeKernelIdentity, error) {
	return nativeKernelIdentity{}, fmt.Errorf("native managed admission requires Linux cgroup2")
}

func (unsupportedNativeCapacityHost) Resources(nativeCapacityConfig) error {
	return fmt.Errorf("native managed admission requires Linux cgroup2")
}

func (unsupportedNativeCapacityHost) Command(string) (runtime.CommandScope, error) {
	return runtime.CommandScope{}, fmt.Errorf("native managed admission requires Linux cgroup2")
}

func (unsupportedNativeCapacityHost) Observe(nativeCapacitySeat) (nativeScopeObservation, error) {
	return nativeScopeObservation{}, fmt.Errorf("native managed admission requires Linux cgroup2")
}

func (unsupportedNativeCapacityHost) SelfScope(string) (nativeScopeObservation, error) {
	return nativeScopeObservation{}, fmt.Errorf("native managed admission requires Linux cgroup2")
}
