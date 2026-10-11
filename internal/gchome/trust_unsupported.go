//go:build !((linux && !android) || (darwin && !ios))

package gchome

import (
	"fmt"
	"runtime"
)

func inspectTrustedProductUsagePath(_, _ string) (bool, error) {
	return false, fmt.Errorf("gchome: product-usage trust inspection is unsupported on %s", runtime.GOOS)
}
