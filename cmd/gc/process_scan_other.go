//go:build !windows

package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func configureProcessTableScan(runtime.Provider, *config.City) {}

func (*CityRuntime) refreshProcessTableScan(runtime.Provider, *config.City) {}
