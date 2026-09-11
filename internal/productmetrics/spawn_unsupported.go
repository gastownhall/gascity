//go:build !((linux && !android) || (darwin && !ios))

package productmetrics

import (
	"fmt"
	"runtime"
)

func platformStartPrivateUploader(privateUploaderProcessSpec) (func() error, error) {
	return nil, fmt.Errorf("%w: detached uploader on %s", errStorageUnsupported, runtime.GOOS)
}

func platformPrivateUploaderSupported() bool { return false }
