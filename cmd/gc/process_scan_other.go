//go:build !windows

package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func configureProcessTableScan(runtime.Provider, *config.City) {}

// processScanState is empty off Windows, where the process-table scan is not
// scoped.
type processScanState struct{}

// refreshProcessTableScan does nothing off Windows.
func (*processScanState) refreshProcessTableScan(runtime.Provider, *config.City, *config.City) {}
