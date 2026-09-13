//go:build !linux

package sqlite

// sqliteSourceDescriptorDetectionSupported reports whether this platform can
// observe descriptors another part of the process already holds on a source.
// The check reads /proc, so only Linux can answer it; elsewhere
// ensureNoSQLiteSourceDescriptors is a no-op and callers must not expect a
// refusal. Mirrors sqliteWriterFencingSupported.
func sqliteSourceDescriptorDetectionSupported() bool { return false }

func ensureNoSQLiteSourceDescriptors(string) error { return nil }
