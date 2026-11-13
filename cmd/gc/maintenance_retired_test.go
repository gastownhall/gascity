package main

import (
	"bytes"
	"testing"
)

// TestGCMaintenanceCommandIsRetired pins that the `gc maintenance` command tree
// (status, dolt-gc) is gone with the supervisor loop it fronted.
func TestGCMaintenanceCommandIsRetired(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := newRootCmd(&buf, &buf)
	for _, c := range root.Commands() {
		if c.Name() == "maintenance" {
			t.Fatalf("root command still registers %q; the legacy Dolt maintenance CLI must be retired", c.Name())
		}
	}
}
