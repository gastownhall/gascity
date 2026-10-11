//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func platformStartPrivateUploader(spec privateUploaderProcessSpec) (func() error, error) {
	command, null, err := newPlatformPrivateUploaderCommand(spec)
	if err != nil {
		return nil, err
	}
	return startPrivateUploaderCommand(command.Start, command.Wait, null.Close)
}

func newPlatformPrivateUploaderCommand(spec privateUploaderProcessSpec) (*exec.Cmd, *os.File, error) {
	if spec.executable == "" || len(spec.args) != 2 || len(spec.environment) == 0 || spec.directory != "/" {
		return nil, nil, errors.New("productmetrics: private uploader process spec is incomplete")
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("productmetrics: open null device: %w", err)
	}
	command := exec.Command(spec.executable, spec.args...)
	command.Env = append([]string(nil), spec.environment...)
	command.Dir = spec.directory
	command.Stdin = null
	command.Stdout = null
	command.Stderr = null
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command, null, nil
}

func platformPrivateUploaderSupported() bool { return true }

func startPrivateUploaderCommand(
	start func() error,
	wait func() error,
	closeParentDescriptors func() error,
) (func() error, error) {
	if start == nil || wait == nil || closeParentDescriptors == nil {
		return nil, errors.New("productmetrics: private uploader command dependencies are incomplete")
	}
	if err := start(); err != nil {
		return nil, errors.Join(fmt.Errorf("productmetrics: start detached uploader process: %w", err), closeParentDescriptors())
	}
	if err := closeParentDescriptors(); err != nil {
		// Start succeeded, so exactly one owner must reap the child even though
		// the parent-side descriptor close failed and the caller sees an error.
		go func() { _ = wait() }()
		return nil, fmt.Errorf("productmetrics: close parent null device: %w", err)
	}
	return wait, nil
}
