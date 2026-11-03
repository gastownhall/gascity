package main

import (
	"errors"
	"fmt"
	goruntime "runtime"
)

// errSupervisorServiceManagerUnsupported is the sentinel callers match with
// errors.Is when a platform has no service manager gc installs into. It is a
// distinct outcome from a service manager that is present and refused the
// operation, because only one of the two is worth retrying or reporting as a
// failure.
var errSupervisorServiceManagerUnsupported = errors.New("supervisor service manager unsupported on this platform")

// supervisorServiceManagerUnsupportedError names the platform and the
// operation that has no service manager behind it, and states what to do
// instead.
//
// The alternative is not a downgrade. Without a service manager the supported
// lifecycle is a foreground supervisor with explicit CLI start/stop, and the
// control endpoint's stop command is the authoritative graceful shutdown on
// every platform — a service manager only adds start-on-login and restart
// supervision on top of it.
//
// Gas City must not register a Windows service to close that gap. A service
// runs in a session with no desktop and no user profile, which is exactly what
// a supervisor that launches interactive agent sessions cannot do; installing
// one would produce a supervisor that starts and then cannot do its job.
type supervisorServiceManagerUnsupportedError struct {
	GOOS string
	Op   string
}

func (e *supervisorServiceManagerUnsupportedError) Error() string {
	return fmt.Sprintf(
		"%s is not supported on %s: there is no service manager gc installs into. Run the supervisor in the foreground with 'gc supervisor run', or start and stop it explicitly with 'gc supervisor start' and 'gc supervisor stop'",
		e.Op, e.GOOS)
}

func (e *supervisorServiceManagerUnsupportedError) Is(target error) bool {
	return target == errSupervisorServiceManagerUnsupported
}

// supervisorServiceManagerKind names the service manager gc installs into on
// this platform, or "" when there is none.
func supervisorServiceManagerKind() string {
	switch goruntime.GOOS {
	case "darwin":
		return "launchd"
	case "linux":
		return "systemd"
	default:
		return ""
	}
}

// supervisorServiceManagerAvailable reports whether gc has a service manager
// to install into on this platform.
func supervisorServiceManagerAvailable() bool {
	return supervisorServiceManagerKind() != ""
}

func newSupervisorServiceManagerUnsupportedError(op string) error {
	return &supervisorServiceManagerUnsupportedError{GOOS: goruntime.GOOS, Op: op}
}
