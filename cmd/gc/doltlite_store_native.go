//go:build cgo && gascity_native_beads

package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

const nativeDoltliteBeadsEnv = "GC_NATIVE_DOLTLITE_BEADS"

func openOptimizedDoltliteStore(storePath string, store *beads.BdStore) (beads.Store, bool) {
	if !nativeDoltliteBeadsEnabled() {
		return nil, false
	}
	direct, err := beads.NewDoltliteReadStore(storePath, store)
	if err == nil {
		return direct, true
	}
	return nil, false
}

func nativeDoltliteBeadsEnabled() bool {
	raw := strings.TrimSpace(os.Getenv(nativeDoltliteBeadsEnv))
	if raw == "" {
		return false
	}
	enabled, err := strconv.ParseBool(raw)
	return err == nil && enabled
}
