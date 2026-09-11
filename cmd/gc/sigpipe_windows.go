//go:build windows

package main

// ignoreSIGPIPE is a no-op on Windows, which has no SIGPIPE: a write to a
// closed handle already returns an error rather than terminating the process,
// which is the behavior ignoreSIGPIPE buys on Unix.
func ignoreSIGPIPE() {}
