//go:build !productmetrics_testhook

package main

import "github.com/gastownhall/gascity/internal/productmetrics"

func configuredPrivateProductMetricsRunner() privateProductMetricsRunFunc {
	return runProductionProductMetricsChild
}

func configuredProductMetricsControlService() (*productmetrics.Service, error) {
	return openProductionProductMetricsService()
}
