package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// diskPreflightFixture swaps doltContainerFreeBytesFunc for the duration of
// a test and restores it on cleanup. Never calls the real syscall.
func diskPreflightFixture(t *testing.T, fakeFree int64, fakeErr error) {
	t.Helper()
	orig := doltContainerFreeBytesFunc
	doltContainerFreeBytesFunc = func(_ string) (int64, error) {
		return fakeFree, fakeErr
	}
	t.Cleanup(func() { doltContainerFreeBytesFunc = orig })
}

func TestDiskPreflightCritical(t *testing.T) {
	const minFree = 500 << 20 // 500 MiB
	diskPreflightFixture(t, minFree-1, nil)

	var stderr bytes.Buffer
	err := checkManagedDoltDiskPreflight("/data/dolt", minFree, 2<<30, &stderr)
	if err == nil {
		t.Fatal("expected non-nil error for critical disk level, got nil")
	}
	msg := err.Error()
	// Error message must name both the actual and floor bytes.
	if !strings.Contains(msg, "refusing to start managed Dolt") {
		t.Errorf("error message missing expected prefix, got: %s", msg)
	}
	if !strings.Contains(msg, "/data/dolt") {
		t.Errorf("error message missing data-dir path, got: %s", msg)
	}
}

func TestDiskPreflightWarn(t *testing.T) {
	const (
		warnFree = 2 << 30   // 2 GiB
		minFree  = 500 << 20 // 500 MiB
	)
	// Free is below warn threshold but above min threshold.
	diskPreflightFixture(t, warnFree-1, nil)

	var stderr bytes.Buffer
	err := checkManagedDoltDiskPreflight("/data/dolt", minFree, warnFree, &stderr)
	if err != nil {
		t.Fatalf("expected nil error for warn level, got: %v", err)
	}
	// A warning should be logged to stderr.
	if !strings.Contains(stderr.String(), "WARN") {
		t.Errorf("expected WARN in stderr output, got: %q", stderr.String())
	}
}

func TestDiskPreflightOK(t *testing.T) {
	const warnFree = 2 << 30 // 2 GiB
	// Free is above warn threshold.
	diskPreflightFixture(t, warnFree+1, nil)

	var stderr bytes.Buffer
	err := checkManagedDoltDiskPreflight("/data/dolt", 500<<20, warnFree, &stderr)
	if err != nil {
		t.Fatalf("expected nil error for healthy disk, got: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output for healthy disk, got: %q", stderr.String())
	}
}

func TestDiskPreflightStatfsError(t *testing.T) {
	probeErr := errors.New("statfs: permission denied")
	diskPreflightFixture(t, -1, probeErr)

	var stderr bytes.Buffer
	err := checkManagedDoltDiskPreflight("/data/dolt", 500<<20, 2<<30, &stderr)
	// Fail-open: probe error must not block startup.
	if err != nil {
		t.Fatalf("expected nil error on probe failure (fail-open), got: %v", err)
	}
	// Probe error should be logged as a warning.
	if !strings.Contains(stderr.String(), "fail-open") {
		t.Errorf("expected fail-open message in stderr, got: %q", stderr.String())
	}
}

func TestDiskPreflightDisabled(t *testing.T) {
	// Even if disk would be critical, minFree=0 disables the check.
	diskPreflightFixture(t, 0, nil) // 0 bytes free — would be critical

	var stderr bytes.Buffer
	err := checkManagedDoltDiskPreflight("/data/dolt", 0 /* disabled */, 2<<30, &stderr)
	if err != nil {
		t.Fatalf("expected nil error when check disabled (minFree=0), got: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output when check disabled, got: %q", stderr.String())
	}
}
