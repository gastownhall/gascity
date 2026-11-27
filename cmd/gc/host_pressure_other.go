//go:build !linux

package main

// hostPressureSupported is false off Linux, which exposes no PSI: the CPU
// gate never holds a start there.
var hostPressureSupported = false
