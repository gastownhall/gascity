//go:build productmetrics_testhook

package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestProductMetricsTestOnlyCensusEscapeIsNarrow(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	metrics := &cobra.Command{Use: "metrics"}
	testOnly := newProductMetricsTesthookRecordHelpCommand()
	metrics.AddCommand(testOnly)
	root.AddCommand(metrics)
	if !ignoreProductMetricsCensusCommand(testOnly) {
		t.Fatal("reviewed test-only command was not recognized")
	}

	for name, mutate := range map[string]func(*cobra.Command){
		"visible":            func(command *cobra.Command) { command.Hidden = false },
		"wrong name":         func(command *cobra.Command) { command.Use = "other" },
		"missing annotation": func(command *cobra.Command) { command.Annotations = nil },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *testOnly
			copy.Annotations = cloneCommandAnnotations(testOnly.Annotations)
			mutate(&copy)
			if ignoreProductMetricsCensusCommand(&copy) {
				t.Fatal("malformed test-only command was accepted")
			}
		})
	}
}
