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
	Transport     string                     `json:"transport"`
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
	request, ok := parseJSONSchemaRequest(args)
	if !ok {
		return false, 0
	}

	cmd, _, err := root.Find(request.commandArgs)
	if err != nil || cmd == nil {
		return true, writeJSONSchemaUnavailable(stdout, "json_schema_command_not_found",
			fmt.Sprintf("command %q was not found", strings.Join(request.commandArgs, " ")))
	}
	if cmd == root && len(request.commandArgs) > 0 {
		return true, writeJSONSchemaUnavailable(stdout, "json_schema_command_not_found",
			fmt.Sprintf("command %q was not found", strings.Join(request.commandArgs, " ")))
	}

	commandPath := commandPathWords(cmd)
	if request.role == "" || request.role == jsonSchemaManifestRole {
		if err := writeJSONSchemaManifest(stdout, cmd, commandPath); err != nil {
			return true, 1
		}
		return true, 0
	}

	schema, err := schemaForRole(cmd, commandPath, request.role)
	if err != nil {
		return true, writeJSONSchemaUnavailable(stdout, "json_schema_unavailable", err.Error())
	}
	if err := writeRawJSONLine(stdout, schema); err != nil {
		return true, 1
	}
	return true, 0
}

type jsonSchemaRequest struct {
	role        string
	commandArgs []string
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
			request.role = jsonSchemaManifestRole
			if i+1 < len(args) && isJSONSchemaRole(args[i+1]) {
				request.role = args[i+1]
				i++
			}
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

func writeJSONSchemaManifest(stdout io.Writer, cmd *cobra.Command, commandPath []string) error {
	schemas := map[string]json.RawMessage{}
	resultSchema, resultErr := readCommandSchema(cmd, commandPath, jsonSchemaResultRole)
	if resultErr == nil {
		schemas[jsonSchemaResultRole] = resultSchema
		if failureSchema, err := schemaForRole(cmd, commandPath, jsonSchemaFailureRole); err == nil {
			schemas[jsonSchemaFailureRole] = failureSchema
		}
	}

	return writeCLIJSONLine(stdout, jsonSchemaManifest{
		SchemaVersion: "1",
		Command:       commandPath,
		Transport:     "jsonl",
		JSONSupported: resultErr == nil,
		Schemas:       schemas,
	})
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
	_ = writeCLIJSONLine(stdout, jsonSchemaErrorPayload{
		SchemaVersion: "1",
		OK:            false,
		Error: jsonSchemaErrorDetail{
			Code:     code,
			Message:  message,
			ExitCode: exitCode,
		},
	})
	return exitCode
}

func writeCLIJSONLine(stdout io.Writer, value any) error {
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}

func writeRawJSONLine(stdout io.Writer, raw json.RawMessage) error {
	_, err := stdout.Write(raw)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, "\n")
	return err
}
