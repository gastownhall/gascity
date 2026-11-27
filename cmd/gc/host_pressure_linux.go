//go:build linux

package main

// hostPressureSupported is whether the platform exposes CPU PSI; a var so a
// test can read the gate as another platform does.
var hostPressureSupported = true
