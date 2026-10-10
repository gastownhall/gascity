//go:build !linux && !darwin

package session

// readBootID knows no boot ID here, so no runtime lease takeover is immediate.
func readBootID() string { return "" }
