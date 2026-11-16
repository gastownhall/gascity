package main

import "errors"

// errSignalUnsupported marks a platformKill signal the platform cannot
// deliver; Windows delivers only SIGKILL.
var errSignalUnsupported = errors.New("signal unsupported on this platform")
