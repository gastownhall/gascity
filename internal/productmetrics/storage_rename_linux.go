//go:build linux && !android

package productmetrics

import "golang.org/x/sys/unix"

func platformRenameNoReplaceAt(sourceFD int, sourceName string, targetFD int, targetName string) error {
	return unix.Renameat2(sourceFD, sourceName, targetFD, targetName, unix.RENAME_NOREPLACE)
}

func platformExchangeAt(sourceFD int, sourceName string, targetFD int, targetName string) error {
	return unix.Renameat2(sourceFD, sourceName, targetFD, targetName, unix.RENAME_EXCHANGE)
}
