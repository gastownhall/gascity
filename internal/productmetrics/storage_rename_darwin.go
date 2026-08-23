//go:build darwin && !ios

package productmetrics

import "golang.org/x/sys/unix"

func platformRenameNoReplaceAt(sourceFD int, sourceName string, targetFD int, targetName string) error {
	return unix.RenameatxNp(sourceFD, sourceName, targetFD, targetName, unix.RENAME_EXCL)
}

func platformExchangeAt(sourceFD int, sourceName string, targetFD int, targetName string) error {
	return unix.RenameatxNp(sourceFD, sourceName, targetFD, targetName, unix.RENAME_SWAP)
}
