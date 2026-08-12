//go:build !((linux && !android) || (darwin && !ios))

package productmetrics

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/gchome"
)

func TestUnsupportedStorageFailsClosed(t *testing.T) {
	if _, err := openStorageRootReadOnly(gchome.ProductUsageHome{}); !errors.Is(err, errStorageUnsupported) {
		t.Fatalf("read-only open error = %v, want unsupported", err)
	}
	if _, err := openStorageRootMutable(gchome.ProductUsageHome{}); !errors.Is(err, errStorageUnsupported) {
		t.Fatalf("mutable open error = %v, want unsupported", err)
	}
}
