//go:build !unix && !windows

package events

import "os"

// lockFile is a no-op on platforms without a native advisory file lock
// implementation. On such platforms cross-process coordination of the
// seq counter is NOT guaranteed — only in-process serialization via
// FileRecorder.mu is provided. Keep this fallback in mind when
// deploying Gas City to an unsupported target.
func lockFile(f *os.File) error { return nil }

// unlockFile is a no-op companion to lockFile on unsupported platforms.
func unlockFile(f *os.File) error { return nil }
