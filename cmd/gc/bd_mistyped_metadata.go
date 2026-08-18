package main

import (
	"github.com/gastownhall/gascity/internal/bdflags"
)

// mistypedMetadataPairRefusal reports whether a `gc bd` invocation carries
// `--set-metadata` pairs bd would silently drop, and the message to print.
//
// gc bd writes exec raw bd, so nothing upstream of this sees them. It depends
// only on internal/bdflags — the upstream-owned source of truth for bd's flag
// names — so this guard carries no fork-local dependency.
func mistypedMetadataPairRefusal(bdArgs []string) (string, bool) {
	verb, verbArgs := bdflags.SplitGlobalFlags(bdArgs)
	return bdflags.DroppedMetadataRefusal("gc bd", verb, verbArgs)
}
