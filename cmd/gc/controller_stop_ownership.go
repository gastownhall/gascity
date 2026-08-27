package main

import (
	"errors"
	"fmt"
	"os"
	"time"
)

var (
	errControllerStopOwnershipUnproven    = errors.New("controller stop ownership is unproven")
	errControllerStopSocketReplaced       = errors.New("controller socket was replaced")
	errControllerStopOwnershipWaitTimeout = errors.New("timed out waiting for controller stop ownership")
)

type controllerStopOwnershipOps struct {
	stat    func(string) (os.FileInfo, error)
	acquire func(string) (*os.File, error)
	now     func() time.Time
	retry   func()
}

func waitForAcknowledgedControllerOwnership(cityPath string, result controllerStopResult, timeout time.Duration) (*os.File, error) {
	return waitForAcknowledgedControllerOwnershipWithOps(
		cityPath,
		result,
		timeout,
		controllerStopOwnershipOps{
			stat:    os.Stat,
			acquire: acquireControllerLock,
			now:     time.Now,
			retry:   func() { time.Sleep(50 * time.Millisecond) },
		},
	)
}

func waitForAcknowledgedControllerOwnershipWithOps(
	cityPath string,
	result controllerStopResult,
	timeout time.Duration,
	ops controllerStopOwnershipOps,
) (*os.File, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := ops.now().Add(timeout)
	for {
		if err := validateAcknowledgedControllerSocket(cityPath, result, ops.stat, false); err != nil {
			return nil, err
		}
		ownership, err := ops.acquire(cityPath)
		switch {
		case err == nil && ownership == nil:
			return nil, fmt.Errorf("%w: lock acquisition returned no ownership", errControllerStopOwnershipUnproven)
		case err == nil:
			if err := validateAcknowledgedControllerSocket(cityPath, result, ops.stat, true); err != nil {
				_ = ownership.Close()
				return nil, err
			}
			return ownership, nil
		case !errors.Is(err, errControllerAlreadyRunning):
			return nil, fmt.Errorf("acquiring acknowledged controller ownership: %w", err)
		case !ops.now().Before(deadline):
			return nil, fmt.Errorf("%w after %s", errControllerStopOwnershipWaitTimeout, timeout)
		default:
			ops.retry()
		}
	}
}

func validateAcknowledgedControllerSocket(
	cityPath string,
	result controllerStopResult,
	stat func(string) (os.FileInfo, error),
	requireAbsent bool,
) error {
	if result.outcome != controllerStopAcknowledged || result.err != nil {
		return fmt.Errorf("%w: result is not an error-free acknowledgement", errControllerStopOwnershipUnproven)
	}
	socketPath := controllerSocketPath(cityPath)
	if result.socketPath == "" || result.socketPath != socketPath || result.socketInfo == nil {
		return fmt.Errorf("%w: acknowledgement has no matching socket witness", errControllerStopOwnershipUnproven)
	}
	current, err := stat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stating acknowledged controller socket: %w", err)
	}
	if !os.SameFile(result.socketInfo, current) {
		return fmt.Errorf("%w at %s", errControllerStopSocketReplaced, socketPath)
	}
	if requireAbsent {
		return fmt.Errorf("%w: acknowledged controller socket still exists after lock acquisition", errControllerStopOwnershipUnproven)
	}
	return nil
}
