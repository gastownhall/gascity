//go:build !linux && !darwin

package proctable

import "github.com/gastownhall/gascity/internal/runtime"

// ScanBySessionIDOutsideTmux has no tmux fence to apply on these platforms
// (windows has no tmux pane process tree this scan separates; the rest have no
// scan at all), so it returns ScanBySessionID's answer: the live scan on
// windows, an error elsewhere.
func ScanBySessionIDOutsideTmux(id string) ([]runtime.LiveRuntime, error) {
	return ScanBySessionID(id)
}
