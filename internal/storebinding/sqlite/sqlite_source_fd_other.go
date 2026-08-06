//go:build !linux

package sqlite

func ensureNoSQLiteSourceDescriptors(string) error { return nil }
