package main

import (
	"errors"
	"strings"
	"testing"
)

// TestListenerChildFailureCarriesReadinessExitAndStderr pins the one formatter
// every python listener fixture reports a failed start through (ga-os7kifr
// AC4/AC6): the subject, the readiness error, the child's exit status and its
// stderr, with both errors still reachable through errors.Is.
func TestListenerChildFailureCarriesReadinessExitAndStderr(t *testing.T) {
	errReady := errors.New("never dialed")
	errExit := errors.New("exit status 1")
	err := listenerChildFailure("listener process on 4242", errReady, errExit, []byte("OSError: [Errno 98] Address already in use\n"))
	want := "listener process on 4242 did not become ready: never dialed (child exit: exit status 1); child stderr:\nOSError: [Errno 98] Address already in use\n"
	if err == nil || err.Error() != want {
		t.Fatalf("listenerChildFailure() = %v, want %q", err, want)
	}
	if !errors.Is(err, errReady) {
		t.Errorf("error does not wrap the readiness error: %v", err)
	}
	if !errors.Is(err, errExit) {
		t.Errorf("error does not wrap the child exit error: %v", err)
	}
}

// TestListenerChildFailureKeepsAnEmptyStderrVisible pins that a child that said
// nothing still yields a well-formed report rather than a dangling prefix.
func TestListenerChildFailureKeepsAnEmptyStderrVisible(t *testing.T) {
	err := listenerChildFailure("unix socket process", errors.New("timed out"), errors.New("signal: killed"), nil)
	if err == nil {
		t.Fatal("listenerChildFailure() = nil, want an error")
	}
	for _, want := range []string{"unix socket process did not become ready", "timed out", "child exit: signal: killed", "child stderr:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}
