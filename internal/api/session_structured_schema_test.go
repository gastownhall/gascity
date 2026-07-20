package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/worker"
)

func TestSessionTranscriptRuntimeContainerDoesNotCustomizeJSON(t *testing.T) {
	if _, ok := reflect.TypeOf(sessionTranscriptGetResponse{}).MethodByName("MarshalJSON"); ok {
		t.Fatal("sessionTranscriptGetResponse defines MarshalJSON; typed control-plane wire types must use ordinary struct fields")
	}

	structured, err := json.Marshal(sessionTranscriptGetResponse{
		Format:             "structured",
		StructuredMessages: structuredMessagesField(nil),
	})
	if err != nil {
		t.Fatalf("marshal structured response: %v", err)
	}
	if !strings.Contains(string(structured), `"structured_messages":[]`) {
		t.Fatalf("structured response = %s, want required empty structured_messages array", structured)
	}

	raw, err := json.Marshal(sessionTranscriptGetResponse{Format: "raw"})
	if err != nil {
		t.Fatalf("marshal raw response: %v", err)
	}
	if strings.Contains(string(raw), `"structured_messages"`) {
		t.Fatalf("raw response = %s, want structured_messages omitted", raw)
	}
}

func TestLiveStructuredTranscriptSchemaPublishesNamedDiscriminatedUnions(t *testing.T) {
	schemas := componentSchemas(t, readLiveSupervisorOpenAPISpec(t))

	for _, tc := range []struct {
		name          string
		discriminator string
		variants      map[string]string
	}{
		{
			name:          "SessionStructuredMessage",
			discriminator: "role",
			variants: map[string]string{
				"unknown":   "SessionStructuredMessageUnknown",
				"user":      "SessionStructuredMessageUser",
				"assistant": "SessionStructuredMessageAssistant",
				"system":    "SessionStructuredMessageSystem",
				"tool":      "SessionStructuredMessageTool",
			},
		},
		{
			name:          "SessionStructuredBlock",
			discriminator: "type",
			variants: map[string]string{
				"text":        "SessionStructuredBlockText",
				"thinking":    "SessionStructuredBlockThinking",
				"tool_use":    "SessionStructuredBlockToolUse",
				"tool_result": "SessionStructuredBlockToolResult",
				"interaction": "SessionStructuredBlockInteraction",
				"image":       "SessionStructuredBlockImage",
				"unknown":     "SessionStructuredBlockUnknown",
			},
		},
		{
			name:          "SessionStructuredToolInput",
			discriminator: "kind",
			variants: map[string]string{
				"unknown":   "SessionStructuredToolInputUnknown",
				"command":   "SessionStructuredToolInputCommand",
				"stdin":     "SessionStructuredToolInputStdin",
				"code":      "SessionStructuredToolInputCode",
				"patch":     "SessionStructuredToolInputPatch",
				"write":     "SessionStructuredToolInputWrite",
				"glob":      "SessionStructuredToolInputGlob",
				"fetch":     "SessionStructuredToolInputFetch",
				"search":    "SessionStructuredToolInputSearch",
				"file":      "SessionStructuredToolInputFile",
				"todo":      "SessionStructuredToolInputTodo",
				"plan":      "SessionStructuredToolInputPlan",
				"question":  "SessionStructuredToolInputQuestion",
				"task":      "SessionStructuredToolInputTask",
				"text":      "SessionStructuredToolInputText",
				"arguments": "SessionStructuredToolInputArguments",
			},
		},
		{
			name:          "SessionStructuredToolResult",
			discriminator: "kind",
			variants: map[string]string{
				"unknown":  "SessionStructuredToolResultUnknown",
				"bash":     "SessionStructuredToolResultBash",
				"python":   "SessionStructuredToolResultPython",
				"read":     "SessionStructuredToolResultRead",
				"glob":     "SessionStructuredToolResultGlob",
				"grep":     "SessionStructuredToolResultGrep",
				"search":   "SessionStructuredToolResultSearch",
				"fetch":    "SessionStructuredToolResultFetch",
				"todo":     "SessionStructuredToolResultTodo",
				"plan":     "SessionStructuredToolResultPlan",
				"question": "SessionStructuredToolResultQuestion",
				"stdin":    "SessionStructuredToolResultStdin",
				"task":     "SessionStructuredToolResultTask",
				"write":    "SessionStructuredToolResultWrite",
				"edit":     "SessionStructuredToolResultEdit",
				"text":     "SessionStructuredToolResultText",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertStructuredDiscriminatedUnion(t, schemas, tc.name, tc.discriminator, tc.variants)
		})
	}
}

func TestLiveStructuredTranscriptSchemaRequiresNonNullHistoryMessagesAndBlocks(t *testing.T) {
	schemas := componentSchemas(t, readLiveSupervisorOpenAPISpec(t))

	for _, schemaName := range []string{
		"SessionStreamStructuredMessageEvent",
		"SessionTranscriptStructuredResponse",
	} {
		schema, ok := schemas[schemaName]
		if !ok {
			t.Fatalf("components.schemas missing %s", schemaName)
		}
		assertRequiredFields(t, schemaName, "structured", schema, []string{
			"format", "schema_version", "history", "structured_messages",
		})
		properties := structuredSchemaProperties(t, schemaName, schema)
		assertSchemaLiteral(t, schemaName+".format", properties["format"], "structured")
		assertSchemaLiteral(t, schemaName+".schema_version", properties["schema_version"], sessionStructuredSchemaVersion)
		assertNonNullableRef(t, schemaName+".history", properties["history"], "#/components/schemas/SessionStructuredHistory")
		assertNonNullableArrayRef(t, schemaName+".structured_messages", properties["structured_messages"], "#/components/schemas/SessionStructuredMessage")
	}

	messageUnion := schemas["SessionStructuredMessage"]
	discriminator := structuredDiscriminatorMapping(t, "SessionStructuredMessage", messageUnion, "role")
	for role, ref := range discriminator {
		variant := schemaByRef(t, schemas, ref)
		assertRequiredFields(t, "SessionStructuredMessage", role, variant, []string{"id", "role", "status", "blocks"})
		properties := structuredSchemaProperties(t, ref, variant)
		status, ok := properties["status"].(map[string]any)
		if !ok {
			t.Fatalf("%s.status schema = %#v, want object", ref, properties["status"])
		}
		if got := status["enum"]; !reflect.DeepEqual(got, []any{"unknown", "final", "partial", "superseded"}) {
			t.Fatalf("%s.status enum = %#v, want closed result-status vocabulary", ref, got)
		}
		assertNonNullableArrayRef(t, ref+".blocks", properties["blocks"], "#/components/schemas/SessionStructuredBlock")
		for _, excluded := range []string{"is_subagent", "parent_tool_call_id"} {
			if _, ok := properties[excluded]; ok {
				t.Fatalf("%s exposes out-of-scope v1 field %q", ref, excluded)
			}
		}
	}
}

func TestLiveStructuredTranscriptSchemaClosesToolErrorCategory(t *testing.T) {
	schemas := componentSchemas(t, readLiveSupervisorOpenAPISpec(t))
	schema, ok := schemas["SessionStructuredToolError"]
	if !ok {
		t.Fatal("components.schemas missing SessionStructuredToolError")
	}
	assertRequiredFields(t, "SessionStructuredToolError", "error", schema, []string{"category"})
	properties := structuredSchemaProperties(t, "SessionStructuredToolError", schema)
	category, ok := properties["category"].(map[string]any)
	if !ok {
		t.Fatalf("SessionStructuredToolError.category schema = %#v, want object", properties["category"])
	}
	want := []any{
		"user_rejection",
		"user_rejection_with_reason",
		"command_failure",
		"file_error",
		"validation_error",
		"timeout",
		"network_error",
		"unknown",
	}
	if got := category["enum"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("SessionStructuredToolError.category enum = %#v, want %#v", got, want)
	}
}

func TestLiveStructuredTranscriptSchemaPinsRESTToSnapshotOperation(t *testing.T) {
	schemas := componentSchemas(t, readLiveSupervisorOpenAPISpec(t))
	schema, ok := schemas["SessionTranscriptStructuredResponse"]
	if !ok {
		t.Fatal("components.schemas missing SessionTranscriptStructuredResponse")
	}

	assertRequiredFields(t, "SessionTranscriptStructuredResponse", "structured", schema, []string{"operation"})
	properties := structuredSchemaProperties(t, "SessionTranscriptStructuredResponse", schema)
	assertSchemaLiteral(t, "SessionTranscriptStructuredResponse.operation", properties["operation"], sessionStructuredOperationSnapshot)
	if _, ok := properties["reset_reason"]; ok {
		t.Fatal("SessionTranscriptStructuredResponse exposes reset_reason; REST structured transcripts are always snapshots")
	}
}

func TestLiveStructuredStreamSchemaDocumentsResetReasonCondition(t *testing.T) {
	schemas := componentSchemas(t, readLiveSupervisorOpenAPISpec(t))
	schema, ok := schemas["SessionStreamStructuredMessageEvent"]
	if !ok {
		t.Fatal("components.schemas missing SessionStreamStructuredMessageEvent")
	}

	assertRequiredFields(t, "SessionStreamStructuredMessageEvent", "structured", schema, []string{"operation"})
	properties := structuredSchemaProperties(t, "SessionStreamStructuredMessageEvent", schema)
	operation, ok := properties["operation"].(map[string]any)
	if !ok {
		t.Fatalf("SessionStreamStructuredMessageEvent.operation schema = %#v, want object", properties["operation"])
	}
	if got := operation["enum"]; !reflect.DeepEqual(got, []any{
		sessionStructuredOperationSnapshot,
		sessionStructuredOperationUpsert,
		sessionStructuredOperationReset,
	}) {
		t.Fatalf("SessionStreamStructuredMessageEvent.operation enum = %#v, want closed operation vocabulary", got)
	}
	resetReason, ok := properties["reset_reason"].(map[string]any)
	if !ok {
		t.Fatalf("SessionStreamStructuredMessageEvent.reset_reason schema = %#v, want object", properties["reset_reason"])
	}
	description, _ := resetReason["description"].(string)
	if !strings.Contains(description, "Present if and only if operation is reset") {
		t.Fatalf("SessionStreamStructuredMessageEvent.reset_reason description = %q, want conditional presence contract", description)
	}
}

func TestLiveStructuredTranscriptSchemaVariantsExcludeImpossibleFields(t *testing.T) {
	schemas := componentSchemas(t, readLiveSupervisorOpenAPISpec(t))

	for _, tc := range []struct {
		name    string
		present []string
		absent  []string
	}{
		{name: "SessionStructuredMessageUser", present: []string{"user_prompt"}, absent: []string{"model", "usage", "system_event"}},
		{name: "SessionStructuredMessageAssistant", present: []string{"model", "usage"}, absent: []string{"user_prompt", "system_event"}},
		{name: "SessionStructuredBlockText", present: []string{"text"}, absent: []string{"input", "structured", "interaction"}},
		{name: "SessionStructuredBlockToolResult", present: []string{"structured", "tool_call_id"}, absent: []string{"input", "thinking", "image_url"}},
		{name: "SessionStructuredToolInputCommand", present: []string{"command"}, absent: []string{"code", "patch", "query", "question"}},
		{name: "SessionStructuredToolInputQuestion", present: []string{"question", "options"}, absent: []string{"command", "code", "patch"}},
		{name: "SessionStructuredToolResultRead", present: []string{"file_path", "content"}, absent: []string{"stdout", "patch", "questions"}},
		{name: "SessionStructuredToolResultBash", present: []string{"stdout", "exit_code"}, absent: []string{"patch", "questions", "result_items"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema, ok := schemas[tc.name]
			if !ok {
				t.Fatalf("components.schemas missing %s", tc.name)
			}
			properties := structuredSchemaProperties(t, tc.name, schema)
			for _, field := range tc.present {
				if _, ok := properties[field]; !ok {
					t.Errorf("%s missing relevant field %q", tc.name, field)
				}
			}
			for _, field := range tc.absent {
				if _, ok := properties[field]; ok {
					t.Errorf("%s exposes impossible cross-kind field %q", tc.name, field)
				}
			}
		})
	}
}

func TestStructuredProjectionAllocatesRequiredEmptyArraysAndClosesDiscriminators(t *testing.T) {
	messages, ids := historySnapshotStructuredMessages(nil, false)
	if messages == nil || ids == nil {
		t.Fatalf("nil snapshot = messages %#v ids %#v, want allocated empty arrays", messages, ids)
	}

	messages, _ = historySnapshotStructuredMessages(&worker.HistorySnapshot{
		Entries: []worker.HistoryEntry{{
			ID:     "entry-1",
			Actor:  worker.Actor("provider-special-role"),
			Status: worker.ResultStatus("provider-special-status"),
		}},
	}, false)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one message", messages)
	}
	if messages[0].Role != string(worker.ActorUnknown) {
		t.Fatalf("message role = %q, want closed fallback %q", messages[0].Role, worker.ActorUnknown)
	}
	if messages[0].Status != string(worker.ResultStatusUnknown) {
		t.Fatalf("message status = %q, want closed fallback %q", messages[0].Status, worker.ResultStatusUnknown)
	}
	if messages[0].Blocks == nil {
		t.Fatal("message blocks = nil, want allocated empty array")
	}

	input := sessionStructuredToolInputFromWorker(&worker.StructuredToolInput{Kind: "provider-special-input"})
	if input == nil || input.Kind != "unknown" {
		t.Fatalf("input = %#v, want unknown-kind projection", input)
	}
	result := sessionStructuredToolResultFromWorker(&worker.StructuredToolResult{Kind: "provider-special-result"})
	if result == nil || result.Kind != "unknown" {
		t.Fatalf("result = %#v, want unknown-kind projection", result)
	}
	block := historyBlockToStructuredBlock(worker.HistoryBlock{Kind: worker.BlockKind("provider-special-block")}, false)
	if block == nil || block.Type != string(worker.BlockKindUnknown) {
		t.Fatalf("block = %#v, want unknown-type projection", block)
	}

	wire, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if string(wire) == "" || !jsonHasAllocatedArray(t, wire, "blocks") {
		t.Fatalf("message wire = %s, want blocks:[]", wire)
	}

	fallback := structuredFallbackMessages("session-1", "pane", "partial pane output")
	if len(fallback) != 1 || fallback[0].Status != string(worker.ResultStatusPartial) {
		t.Fatalf("fallback messages = %#v, want one partial result", fallback)
	}
}

func TestStructuredProjectionDropsImpossibleCrossVariantFields(t *testing.T) {
	user := historyEntryToStructuredMessage(worker.HistoryEntry{
		ID:          "user-1",
		Actor:       worker.ActorUser,
		Status:      worker.ResultStatusFinal,
		Model:       "wrong-role-model",
		Usage:       &worker.HistoryUsage{InputTokens: 1},
		UserPrompt:  &worker.HistoryUserPrompt{Text: "hello"},
		SystemEvent: &worker.HistorySystemEvent{Kind: "wrong-role-event"},
	}, false)
	if user.UserPrompt == nil {
		t.Fatal("user prompt missing from user variant")
	}
	if user.Model != "" || user.Usage != nil || user.SystemEvent != nil {
		t.Fatalf("user variant leaked assistant/system fields: %#v", user)
	}

	toolUse := historyBlockToStructuredBlock(worker.HistoryBlock{
		Kind:        worker.BlockKindToolUse,
		Text:        "wrong-kind-text",
		ToolUseID:   "tool-1",
		Name:        "Read",
		ContentText: "wrong-kind-content",
	}, false)
	if toolUse == nil || toolUse.ID != "tool-1" {
		t.Fatalf("tool-use projection = %#v, want typed id", toolUse)
	}
	if toolUse.ToolCallID != "" || toolUse.Text != "" || toolUse.Content != "" {
		t.Fatalf("tool-use variant leaked result/text fields: %#v", toolUse)
	}

	toolResult := historyBlockToStructuredBlock(worker.HistoryBlock{
		Kind:        worker.BlockKindToolResult,
		Text:        "fallback result",
		ToolUseID:   "tool-1",
		ContentText: "result content",
		ImageURL:    "wrong-kind-image",
	}, false)
	if toolResult == nil || toolResult.ToolCallID != "tool-1" || toolResult.Content != "result content" {
		t.Fatalf("tool-result projection = %#v, want typed result fields", toolResult)
	}
	if toolResult.ID != "" || toolResult.Text != "" || toolResult.ImageURL != "" {
		t.Fatalf("tool-result variant leaked use/text/image fields: %#v", toolResult)
	}
}

func TestStructuredToolProjectionMatchesClosedVariantSchemas(t *testing.T) {
	schemas := componentSchemas(t, readLiveSupervisorOpenAPISpec(t))
	falseValue := false
	exitCode := 7

	input := worker.StructuredToolInput{
		Text:          "text",
		Command:       "command",
		LinkedCommand: "linked command",
		Code:          "code",
		Patch:         "patch",
		FilePath:      "file.txt",
		Language:      "text",
		URL:           "https://example.com",
		Prompt:        "prompt",
		TaskID:        "task-1",
		TaskType:      "worker",
		TaskStatus:    "completed",
		Description:   "description",
		Question:      "question",
		Options:       []string{"option"},
		Query:         "query",
		Pattern:       "pattern",
		Plan:          "plan",
		Explanation:   "explanation",
		Steps:         []worker.StructuredPlanStep{{Step: "step", Status: "done"}},
		Todos:         []worker.StructuredTodoItem{{ID: "todo-1", Content: "todo"}},
		Arguments:     []worker.StructuredArgument{{Name: "argument", Value: "value"}},
	}
	for kind, schemaName := range map[string]string{
		"unknown":   "SessionStructuredToolInputUnknown",
		"command":   "SessionStructuredToolInputCommand",
		"stdin":     "SessionStructuredToolInputStdin",
		"code":      "SessionStructuredToolInputCode",
		"patch":     "SessionStructuredToolInputPatch",
		"write":     "SessionStructuredToolInputWrite",
		"glob":      "SessionStructuredToolInputGlob",
		"fetch":     "SessionStructuredToolInputFetch",
		"search":    "SessionStructuredToolInputSearch",
		"file":      "SessionStructuredToolInputFile",
		"todo":      "SessionStructuredToolInputTodo",
		"plan":      "SessionStructuredToolInputPlan",
		"question":  "SessionStructuredToolInputQuestion",
		"task":      "SessionStructuredToolInputTask",
		"text":      "SessionStructuredToolInputText",
		"arguments": "SessionStructuredToolInputArguments",
	} {
		t.Run("input/"+kind, func(t *testing.T) {
			input.Kind = kind
			assertStructuredProjectionKeysMatchSchema(t, schemas, schemaName, sessionStructuredToolInputFromWorker(&input))
		})
	}

	result := worker.StructuredToolResult{
		Text:              "text",
		Command:           "command",
		Stdout:            "stdout",
		Stderr:            "stderr",
		ExitCode:          &exitCode,
		Interrupted:       true,
		Truncated:         true,
		IsImage:           true,
		Mode:              "mode",
		Query:             "query",
		URL:               "https://example.com",
		TaskID:            "task-1",
		TaskType:          "worker",
		TaskStatus:        "completed",
		Description:       "description",
		TotalDurationMs:   1,
		TotalTokens:       2,
		TotalToolUseCount: 3,
		Output:            "output",
		Question:          "question",
		Questions:         []worker.StructuredQuestion{{Question: "question"}},
		Answer:            "answer",
		Options:           []string{"option"},
		Answers:           []worker.StructuredArgument{{Name: "answer", Value: "value"}},
		Counts:            []worker.StructuredArgument{{Name: "count", Value: "1"}},
		StatusCode:        200,
		StatusText:        "OK",
		Bytes:             4,
		Filenames:         []string{"file.txt"},
		NumFiles:          1,
		NumResults:        1,
		DurationMs:        5,
		AppliedLimit:      6,
		StdoutLines:       7,
		StderrLines:       8,
		Timestamp:         "2026-01-01T00:00:00Z",
		ResultItems:       []worker.StructuredSearchResultItem{{Title: "result"}},
		Content:           "content",
		NumLines:          9,
		FilePath:          "file.txt",
		FilePaths:         []string{"file.txt"},
		Language:          "text",
		Code:              "code",
		Plan:              "plan",
		Explanation:       "explanation",
		Steps:             []worker.StructuredPlanStep{{Step: "step", Status: "done"}},
		Patch:             "patch",
		PatchHunks:        []worker.StructuredPatchHunk{{FilePath: "file.txt"}},
		OldString:         "old",
		NewString:         "new",
		OriginalFile:      "original",
		ReplaceAll:        &falseValue,
		UserModified:      &falseValue,
		OldTodos:          []worker.StructuredTodoItem{{ID: "old"}},
		NewTodos:          []worker.StructuredTodoItem{{ID: "new"}},
		StartLine:         10,
		TotalLines:        11,
		Error:             &worker.StructuredToolError{Category: "unknown", Message: "error"},
	}
	for kind, schemaName := range map[string]string{
		"unknown":  "SessionStructuredToolResultUnknown",
		"bash":     "SessionStructuredToolResultBash",
		"python":   "SessionStructuredToolResultPython",
		"read":     "SessionStructuredToolResultRead",
		"glob":     "SessionStructuredToolResultGlob",
		"grep":     "SessionStructuredToolResultGrep",
		"search":   "SessionStructuredToolResultSearch",
		"fetch":    "SessionStructuredToolResultFetch",
		"todo":     "SessionStructuredToolResultTodo",
		"plan":     "SessionStructuredToolResultPlan",
		"question": "SessionStructuredToolResultQuestion",
		"stdin":    "SessionStructuredToolResultStdin",
		"task":     "SessionStructuredToolResultTask",
		"write":    "SessionStructuredToolResultWrite",
		"edit":     "SessionStructuredToolResultEdit",
		"text":     "SessionStructuredToolResultText",
	} {
		t.Run("result/"+kind, func(t *testing.T) {
			result.Kind = kind
			assertStructuredProjectionKeysMatchSchema(t, schemas, schemaName, sessionStructuredToolResultFromWorker(&result))
		})
	}
}

func TestStructuredSearchInputProjectionOmitsOtherVariantFields(t *testing.T) {
	projected := sessionStructuredToolInputFromWorker(&worker.StructuredToolInput{
		Kind:        "search",
		Query:       "structured transcripts",
		URL:         "https://provider.example/search",
		TaskID:      "provider-task-1",
		Description: "provider search metadata",
	})
	if projected == nil {
		t.Fatal("search input projection is nil")
	}
	if projected.Kind != "search" || projected.Query != "structured transcripts" {
		t.Fatalf("search input projection = %#v, want typed search query", projected)
	}
	if projected.URL != "" || projected.TaskID != "" || projected.Description != "" {
		t.Fatalf("search input projection leaked fetch/task fields: %#v", projected)
	}
}

func assertStructuredProjectionKeysMatchSchema(t *testing.T, schemas map[string]map[string]any, schemaName string, projection any) {
	t.Helper()
	wire, err := json.Marshal(projection)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(wire, &object); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	schema, ok := schemas[schemaName]
	if !ok {
		t.Fatalf("components.schemas missing %s", schemaName)
	}
	properties := structuredSchemaProperties(t, schemaName, schema)
	for key := range object {
		if _, ok := properties[key]; !ok {
			t.Errorf("%s runtime projection emits schema-forbidden field %q: %s", schemaName, key, wire)
		}
	}
	for key := range properties {
		if _, ok := object[key]; !ok {
			t.Errorf("%s schema field %q is not projected from a populated neutral source: %s", schemaName, key, wire)
		}
	}
}

func assertStructuredDiscriminatedUnion(t *testing.T, schemas map[string]map[string]any, name, property string, variants map[string]string) {
	t.Helper()
	union, ok := schemas[name]
	if !ok {
		t.Fatalf("components.schemas missing %s", name)
	}
	oneOf, ok := union["oneOf"].([]any)
	if !ok || len(oneOf) != len(variants) {
		t.Fatalf("%s oneOf = %#v, want %d named variants", name, union["oneOf"], len(variants))
	}
	mapping := structuredDiscriminatorMapping(t, name, union, property)
	if len(mapping) != len(variants) {
		t.Fatalf("%s discriminator mapping has %d entries, want %d", name, len(mapping), len(variants))
	}
	for value, variantName := range variants {
		wantRef := "#/components/schemas/" + variantName
		if got := mapping[value]; got != wantRef {
			t.Fatalf("%s mapping[%q] = %q, want %q", name, value, got, wantRef)
		}
		variant := schemaByRef(t, schemas, wantRef)
		properties := structuredSchemaProperties(t, variantName, variant)
		assertSchemaLiteral(t, variantName+"."+property, properties[property], value)
		assertRequiredFields(t, name, value, variant, []string{property})
	}
}

func structuredDiscriminatorMapping(t *testing.T, name string, union map[string]any, property string) map[string]string {
	t.Helper()
	raw, ok := union["discriminator"].(map[string]any)
	if !ok {
		t.Fatalf("%s discriminator missing: %#v", name, union)
	}
	if got, _ := raw["propertyName"].(string); got != property {
		t.Fatalf("%s discriminator property = %q, want %q", name, got, property)
	}
	rawMapping, ok := raw["mapping"].(map[string]any)
	if !ok {
		t.Fatalf("%s discriminator mapping missing: %#v", name, raw)
	}
	mapping := make(map[string]string, len(rawMapping))
	for value, rawRef := range rawMapping {
		ref, ok := rawRef.(string)
		if !ok {
			t.Fatalf("%s discriminator mapping[%q] is not a string: %#v", name, value, rawRef)
		}
		mapping[value] = ref
	}
	return mapping
}

func structuredSchemaProperties(t *testing.T, name string, schema map[string]any) map[string]any {
	t.Helper()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s properties missing: %#v", name, schema)
	}
	return properties
}

func assertSchemaLiteral(t *testing.T, name string, raw any, want string) {
	t.Helper()
	schema, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s schema missing: %#v", name, raw)
	}
	if got, _ := schema["const"].(string); got != want {
		t.Fatalf("%s const = %q, want %q; schema=%#v", name, got, want, schema)
	}
}

func assertNonNullableRef(t *testing.T, name string, raw any, wantRef string) {
	t.Helper()
	schema, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s schema missing: %#v", name, raw)
	}
	if got, _ := schema["$ref"].(string); got != wantRef {
		t.Fatalf("%s ref = %q, want %q; schema=%#v", name, got, wantRef, schema)
	}
	if nullable, _ := schema["nullable"].(bool); nullable {
		t.Fatalf("%s is nullable: %#v", name, schema)
	}
}

func assertNonNullableArrayRef(t *testing.T, name string, raw any, wantItemRef string) {
	t.Helper()
	schema, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s schema missing: %#v", name, raw)
	}
	if got, _ := schema["type"].(string); got != "array" {
		t.Fatalf("%s type = %q, want array; schema=%#v", name, got, schema)
	}
	if nullable, _ := schema["nullable"].(bool); nullable {
		t.Fatalf("%s is nullable: %#v", name, schema)
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		t.Fatalf("%s items missing: %#v", name, schema)
	}
	if got, _ := items["$ref"].(string); got != wantItemRef {
		t.Fatalf("%s item ref = %q, want %q; schema=%#v", name, got, wantItemRef, schema)
	}
}

func jsonHasAllocatedArray(t *testing.T, wire []byte, field string) bool {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(wire, &object); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	array, ok := object[field].([]any)
	return ok && len(array) == 0
}
