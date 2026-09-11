package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/productmetrics"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestProductMetricsJSONSchemasEnforceClosedDomains(t *testing.T) {
	statusPayload, err := json.Marshal(productMetricsStatusForJSON(productMetricsStatus{
		State:                      productmetrics.StateEnabled,
		Reason:                     productmetrics.ReasonEnabled,
		HomeStable:                 true,
		StateSchema:                1,
		RequiredNoticeVersion:      1,
		AcceptedNoticeVersion:      1,
		QueueDiagnosticsAvailable:  true,
		StatusDiagnosticsAvailable: true,
		LastUploadAttemptHourUTC:   "2026-07-12T20:00:00Z",
		LastUploadSuccessHourUTC:   "2026-07-12T19:00:00Z",
		LastErrorClass:             productmetrics.DiagnosticErrorServer5xx,
	}, productMetricsPolicyMetadata{
		EndpointHostname:         "metrics.gascity.example",
		PrivacyURL:               "https://gascity.example/privacy/command-usage",
		EdgeLogRetentionDays:     7,
		RawEventRetentionDays:    90,
		AggregateRetentionMonths: 13,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProductMetricsJSONSchemaE([]string{"metrics", "status"}, statusPayload); err != nil {
		t.Fatalf("valid metrics status rejected: %v\n%s", err, statusPayload)
	}
	statusMutations := []struct {
		name string
		old  string
		new  string
	}{
		{name: "reason", old: `"reason":"enabled"`, new: `"reason":"private-path"`},
		{name: "upload hour", old: `"last_upload_attempt_hour_utc":"2026-07-12T20:00:00Z"`, new: `"last_upload_attempt_hour_utc":"2026-07-12T20:15:00Z"`},
		{name: "error class", old: `"last_error_class":"server-5xx"`, new: `"last_error_class":"private-path"`},
		{name: "retention", old: `"edge_log_days":7`, new: `"edge_log_days":8`},
		{name: "privacy URL", old: `"privacy_url":"https://gascity.example/privacy/command-usage"`, new: `"privacy_url":"not a URI"`},
		{name: "independence", old: `"independence":"` + productMetricsIndependenceText + `"`, new: `"independence":"coupled"`},
	}
	for _, mutation := range statusMutations {
		t.Run("status "+mutation.name, func(t *testing.T) {
			assertProductMetricsSchemaRejectsMutation(t, []string{"metrics", "status"}, statusPayload, mutation.old, mutation.new)
		})
	}

	examplePayload, err := productmetrics.EncodeBatch(productmetrics.ExampleBatch())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProductMetricsJSONSchemaE([]string{"metrics", "example"}, examplePayload); err != nil {
		t.Fatalf("valid metrics example rejected: %v\n%s", err, examplePayload)
	}
	exampleMutations := []struct {
		name string
		old  string
		new  string
	}{
		{name: "UUID version", old: `"event_id":"8c4f4128-a6e8-4f66-bd1b-1fcf1298b124"`, new: `"event_id":"8c4f4128-a6e8-5f66-bd1b-1fcf1298b124"`},
		{name: "release version", old: `"release_version":"0.31.0"`, new: `"release_version":"v0.31.0"`},
		{name: "occurred hour", old: `"occurred_hour_utc":"2026-07-11T00:00:00Z"`, new: `"occurred_hour_utc":"2026-07-11T00:30:00Z"`},
		{name: "command ID", old: `"command_id":"help"`, new: `"command_id":"private-command"`},
	}
	for _, mutation := range exampleMutations {
		t.Run("example "+mutation.name, func(t *testing.T) {
			assertProductMetricsSchemaRejectsMutation(t, []string{"metrics", "example"}, examplePayload, mutation.old, mutation.new)
		})
	}
}

func assertProductMetricsSchemaRejectsMutation(t *testing.T, command []string, valid []byte, old, replacement string) {
	t.Helper()
	mutated := bytes.Replace(valid, []byte(old), []byte(replacement), 1)
	if bytes.Equal(mutated, valid) {
		t.Fatalf("schema mutation did not match %q in %s", old, valid)
	}
	if err := validateProductMetricsJSONSchemaE(command, mutated); err == nil {
		t.Fatalf("schema for %v accepted mutation %q -> %q:\n%s", command, old, replacement, mutated)
	}
}

func validateProductMetricsJSONSchemaE(command []string, data []byte) error {
	rawSchema, err := readBuiltinSchema(command, jsonSchemaResultRole)
	if err != nil {
		return err
	}
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	schemaURL := strings.Join(command, "/") + "/result.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
		return err
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return err
	}
	return compiled.Validate(instance)
}
