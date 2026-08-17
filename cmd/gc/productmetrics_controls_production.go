//go:build !productmetrics_testhook

package main

import "github.com/spf13/cobra"

func registerProductMetricsBuildCommands(*cobra.Command) {}
