//go:build productmetrics_testhook

package main

import (
	"errors"

	"github.com/gastownhall/gascity/internal/productmetrics"
	"github.com/spf13/cobra"
)

const productMetricsTesthookRecordHelpCommand = "__testhook-record-help"

const (
	productMetricsCensusAnnotation    = "gc.productmetrics.census"
	productMetricsCensusTestOnlyValue = "test-only"
)

func ignoreProductMetricsCensusCommand(command *cobra.Command) bool {
	if command == nil || command.Annotations[productMetricsCensusAnnotation] != productMetricsCensusTestOnlyValue ||
		!command.Hidden || !command.Runnable() || command.HasSubCommands() || len(command.Aliases) != 0 || command.Name() != productMetricsTesthookRecordHelpCommand {
		return false
	}
	parent := command.Parent()
	return parent != nil && parent.Name() == "metrics" && parent.Parent() != nil && parent.Parent().Name() == "gc"
}

func registerProductMetricsBuildCommands(metrics *cobra.Command) {
	if metrics == nil {
		return
	}
	metrics.AddCommand(newProductMetricsTesthookRecordHelpCommand())
}

func newProductMetricsTesthookRecordHelpCommand() *cobra.Command {
	return &cobra.Command{
		Use:    productMetricsTesthookRecordHelpCommand,
		Hidden: true,
		Annotations: map[string]string{
			productMetricsCensusAnnotation: productMetricsCensusTestOnlyValue,
		},
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) (returnErr error) {
			if productMetricsControlServiceFactory == nil {
				return errors.New("product metrics test recorder is unavailable")
			}
			service, err := productMetricsControlServiceFactory()
			if err != nil {
				return errors.New("product metrics test recorder could not open the control service")
			}
			if service == nil {
				return errors.New("product metrics test recorder opened no control service")
			}
			permit := service.RecordingPermit(productmetrics.InvocationContext{Recordable: true})
			defer func() { returnErr = errors.Join(returnErr, permit.Close()) }()
			if result := service.RecordOnce(permit, productmetrics.CommandHelp); result != productmetrics.RecordStored {
				return errors.New("product metrics test recorder did not store an event")
			}
			return nil
		},
	}
}
