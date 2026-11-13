//go:build !windows

package proctable

import "github.com/gastownhall/gascity/internal/runtime"

func rootIsCityInfrastructure(live runtime.LiveRuntime) (bool, error) {
	argv, err := rootArgv(live.PID)
	if err != nil {
		return false, nil
	}
	return IsCityInfrastructureArgv(argv), nil
}
