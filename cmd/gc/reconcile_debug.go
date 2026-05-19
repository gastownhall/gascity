package main

import (
	"fmt"
	"io"
	"os"
)

func reconcileDebugf(w io.Writer, format string, args ...interface{}) {
	if os.Getenv("GC_RECONCILE_DEBUG") == "" {
		return
	}
	fmt.Fprintf(w, format, args...) //nolint:errcheck
}
