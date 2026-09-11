//go:build !((linux && !android) || (darwin && !ios))

package productmetrics

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/gastownhall/gascity/internal/gchome"
)

var errStorageUnsupported = errors.New("productmetrics: durable storage is unsupported on this platform")

func platformOpenStorageRoot(_ gchome.ProductUsageHome, _ bool, _ storageTestHooks) (storageDirectoryBackend, error) {
	return nil, fmt.Errorf("%w: %s", errStorageUnsupported, runtime.GOOS)
}
