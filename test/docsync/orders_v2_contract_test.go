package docsync

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var ordersV2FixtureMap = map[string]string{
	"broker-capabilities.json":                        "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/capabilities_payload",
	"broker-formulas-list-request-empty-target.json":  "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/formulas_list_request",
	"broker-mutate-result-confirmation-required.json": "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/mutate_result",
	"broker-mutate-result-success.json":               "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/formulas_execute_mutate_result",
	"broker-mutate-result-validation-error.json":      "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/mutate_result",
	"broker-watch-result-workflow.json":               "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/watch_result",
	"broker-workflow-event.json":                      "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/workflow_event_frame",
	"broker-workflow-resync-required.json":            "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/workflow_resync_required",
	"broker-workflow-watch-ready.json":                "https://contracts.gastownhall/orders-v2/broker.schema.json#/$defs/workflow_watch_ready",
	"gc-http-formulas-detail.json":                    "https://contracts.gastownhall/orders-v2/gc-http.schema.json#/$defs/formulas_detail_response",
	"gc-http-formulas-execute-request.json":           "https://contracts.gastownhall/orders-v2/gc-http.schema.json#/$defs/formulas_execute_request",
	"gc-http-formulas-execute-response.json":          "https://contracts.gastownhall/orders-v2/gc-http.schema.json#/$defs/formulas_execute_response",
	"gc-http-formulas-list.json":                      "https://contracts.gastownhall/orders-v2/gc-http.schema.json#/$defs/formulas_list_response",
	"gc-http-orders-feed.json":                        "https://contracts.gastownhall/orders-v2/gc-http.schema.json#/$defs/orders_feed_response",
	"gc-http-workflow-get.json":                       "https://contracts.gastownhall/orders-v2/gc-http.schema.json#/$defs/workflow_get_response",
}

var ordersV2TypeExports = []string{
	"OrdersFeedRequest",
	"FormulasListRequest",
	"FormulasDetailRequest",
	"WorkflowGetRequest",
	"WatchResource",
	"BrokerErrorPayload",
	"WorkflowEventFrame",
	"WatchResult",
	"WorkflowWatchReady",
	"WorkflowResyncRequired",
	"OrdersFeedRefresh",
	"MutateResult",
	"FormulasExecuteMutateResult",
	"OrdersLaunchMutateResult",
	"OrdersLaunchRequest",
	"OrdersLaunchResponse",
}

var ordersV2TypeSnippets = []string{
	`type: "workflow:event";`,
	`type: "watch:result";`,
	`type: "workflow:watch_ready";`,
	`type: "workflow:resync_required";`,
	"confirmToken?: string;",
	"errorPayload?: BrokerErrorPayload;",
}

func TestOrdersV2Contract(t *testing.T) {
	root := repoRoot()
	contractDir := filepath.Join(root, "contracts", "orders-v2")
	schemaDir := filepath.Join(contractDir, "schemas")
	fixtureDir := filepath.Join(contractDir, "fixtures")
	typesPath := filepath.Join(contractDir, "types", "orders-v2.ts")

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)

	schemaFiles := []string{
		"common.schema.json",
		"gc-http.schema.json",
		"broker.schema.json",
	}
	assertOrdersV2DirEntries(t, schemaDir, schemaFiles)
	assertOrdersV2DirEntries(t, fixtureDir, sortedOrdersV2Fixtures())

	for _, schemaFile := range schemaFiles {
		schemaPath := filepath.Join(schemaDir, schemaFile)
		schemaDoc, ok := loadOrdersV2JSON(t, schemaPath).(map[string]any)
		if !ok {
			t.Fatalf("%s did not decode to a JSON object", schemaPath)
		}
		schemaID, ok := schemaDoc["$id"].(string)
		if !ok || schemaID == "" {
			t.Fatalf("%s is missing $id", schemaPath)
		}
		if err := compiler.AddResource(schemaID, schemaDoc); err != nil {
			t.Fatalf("add schema %s: %v", schemaFile, err)
		}
	}

	for fixtureFile, schemaID := range ordersV2FixtureMap {
		schema, err := compiler.Compile(schemaID)
		if err != nil {
			t.Fatalf("compile schema for %s: %v", fixtureFile, err)
		}
		fixturePath := filepath.Join(fixtureDir, fixtureFile)
		fixtureDoc := loadOrdersV2JSON(t, fixturePath)
		if err := schema.Validate(fixtureDoc); err != nil {
			t.Fatalf("%s failed %s: %v", fixtureFile, schemaID, err)
		}
	}

	typesBytes, err := os.ReadFile(typesPath)
	if err != nil {
		t.Fatalf("read %s: %v", typesPath, err)
	}
	types := string(typesBytes)
	if !strings.Contains(types, "Hand-maintained TypeScript mirrors") {
		t.Fatalf("%s is missing the maintenance banner", typesPath)
	}

	for _, exportName := range ordersV2TypeExports {
		exportPattern := regexp.MustCompile(`export\s+(?:interface|type)\s+` + regexp.QuoteMeta(exportName) + `\b`)
		if !exportPattern.MatchString(types) {
			t.Fatalf("%s is missing exported type %s", typesPath, exportName)
		}
	}

	for _, snippet := range ordersV2TypeSnippets {
		if !strings.Contains(types, snippet) {
			t.Fatalf("%s is missing required snippet %q", typesPath, snippet)
		}
	}
}

func loadOrdersV2JSON(t *testing.T, path string) any {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var doc any
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return doc
}

func assertOrdersV2DirEntries(t *testing.T, dir string, expected []string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}

	var actual []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		actual = append(actual, entry.Name())
	}
	slices.Sort(actual)

	expectedCopy := append([]string(nil), expected...)
	slices.Sort(expectedCopy)

	if !slices.Equal(actual, expectedCopy) {
		t.Fatalf("%s drift: expected %v, found %v", dir, expectedCopy, actual)
	}
}

func sortedOrdersV2Fixtures() []string {
	fixtures := make([]string, 0, len(ordersV2FixtureMap))
	for name := range ordersV2FixtureMap {
		fixtures = append(fixtures, name)
	}
	slices.Sort(fixtures)
	return fixtures
}
