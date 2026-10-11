//go:build !productmetrics_testhook

package main

import "github.com/spf13/cobra"

func ignoreProductMetricsCensusCommand(*cobra.Command) bool { return false }
