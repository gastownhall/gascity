//go:build !unix

package testenv

import "os"

// checkTemplateEntry accepts every entry: file ownership and permission bits
// do not describe who can write a file on these platforms.
func checkTemplateEntry(string, os.FileInfo, int) error { return nil }
