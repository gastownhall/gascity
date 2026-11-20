//go:build !unix

package main

import (
	"errors"
	"os"
)

func rigCloneNativeDirectoryIdentity(os.FileInfo) (string, error) {
	return "", errors.New("native clone directory ownership identity is unsupported on this platform")
}
