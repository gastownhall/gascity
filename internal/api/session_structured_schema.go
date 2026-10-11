package api

import (
	"fmt"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/worker"
)

type structuredSchemaVariant struct {
	value    string
	name     string
	fields   []string
	required []string
}

var structuredMessageSchemaVariants = []structuredSchemaVariant{
	{
		value:  string(worker.ActorUnknown),
		name:   "SessionStructuredMessageUnknown",
		fields: []string{"id", "provider", "timestamp", "model", "stop_reason", "usage", "user_prompt", "system_event", "status", "blocks"},
	},
	{
		value:  string(worker.ActorUser),
		name:   "SessionStructuredMessageUser",
		fields: []string{"id", "provider", "timestamp", "user_prompt", "status", "blocks"},
	},
	{
		value:  string(worker.ActorAssistant),
		name:   "SessionStructuredMessageAssistant",
		fields: []string{"id", "provider", "timestamp", "model", "stop_reason", "usage", "status", "blocks"},
	},
	{
		value:  string(worker.ActorSystem),
		name:   "SessionStructuredMessageSystem",
		fields: []string{"id", "provider", "timestamp", "system_event", "status", "blocks"},
	},
	{
		value:  string(worker.ActorTool),
		name:   "SessionStructuredMessageTool",
		fields: []string{"id", "provider", "timestamp", "status", "blocks"},
	},
}

var structuredBlockSchemaVariants = []structuredSchemaVariant{
	{value: string(worker.BlockKindText), name: "SessionStructuredBlockText", fields: []string{"text"}},
	{value: string(worker.BlockKindThinking), name: "SessionStructuredBlockThinking", fields: []string{"thinking", "signature"}},
	{value: string(worker.BlockKindToolUse), name: "SessionStructuredBlockToolUse", fields: []string{"id", "name", "file_path", "input"}},
	{value: string(worker.BlockKindToolResult), name: "SessionStructuredBlockToolResult", fields: []string{"tool_call_id", "name", "file_path", "content", "is_error", "structured"}},
	{value: string(worker.BlockKindInteraction), name: "SessionStructuredBlockInteraction", fields: []string{"interaction"}},
	{value: string(worker.BlockKindImage), name: "SessionStructuredBlockImage", fields: []string{"text", "file_path", "image_url", "mime_type"}},
	{
		value:  string(worker.BlockKindUnknown),
		name:   "SessionStructuredBlockUnknown",
		fields: []string{"text", "thinking", "signature", "id", "tool_call_id", "name", "file_path", "image_url", "mime_type", "input", "content", "is_error", "structured", "interaction"},
	},
}

var structuredToolInputSchemaVariants = []structuredSchemaVariant{
	{
		value:  "unknown",
		name:   "SessionStructuredToolInputUnknown",
		fields: []string{"text", "command", "linked_command", "code", "patch", "file_path", "language", "url", "prompt", "task_id", "task_type", "task_status", "description", "question", "options", "query", "pattern", "plan", "explanation", "steps", "todos", "arguments"},
	},
	{value: "command", name: "SessionStructuredToolInputCommand", fields: []string{"command", "arguments"}, required: []string{"command"}},
	{value: "stdin", name: "SessionStructuredToolInputStdin", fields: []string{"task_id", "text", "linked_command"}},
	{value: "code", name: "SessionStructuredToolInputCode", fields: []string{"code", "language"}, required: []string{"code"}},
	{value: "patch", name: "SessionStructuredToolInputPatch", fields: []string{"patch", "file_path", "language"}, required: []string{"patch"}},
	{value: "write", name: "SessionStructuredToolInputWrite", fields: []string{"file_path", "language", "text"}},
	{value: "glob", name: "SessionStructuredToolInputGlob", fields: []string{"pattern", "query", "file_path", "arguments"}},
	{value: "fetch", name: "SessionStructuredToolInputFetch", fields: []string{"url", "prompt"}},
	{value: "search", name: "SessionStructuredToolInputSearch", fields: []string{"query", "pattern", "file_path", "command", "arguments"}},
	{value: "file", name: "SessionStructuredToolInputFile", fields: []string{"file_path", "language", "command"}, required: []string{"file_path"}},
	{value: "todo", name: "SessionStructuredToolInputTodo", fields: []string{"todos"}},
	{value: "plan", name: "SessionStructuredToolInputPlan", fields: []string{"plan", "explanation", "steps"}},
	{value: "question", name: "SessionStructuredToolInputQuestion", fields: []string{"question", "options"}},
	{value: "task", name: "SessionStructuredToolInputTask", fields: []string{"task_id", "task_type", "task_status", "description", "prompt"}},
	{value: "text", name: "SessionStructuredToolInputText", fields: []string{"text"}, required: []string{"text"}},
	{value: "arguments", name: "SessionStructuredToolInputArguments", fields: []string{"arguments"}, required: []string{"arguments"}},
}

var structuredToolResultSchemaVariants = []structuredSchemaVariant{
	{
		value: "unknown", name: "SessionStructuredToolResultUnknown",
		fields: []string{"text", "command", "stdout", "stderr", "exit_code", "interrupted", "truncated", "is_image", "mode", "query", "url", "task_id", "task_type", "task_status", "description", "total_duration_ms", "total_tokens", "total_tool_use_count", "output", "question", "questions", "answer", "options", "answers", "counts", "status_code", "status_text", "bytes", "filenames", "num_files", "num_results", "duration_ms", "applied_limit", "stdout_lines", "stderr_lines", "timestamp", "result_items", "content", "num_lines", "file_path", "file_paths", "language", "code", "plan", "explanation", "steps", "patch", "patch_hunks", "old_string", "new_string", "original_file", "replace_all", "user_modified", "old_todos", "new_todos", "start_line", "total_lines", "error"},
	},
	{value: "bash", name: "SessionStructuredToolResultBash", fields: []string{"text", "command", "stdout", "stderr", "exit_code", "interrupted", "truncated", "is_image", "task_id", "task_status", "stdout_lines", "stderr_lines", "timestamp", "content", "num_lines", "error"}},
	{value: "python", name: "SessionStructuredToolResultPython", fields: []string{"text", "code", "stdout", "stderr", "exit_code", "interrupted", "truncated", "is_image", "error"}},
	{value: "read", name: "SessionStructuredToolResultRead", fields: []string{"file_path", "language", "content", "num_lines", "start_line", "total_lines", "error"}},
	{value: "glob", name: "SessionStructuredToolResultGlob", fields: []string{"filenames", "num_files", "duration_ms", "truncated", "content", "num_lines", "error"}},
	{value: "grep", name: "SessionStructuredToolResultGrep", fields: []string{"mode", "query", "filenames", "num_files", "num_results", "counts", "duration_ms", "applied_limit", "result_items", "content", "num_lines", "error"}},
	{value: "search", name: "SessionStructuredToolResultSearch", fields: []string{"mode", "query", "filenames", "num_files", "num_results", "counts", "duration_ms", "applied_limit", "result_items", "content", "num_lines", "error"}},
	{value: "fetch", name: "SessionStructuredToolResultFetch", fields: []string{"text", "url", "status_code", "status_text", "bytes", "duration_ms", "content", "num_lines", "error"}},
	{value: "todo", name: "SessionStructuredToolResultTodo", fields: []string{"text", "content", "old_todos", "new_todos", "error"}},
	{value: "plan", name: "SessionStructuredToolResultPlan", fields: []string{"text", "content", "plan", "explanation", "steps", "error"}},
	{value: "question", name: "SessionStructuredToolResultQuestion", fields: []string{"text", "content", "question", "questions", "answer", "options", "answers", "error"}},
	{value: "stdin", name: "SessionStructuredToolResultStdin", fields: []string{"text", "task_id", "content", "num_lines", "error"}},
	{value: "task", name: "SessionStructuredToolResultTask", fields: []string{"text", "task_id", "task_type", "task_status", "description", "total_duration_ms", "total_tokens", "total_tool_use_count", "output", "stdout", "stderr", "exit_code", "content", "error"}},
	{value: "write", name: "SessionStructuredToolResultWrite", fields: []string{"text", "file_path", "file_paths", "language", "content", "num_lines", "patch", "patch_hunks", "start_line", "total_lines", "error"}},
	{value: "edit", name: "SessionStructuredToolResultEdit", fields: []string{"file_path", "file_paths", "patch", "patch_hunks", "old_string", "new_string", "original_file", "replace_all", "user_modified", "content", "error"}},
	{value: "text", name: "SessionStructuredToolResultText", fields: []string{"text", "content", "error"}},
}

type (
	sessionStructuredMessageSchemaFields            SessionStructuredMessage
	sessionStructuredBlockSchemaFields              SessionStructuredBlock
	sessionStructuredToolInputSchemaFields          SessionStructuredToolInput
	sessionStructuredToolResultSchemaFields         SessionStructuredToolResult
	sessionStreamStructuredMessageEventSchemaFields SessionStreamStructuredMessageEvent
	sessionTranscriptStructuredResponseSchemaFields sessionTranscriptStructuredResponse
)

// Schema registers SessionStructuredMessage as a named role-discriminated
// union. The runtime struct remains a compact projection carrier while the
// published contract gives generated clients closed role variants.
func (SessionStructuredMessage) Schema(r huma.Registry) *huma.Schema {
	return registerStructuredSchemaUnion(
		r,
		"SessionStructuredMessage",
		"Structured transcript message",
		"Provider-normalized transcript message discriminated by its closed role vocabulary.",
		"role",
		reflect.TypeOf(sessionStructuredMessageSchemaFields{}),
		structuredMessageSchemaVariants,
		[]string{"blocks"},
	)
}

// Schema registers SessionStructuredBlock as a named type-discriminated union.
func (SessionStructuredBlock) Schema(r huma.Registry) *huma.Schema {
	return registerStructuredSchemaUnion(
		r,
		"SessionStructuredBlock",
		"Structured transcript block",
		"Provider-normalized transcript block discriminated by its closed block type vocabulary.",
		"type",
		reflect.TypeOf(sessionStructuredBlockSchemaFields{}),
		structuredBlockSchemaVariants,
		nil,
	)
}

// Schema registers SessionStructuredToolInput as a named kind-discriminated
// union. Provider-native input remains available only through format=raw.
func (SessionStructuredToolInput) Schema(r huma.Registry) *huma.Schema {
	return registerStructuredSchemaUnion(
		r,
		"SessionStructuredToolInput",
		"Structured tool input",
		"Provider-neutral tool input discriminated by its closed kind vocabulary.",
		"kind",
		reflect.TypeOf(sessionStructuredToolInputSchemaFields{}),
		structuredToolInputSchemaVariants,
		nil,
	)
}

// Schema registers SessionStructuredToolResult as a named kind-discriminated
// union. Provider-native results remain available only through format=raw.
func (SessionStructuredToolResult) Schema(r huma.Registry) *huma.Schema {
	return registerStructuredSchemaUnion(
		r,
		"SessionStructuredToolResult",
		"Structured tool result",
		"Provider-neutral tool result discriminated by its closed kind vocabulary.",
		"kind",
		reflect.TypeOf(sessionStructuredToolResultSchemaFields{}),
		structuredToolResultSchemaVariants,
		nil,
	)
}

// Schema registers the structured SSE payload with literal format and schema
// values plus the required non-null REST-to-SSE handoff fields.
func (SessionStreamStructuredMessageEvent) Schema(r huma.Registry) *huma.Schema {
	const name = "SessionStreamStructuredMessageEvent"
	if _, ok := r.Map()[name]; !ok {
		schema := huma.SchemaFromType(r, reflect.TypeOf(sessionStreamStructuredMessageEventSchemaFields{}))
		schema.Title = "Structured session stream message"
		schema.Description = "Provider-neutral structured transcript update with explicit snapshot, upsert, or reset application semantics."
		// Keep this as a field-addressable object rather than a top-level oneOf:
		// oapi-codegen represents such unions as raw JSON wrappers. The closed
		// operation/reset-reason enums and the field's conditional-presence
		// documentation are the most precise contract that preserves typed Go
		// client fields; runtime construction enforces the combination.
		constrainStructuredEnvelopeSchema(schema)
		r.Map()[name] = schema
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}

// Schema registers the structured REST response with the same literal and
// required-field contract as the structured SSE payload.
func (sessionTranscriptStructuredResponse) Schema(r huma.Registry) *huma.Schema {
	const name = "SessionTranscriptStructuredResponse"
	if _, ok := r.Map()[name]; !ok {
		schema := huma.SchemaFromType(r, reflect.TypeOf(sessionTranscriptStructuredResponseSchemaFields{}))
		schema.Title = "Structured session transcript response"
		schema.Description = "Provider-neutral structured transcript snapshot."
		constrainStructuredSnapshotEnvelopeSchema(schema)
		r.Map()[name] = schema
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}

func registerStructuredSchemaUnion(
	r huma.Registry,
	name string,
	title string,
	description string,
	discriminator string,
	fieldsType reflect.Type,
	variants []structuredSchemaVariant,
	requiredNonNullableFields []string,
) *huma.Schema {
	if _, ok := r.Map()[name]; !ok {
		fields := huma.SchemaFromType(r, fieldsType)
		oneOf := make([]*huma.Schema, 0, len(variants))
		mapping := make(map[string]string, len(variants))
		for _, variant := range variants {
			ref := schemaRefPrefix + variant.name
			if _, ok := r.Map()[variant.name]; !ok {
				variantSchema := selectStructuredSchemaFields(fields, variant.name, append([]string{discriminator}, variant.fields...))
				variantSchema.Title = variant.name
				setStructuredSchemaLiteral(variantSchema, discriminator, variant.value)
				requireStructuredSchemaFields(variantSchema, discriminator)
				for _, field := range variant.required {
					requireStructuredSchemaFields(variantSchema, field)
					setStructuredSchemaNonNullable(variantSchema, field)
				}
				for _, field := range requiredNonNullableFields {
					requireStructuredSchemaFields(variantSchema, field)
					setStructuredSchemaNonNullable(variantSchema, field)
				}
				r.Map()[variant.name] = variantSchema
			}
			oneOf = append(oneOf, &huma.Schema{Ref: ref})
			mapping[variant.value] = ref
		}
		r.Map()[name] = &huma.Schema{
			Title:       title,
			Description: description,
			OneOf:       oneOf,
			Discriminator: &huma.Discriminator{
				PropertyName: discriminator,
				Mapping:      mapping,
			},
		}
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}

func selectStructuredSchemaFields(source *huma.Schema, variantName string, fieldNames []string) *huma.Schema {
	selected := &huma.Schema{
		Type:                 huma.TypeObject,
		AdditionalProperties: false,
		Properties:           make(map[string]*huma.Schema, len(fieldNames)),
	}
	required := make(map[string]bool, len(source.Required))
	for _, field := range source.Required {
		required[field] = true
	}
	for _, field := range fieldNames {
		property, ok := source.Properties[field]
		if !ok || property == nil {
			panic(fmt.Sprintf("structured schema variant %s names unknown field %q", variantName, field))
		}
		clone := *property
		selected.Properties[field] = &clone
		if required[field] {
			selected.Required = append(selected.Required, field)
		}
	}
	return selected
}

func constrainStructuredEnvelopeSchema(schema *huma.Schema) {
	setStructuredSchemaLiteral(schema, "format", "structured")
	setStructuredSchemaLiteral(schema, "schema_version", sessionStructuredSchemaVersion)
	requireStructuredSchemaFields(schema, "format", "schema_version", "history", "structured_messages")
	setStructuredSchemaNonNullable(schema, "history")
	setStructuredSchemaNonNullable(schema, "structured_messages")
}

func constrainStructuredSnapshotEnvelopeSchema(schema *huma.Schema) {
	constrainStructuredEnvelopeSchema(schema)
	setStructuredSchemaLiteral(schema, "operation", sessionStructuredOperationSnapshot)
	requireStructuredSchemaFields(schema, "operation")
	delete(schema.Properties, "reset_reason")
}

func setStructuredSchemaLiteral(schema *huma.Schema, field, value string) {
	property, ok := schema.Properties[field]
	if !ok || property == nil {
		property = &huma.Schema{Type: huma.TypeString}
	} else {
		clone := *property
		property = &clone
	}
	property.Nullable = false
	property.Enum = nil
	property.Extensions = map[string]any{"const": value}
	schema.Properties[field] = property
}

func setStructuredSchemaNonNullable(schema *huma.Schema, field string) {
	property, ok := schema.Properties[field]
	if !ok || property == nil {
		return
	}
	clone := *property
	clone.Nullable = false
	schema.Properties[field] = &clone
}

func requireStructuredSchemaFields(schema *huma.Schema, fields ...string) {
	seen := make(map[string]bool, len(schema.Required)+len(fields))
	for _, field := range schema.Required {
		seen[field] = true
	}
	for _, field := range fields {
		if seen[field] {
			continue
		}
		schema.Required = append(schema.Required, field)
		seen[field] = true
	}
}

func closedStructuredSchemaValue(value string, variants []structuredSchemaVariant) string {
	for _, variant := range variants {
		if value == variant.value {
			return value
		}
	}
	return "unknown"
}

func sessionStructuredMessageRole(actor worker.Actor) string {
	return closedStructuredSchemaValue(string(actor), structuredMessageSchemaVariants)
}

func sessionStructuredMessageStatus(status worker.ResultStatus) string {
	switch status {
	case worker.ResultStatusUnknown, worker.ResultStatusFinal, worker.ResultStatusPartial, worker.ResultStatusSuperseded:
		return string(status)
	default:
		return string(worker.ResultStatusUnknown)
	}
}

func sessionStructuredBlockType(kind worker.BlockKind) string {
	return closedStructuredSchemaValue(string(kind), structuredBlockSchemaVariants)
}

func sessionStructuredToolInputKind(kind string) string {
	return closedStructuredSchemaValue(kind, structuredToolInputSchemaVariants)
}

func sessionStructuredToolResultKind(kind string) string {
	return closedStructuredSchemaValue(kind, structuredToolResultSchemaVariants)
}
