//go:build !windows

package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func configureProcessTableScan(runtime.Provider, *config.City) {}

// processScanState is empty off Windows, where the process-table scan is not
// scoped. The no-op refresh is declared on it so CityRuntime's promoted call
// keeps the embedded field in use.
type processScanState struct{}

func (*processScanState) refreshProcessTableScan(runtime.Provider, *config.City) {}
