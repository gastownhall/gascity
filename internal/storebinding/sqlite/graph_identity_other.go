//go:build !unix

package sqlite

import (
	"fmt"
	"os"
)

func platformFileIdentity(info os.FileInfo) string {
	return fmt.Sprintf("mode=%v;size=%d;mtime=%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
}

func platformFileHasMultipleLinks(os.FileInfo) bool { return false }
