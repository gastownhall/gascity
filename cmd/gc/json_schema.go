package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	gascity "github.com/gastownhall/gascity"
	"github.com/spf13/cobra"
)

const (
	jsonSchemaDirAnnotation = "gc.json.schema_dir"
	jsonSchemaManifestRole  = "manifest"
	jsonSchemaResultRole    = "result"
	jsonSchemaFailureRole   = "failure"
)

type jsonSchemaManifest struct {
	SchemaVersion string                     `json:"schema_version"`
	Command       []string                   `json:"command"`
	JSONSupported bool                       `json:"json_supported"`
	Schemas       map[string]json.RawMessage `json:"schemas"`
}

type jsonSchemaErrorPayload struct {
	SchemaVersion string                `json:"schema_version"`
	OK            bool                  `json:"ok"`
	Error         jsonSchemaErrorDetail `json:"error"`
}

type jsonSchemaErrorDetail struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	ExitCode int    `json:"exit_code"`
}

func configureJSONSchemaFlag(root *cobra.Command) {
	root.PersistentFlags().String("json-schema", "", "emit JSON Schema for this command; optional value: result or failure")
	if flag := root.PersistentFlags().Lookup("json-schema"); flag != nil {
		flag.NoOptDefVal = jsonSchemaManifestRole
	}
}

func handleJSONSchemaRequest(root *cobra.Command, args []string, stdout io.Writer) (bool, int) {
	action, ok := prepareJSONSchemaRequest(root, args)
	if !ok {
		return false, 0
	}
	return action.execute(stdout, io.Discard)
}

func prepareJSONSchemaRequest(root *cobra.Command, args []string) (jsonPreparedEarlyAction, bool) {
	request, ok := parseJSONSchemaRequest(args)
	if !ok {
		return jsonPreparedEarlyAction{}, false
	}

	cmd, _, err := root.Find(request.commandArgs)
	if err != nil || cmd == nil {
		return preparedJSONFailure(
			jsonPreparedEarlySchema,
			"json_schema_command_not_found",
			fmt.Sprintf("command %q was not found", strings.Join(request.commandArgs, " ")),
		), true
	}
	if cmd == root && len(request.commandArgs) > 0 {
		return preparedJSONFailure(
			jsonPreparedEarlySchema,
			"json_schema_command_not_found",
			fmt.Sprintf("command %q was not found", strings.Join(request.commandArgs, " ")),
		), true
	}

	commandPath := commandPathWords(cmd)
	if request.role == "" || request.role == jsonSchemaManifestRole {
		manifest := resolveJSONSchemaManifest(cmd, commandPath)
		return jsonPreparedEarlyAction{
			kind:     jsonPreparedEarlySchema,
			handled:  true,
			exitCode: 0,
			emit: func(stdout, _ io.Writer) int {
				if err := writeCLIJSONLine(stdout, manifest); err != nil {
					return 1
				}
				return 0
			},
		}, true
	}

	schema, err := schemaForRole(cmd, commandPath, request.role)
	if err != nil {
		return preparedJSONFailure(jsonPreparedEarlySchema, "json_schema_unavailable", err.Error()), true
	}
	schema = append(json.RawMessage(nil), schema...)
	return jsonPreparedEarlyAction{
		kind:     jsonPreparedEarlySchema,
		handled:  true,
		exitCode: 0,
		emit: func(stdout, _ io.Writer) int {
			if err := writeRawJSONLine(stdout, schema); err != nil {
				return 1
			}
			return 0
		},
	}, true
}

func handleJSONContractRequest(root *cobra.Command, args []string, stdout, stderr io.Writer) (bool, int) {
	action, ok := prepareJSONContractRequest(root, args)
	if !ok {
		return false, 0
	}
	return action.execute(stdout, stderr)
}

func prepareJSONContractRequest(root *cobra.Command, args []string) (jsonPreparedEarlyAction, bool) {
	request, disposition := resolveJSONContractDisposition(root, args)
	switch disposition {
	case jsonContractNotRequested, jsonContractPassthrough:
		return jsonPreparedEarlyAction{}, false
	case jsonContractPassthroughWithWarning:
		commandPath := commandPathWords(request.cmd)
		message := fmt.Sprintf("gc: warning: command %q does not declare JSON support; allowing --json pass-through during schema rollout (set GC_JSON_CONTRACT_STRICT=1 to enforce)\n", strings.Join(commandPath, " "))
		return jsonPreparedEarlyAction{
			kind:     jsonPreparedEarlyContractWarning,
			exitCode: 0,
			emit: func(_ io.Writer, stderr io.Writer) int {
				_, _ = io.WriteString(stderr, message)
				return 0
			},
		}, true
	case jsonContractCommandNotFound:
		return preparedJSONFailure(
			jsonPreparedEarlyContractFailure,
			"json_command_not_found",
			fmt.Sprintf("command %q was not found", strings.Join(request.commandArgs, " ")),
		), true
	case jsonContractUnsupported:
		commandPath := commandPathWords(request.cmd)
		return preparedJSONFailure(
			jsonPreparedEarlyContractFailure,
			"json_unsupported",
			fmt.Sprintf("command %q does not declare JSON support", strings.Join(commandPath, " ")),
		), true
	}
	return jsonPreparedEarlyAction{}, false
}

func shouldBufferJSONExecution(root *cobra.Command, args []string) bool {
	request, ok := resolveJSONRequest(root, args)
	if !ok {
		return false
	}
	if request.findErr != nil || request.cmd == nil {
		return true
	}
	commandPath := commandPathWords(request.cmd)
	if isBDCommandPath(commandPath) {
		return false
	}
	schema, err := readCommandSchema(request.cmd, commandPath, jsonSchemaResultRole)
	if err != nil {
		return !allowMissingLocalJSONSchemaPassthrough(request.cmd, err)
	}
	return !schemaDeclaresJSONL(schema)
}

func shouldReportJSONExecutionError(root *cobra.Command, args []string) bool {
	request, ok := resolveJSONRequest(root, args)
	if !ok {
		return false
	}
	if request.findErr != nil || request.cmd == nil {
		return true
	}
	commandPath := commandPathWords(request.cmd)
	if isBDCommandPath(commandPath) {
		return false
	}
	if _, err := readCommandSchema(request.cmd, commandPath, jsonSchemaResultRole); err != nil {
		return !allowMissingLocalJSONSchemaPassthrough(request.cmd, err)
	}
	return true
}

type jsonSchemaRequest struct {
	role        string
	commandArgs []string
}

type jsonRequest struct {
	cmd         *cobra.Command
	commandArgs []string
	findErr     error
}

type jsonContractDisposition uint8

const (
	jsonContractNotRequested jsonContractDisposition = iota
	jsonContractPassthrough
	jsonContractPassthroughWithWarning
	jsonContractCommandNotFound
	jsonContractUnsupported
)

type jsonPreparedEarlyKind uint8

const (
	jsonPreparedEarlyNone jsonPreparedEarlyKind = iota
	jsonPreparedEarlySchema
	jsonPreparedEarlyContractWarning
	jsonPreparedEarlyContractFailure
)

// jsonPreparedEarlyAction is stack-local output scaffolding. It may hold
// command-derived display data in its emitter, but is executed immediately and
// never crosses into product-metrics lifecycle state; only its closed metadata
// is projected there.
type jsonPreparedEarlyAction struct {
	kind     jsonPreparedEarlyKind
	handled  bool
	exitCode int
	emit     func(io.Writer, io.Writer) int
}

func (action jsonPreparedEarlyAction) execute(stdout, stderr io.Writer) (bool, int) {
	if action.emit == nil {
		return action.handled, action.exitCode
	}
	return action.handled, action.emit(stdout, stderr)
}

func prepareJSONEarlyAction(root *cobra.Command, args []string) (jsonPreparedEarlyAction, bool) {
	if action, ok := prepareJSONSchemaRequest(root, args); ok {
		return action, true
	}
	return prepareJSONContractRequest(root, args)
}

func preparedJSONFailure(kind jsonPreparedEarlyKind, code, message string) jsonPreparedEarlyAction {
	return jsonPreparedEarlyAction{
		kind:     kind,
		handled:  true,
		exitCode: 1,
		emit: func(stdout, _ io.Writer) int {
			return writeJSONSchemaUnavailable(stdout, code, message)
		},
	}
}

func resolveJSONContractDisposition(root *cobra.Command, args []string) (jsonRequest, jsonContractDisposition) {
	request, ok := resolveJSONRequest(root, args)
	if !ok {
		return jsonRequest{}, jsonContractNotRequested
	}
	cmd := request.cmd
	if request.findErr != nil || cmd == nil || (cmd == root && len(request.commandArgs) > 0) {
		return request, jsonContractCommandNotFound
	}
	commandPath := commandPathWords(cmd)
	if isBDCommandPath(commandPath) {
		return request, jsonContractPassthrough
	}
	if _, err := readCommandSchema(cmd, commandPath, jsonSchemaResultRole); err != nil {
		if allowMissingLocalJSONSchemaPassthrough(cmd, err) {
			return request, jsonContractPassthroughWithWarning
		}
		return request, jsonContractUnsupported
	}
	return request, jsonContractPassthrough
}

func resolveJSONRequest(root *cobra.Command, args []string) (jsonRequest, bool) {
	filteredArgs, jsonRequested := filterJSONFlag(args)
	if !jsonRequested {
		return jsonRequest{}, false
	}
	cmd, _, err := root.Find(filteredArgs)
	request := jsonRequest{
		cmd:         cmd,
		commandArgs: fallbackCommandArgs(filteredArgs),
		findErr:     err,
	}
	if cmd != nil {
		if commandPath := commandPathWords(cmd); len(commandPath) > 0 {
			request.commandArgs = commandPath
		}
	}
	return request, true
}

func filterJSONFlag(args []string) ([]string, bool) {
	filtered := make([]string, 0, len(args))
	jsonRequested := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			filtered = append(filtered, args[i:]...)
			break
		}
		switch {
		case arg == "--json":
			jsonRequested = true
		case strings.HasPrefix(arg, "--json="):
			value := strings.TrimPrefix(arg, "--json=")
			jsonRequested = value == "" || value == "true" || value == "1"
		default:
			filtered = append(filtered, arg)
		}
	}
	return filtered, jsonRequested
}

func isJSONControlArg(arg string) bool {
	return arg == "--json" || strings.HasPrefix(arg, "--json=")
}

func fallbackCommandArgs(args []string) []string {
	var words []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--city=") || strings.HasPrefix(arg, "--rig=") {
			continue
		}
		if arg == "--city" || arg == "--rig" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		words = append(words, arg)
	}
	return words
}

func parseJSONSchemaRequest(args []string) (jsonSchemaRequest, bool) {
	var request jsonSchemaRequest
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			request.commandArgs = append(request.commandArgs, args[i:]...)
			break
		}
		switch {
		case arg == "--json-schema":
			request.role, i = consumeJSONSchemaRole(args, i)
		case strings.HasPrefix(arg, "--json-schema="):
			request.role = strings.TrimPrefix(arg, "--json-schema=")
			if request.role == "" {
				request.role = jsonSchemaManifestRole
			}
		case arg == "--city" || arg == "--rig":
			i++
		case strings.HasPrefix(arg, "--city=") || strings.HasPrefix(arg, "--rig="):
			continue
		default:
			request.commandArgs = append(request.commandArgs, arg)
		}
	}
	if request.role == "" {
		return jsonSchemaRequest{}, false
	}
	return request, true
}

func consumeJSONSchemaRole(args []string, index int) (string, int) {
	role := jsonSchemaManifestRole
	if index+1 < len(args) && isJSONSchemaRole(args[index+1]) {
		return args[index+1], index + 1
	}
	return role, index
}

func isJSONSchemaRole(value string) bool {
	return value == jsonSchemaManifestRole || value == jsonSchemaResultRole || value == jsonSchemaFailureRole
}

func commandPathWords(cmd *cobra.Command) []string {
	var reversed []string
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		reversed = append(reversed, c.Name())
	}
	slices.Reverse(reversed)
	return reversed
}

func isBDCommandPath(commandPath []string) bool {
	return len(commandPath) > 0 && commandPath[0] == "bd"
}

func schemaDeclaresJSONL(schema json.RawMessage) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(schema, &object); err != nil {
		return false
	}
	_, ok := object["x-gc-jsonl"]
	return ok
}

func allowMissingLocalJSONSchemaPassthrough(cmd *cobra.Command, err error) bool {
	if cmd == nil || !os.IsNotExist(err) {
		return false
	}
	if strings.TrimSpace(cmd.Annotations[jsonSchemaDirAnnotation]) == "" {
		return false
	}
	return !strictPackJSONSchemaContract()
}

func strictPackJSONSchemaContract() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_JSON_CONTRACT_STRICT"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func resolveJSONSchemaManifest(cmd *cobra.Command, commandPath []string) jsonSchemaManifest {
	schemas := map[string]json.RawMessage{}
	resultSchema, resultErr := readCommandSchema(cmd, commandPath, jsonSchemaResultRole)
	if resultErr == nil {
		schemas[jsonSchemaResultRole] = resultSchema
		if failureSchema, err := schemaForRole(cmd, commandPath, jsonSchemaFailureRole); err == nil {
			schemas[jsonSchemaFailureRole] = failureSchema
		}
	}

	return jsonSchemaManifest{
		SchemaVersion: "1",
		Command:       commandPath,
		JSONSupported: resultErr == nil,
		Schemas:       schemas,
	}
}

func schemaForRole(cmd *cobra.Command, commandPath []string, role string) (json.RawMessage, error) {
	if role != jsonSchemaResultRole && role != jsonSchemaFailureRole {
		return nil, fmt.Errorf("unsupported schema role %q", role)
	}
	if _, err := readCommandSchema(cmd, commandPath, jsonSchemaResultRole); err != nil {
		return nil, fmt.Errorf("command %q does not declare JSON support", strings.Join(commandPath, " "))
	}
	if role == jsonSchemaFailureRole {
		if schema, err := readCommandSchema(cmd, commandPath, jsonSchemaFailureRole); err == nil {
			return schema, nil
		}
		return readSharedFailureSchema()
	}
	return readCommandSchema(cmd, commandPath, role)
}

func readCommandSchema(cmd *cobra.Command, commandPath []string, role string) (json.RawMessage, error) {
	if cmd != nil {
		if schemaDir := strings.TrimSpace(cmd.Annotations[jsonSchemaDirAnnotation]); schemaDir != "" {
			return readLocalSchema(filepath.Join(schemaDir, role+".schema.json"))
		}
	}
	return readBuiltinSchema(commandPath, role)
}

func readBuiltinSchema(commandPath []string, role string) (json.RawMessage, error) {
	if len(commandPath) == 0 {
		return nil, fmt.Errorf("root command does not declare JSON support")
	}
	parts := append([]string{"schemas"}, commandPath...)
	parts = append(parts, role+".schema.json")
	return readEmbeddedSchema(filepath.ToSlash(filepath.Join(parts...)))
}

func readSharedFailureSchema() (json.RawMessage, error) {
	return readEmbeddedSchema("schemas/failure.schema.json")
}

func readLocalSchema(path string) (json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("%s is not valid JSON", path)
	}
	return json.RawMessage(data), nil
}

func readEmbeddedSchema(path string) (json.RawMessage, error) {
	data, err := gascity.BuiltinSchemas.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("%s is not valid JSON", path)
	}
	return json.RawMessage(data), nil
}

func writeJSONSchemaUnavailable(stdout io.Writer, code, message string) int {
	const exitCode = 1
	_ = writeJSONFailure(stdout, code, message, exitCode)
	return exitCode
}

func writeJSONFailure(stdout io.Writer, code, message string, exitCode int) error {
	return writeCLIJSONLine(stdout, jsonSchemaErrorPayload{
		SchemaVersion: "1",
		OK:            false,
		Error: jsonSchemaErrorDetail{
			Code:     code,
			Message:  message,
			ExitCode: exitCode,
		},
	})
}

func writeCLIJSONLine(stdout io.Writer, value any) error {
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(withDefaultSuccessOK(value))
}

func writeCLIJSONLineOrExit(stdout, stderr io.Writer, context string, value any) int {
	if err := writeCLIJSONLine(stdout, value); err != nil {
		fmt.Fprintf(stderr, "%s: writing JSON result: %v\n", context, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func writeCLIJSONLineOrErr(stdout, stderr io.Writer, context string, value any) error {
	if writeCLIJSONLineOrExit(stdout, stderr, context, value) != 0 {
		return errExit
	}
	return nil
}

func writeRawJSONLine(stdout io.Writer, raw json.RawMessage) error {
	_, err := stdout.Write(raw)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, "\n")
	return err
}

func withDefaultSuccessOK(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return value
	}
	if object == nil {
		return value
	}
	if _, ok := object["ok"]; ok {
		return value
	}
	if _, hasSchemas := object["schemas"]; hasSchemas {
		if _, hasJSONSupported := object["json_supported"]; hasJSONSupported {
			return value
		}
	}
	object["ok"] = json.RawMessage("true")
	return object
}
