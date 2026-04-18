package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// providerCreateRequest is the JSON body for POST /v0/providers.
//
// The authoring DTO accepts the full set of inheritance fields described
// in engdocs/design/provider-inheritance.md §HTTP. `Command` is optional
// when `Base` is set (the chain walk inherits it). `Base` is a
// json.RawMessage so the handler can distinguish:
//
//   - absent / JSON null: no declaration (nil *string on ProviderSpec)
//   - ""                 : explicit standalone opt-out
//   - "<name>"           : concrete value
//
// Similarly, OptionsSchema accepts entries with `omit = true` as the
// removal sentinel for options_schema_merge = "by_key".
type providerCreateRequest struct {
	Name               string                  `json:"name"`
	DisplayName        string                  `json:"display_name,omitempty"`
	Base               json.RawMessage         `json:"base,omitempty"`
	Command            string                  `json:"command,omitempty"`
	Args               []string                `json:"args,omitempty"`
	ArgsAppend         []string                `json:"args_append,omitempty"`
	PromptMode         string                  `json:"prompt_mode,omitempty"`
	PromptFlag         string                  `json:"prompt_flag,omitempty"`
	ReadyDelayMs       int                     `json:"ready_delay_ms,omitempty"`
	Env                map[string]string       `json:"env,omitempty"`
	OptionsSchemaMerge string                  `json:"options_schema_merge,omitempty"`
	OptionsSchema      []config.ProviderOption `json:"options_schema,omitempty"`
}

// providerUpdateRequest is the JSON body for PATCH /v0/provider/{name}.
// Fields use json.RawMessage or pointer types so the handler can tell
// apart three cases: absent key (no-op), explicit null (clear), present
// concrete value (set). See design §HTTP PATCH semantics.
type providerUpdateRequest struct {
	DisplayName        json.RawMessage         `json:"display_name,omitempty"`
	Base               json.RawMessage         `json:"base,omitempty"`
	Command            json.RawMessage         `json:"command,omitempty"`
	Args               []string                `json:"args,omitempty"`
	ArgsAppend         []string                `json:"args_append,omitempty"`
	PromptMode         json.RawMessage         `json:"prompt_mode,omitempty"`
	PromptFlag         json.RawMessage         `json:"prompt_flag,omitempty"`
	ReadyDelayMs       *int                    `json:"ready_delay_ms,omitempty"`
	Env                map[string]string       `json:"env,omitempty"`
	OptionsSchemaMerge json.RawMessage         `json:"options_schema_merge,omitempty"`
	OptionsSchema      []config.ProviderOption `json:"options_schema,omitempty"`
}

// jsonNull is the exact serialization of a JSON null literal.
// We compare against json.RawMessage using bytes.Equal to avoid
// whitespace sensitivity issues.
var jsonNull = []byte("null")

// rawPresence reports whether a json.RawMessage was present in the
// decoded body (non-nil slice). Empty bytes means the field was omitted.
func rawPresence(m json.RawMessage) (present bool) {
	return len(m) > 0
}

// rawIsNull reports whether a present json.RawMessage holds a JSON null
// literal. Caller must first check rawPresence.
func rawIsNull(m json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(m), jsonNull)
}

// rawDecodeString decodes a present, non-null json.RawMessage into a
// string. Returns an error if the RawMessage is not a valid JSON string.
func rawDecodeString(m json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(m, &s); err != nil {
		return "", err
	}
	return s, nil
}

// stringPtr returns a pointer to the given string. Internal helper.
func stringPtr(s string) *string { return &s }

func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	var body providerCreateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid", "name is required")
		return
	}

	// Parse Base presence: present+null/absent = nil; present+string = *string.
	var basePtr *string
	baseDeclared := rawPresence(body.Base) && !rawIsNull(body.Base)
	if baseDeclared {
		v, err := rawDecodeString(body.Base)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "base must be a string or null")
			return
		}
		basePtr = stringPtr(v)
	}

	// Command is optional only when base is declared. Preserve the
	// original "command is required" error text so existing test
	// assertions continue to match.
	if body.Command == "" && basePtr == nil {
		writeError(w, http.StatusBadRequest, "invalid", "command is required")
		return
	}

	if err := validateOptionsSchemaDTO(body.OptionsSchema); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	spec := config.ProviderSpec{
		DisplayName:        body.DisplayName,
		Base:               basePtr,
		Command:            body.Command,
		Args:               body.Args,
		ArgsAppend:         body.ArgsAppend,
		PromptMode:         body.PromptMode,
		PromptFlag:         body.PromptFlag,
		ReadyDelayMs:       body.ReadyDelayMs,
		Env:                body.Env,
		OptionsSchemaMerge: body.OptionsSchemaMerge,
		OptionsSchema:      body.OptionsSchema,
	}

	if err := sm.CreateProvider(body.Name, spec); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if strings.Contains(err.Error(), "validating") {
			writeError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "created", "provider": body.Name})
}

func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	var body providerUpdateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	patch, err := providerPatchFromRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	if err := sm.UpdateProvider(name, patch); err != nil {
		if strings.Contains(err.Error(), "not found") {
			if isBuiltinProvider(name) {
				writeError(w, http.StatusConflict, "conflict",
					"provider "+name+" is a builtin; use PUT /v0/patches/providers to override")
				return
			}
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if strings.Contains(err.Error(), "validating") {
			writeError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "provider": name})
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	if err := sm.DeleteProvider(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			if isBuiltinProvider(name) {
				writeError(w, http.StatusConflict, "conflict",
					"provider "+name+" is a builtin; use DELETE /v0/patches/provider/"+name+" to remove overrides")
				return
			}
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if strings.Contains(err.Error(), "validating") {
			writeError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "provider": name})
}

// isBuiltinProvider checks if a name is a known builtin provider.
func isBuiltinProvider(name string) bool {
	_, ok := config.BuiltinProviders()[name]
	return ok
}

// providerPatchFromRequest converts a wire-level update request into the
// ProviderUpdate patch consumed by the mutator. It resolves the
// absent/null/value trichotomy for each json.RawMessage field into the
// pointer-based patch semantics:
//
//   - absent  → leave patch field nil (no-op at apply time)
//   - null    → set patch field to a pointer to zero value (clear to default)
//   - value   → set patch field to a pointer with the decoded value
//
// Base is further propagated via **string on ProviderUpdate so the apply
// function can distinguish "clear to inherit" from "set to explicit ''".
func providerPatchFromRequest(body providerUpdateRequest) (ProviderUpdate, error) {
	var patch ProviderUpdate

	if rawPresence(body.DisplayName) {
		if rawIsNull(body.DisplayName) {
			patch.DisplayName = stringPtr("")
		} else {
			v, err := rawDecodeString(body.DisplayName)
			if err != nil {
				return patch, fmt.Errorf("display_name must be a string or null")
			}
			patch.DisplayName = stringPtr(v)
		}
	}
	if rawPresence(body.Command) {
		if rawIsNull(body.Command) {
			patch.Command = stringPtr("")
		} else {
			v, err := rawDecodeString(body.Command)
			if err != nil {
				return patch, fmt.Errorf("command must be a string or null")
			}
			patch.Command = stringPtr(v)
		}
	}
	if rawPresence(body.PromptMode) {
		if rawIsNull(body.PromptMode) {
			patch.PromptMode = stringPtr("")
		} else {
			v, err := rawDecodeString(body.PromptMode)
			if err != nil {
				return patch, fmt.Errorf("prompt_mode must be a string or null")
			}
			patch.PromptMode = stringPtr(v)
		}
	}
	if rawPresence(body.PromptFlag) {
		if rawIsNull(body.PromptFlag) {
			patch.PromptFlag = stringPtr("")
		} else {
			v, err := rawDecodeString(body.PromptFlag)
			if err != nil {
				return patch, fmt.Errorf("prompt_flag must be a string or null")
			}
			patch.PromptFlag = stringPtr(v)
		}
	}

	// Base uses **string so the apply layer can represent:
	//   patch.Base == nil                  → no-op
	//   patch.Base == &(*string)(nil)      → clear (removes Base declaration)
	//   patch.Base == &(&"")               → set explicit empty (opt-out)
	//   patch.Base == &(&"<name>")         → set concrete value
	if rawPresence(body.Base) {
		if rawIsNull(body.Base) {
			var zero *string
			patch.Base = &zero
		} else {
			v, err := rawDecodeString(body.Base)
			if err != nil {
				return patch, fmt.Errorf("base must be a string or null")
			}
			vPtr := stringPtr(v)
			patch.Base = &vPtr
		}
	}

	if rawPresence(body.OptionsSchemaMerge) {
		if rawIsNull(body.OptionsSchemaMerge) {
			patch.OptionsSchemaMerge = stringPtr("")
		} else {
			v, err := rawDecodeString(body.OptionsSchemaMerge)
			if err != nil {
				return patch, fmt.Errorf("options_schema_merge must be a string or null")
			}
			if v != "" && v != "replace" && v != "by_key" {
				return patch, fmt.Errorf("options_schema_merge must be \"replace\" or \"by_key\"")
			}
			patch.OptionsSchemaMerge = stringPtr(v)
		}
	}

	if body.Args != nil {
		patch.Args = body.Args
	}
	if body.ArgsAppend != nil {
		patch.ArgsAppend = body.ArgsAppend
	}
	if body.ReadyDelayMs != nil {
		patch.ReadyDelayMs = body.ReadyDelayMs
	}
	if len(body.Env) > 0 {
		patch.Env = body.Env
	}
	if body.OptionsSchema != nil {
		if err := validateOptionsSchemaDTO(body.OptionsSchema); err != nil {
			return patch, err
		}
		patch.OptionsSchema = body.OptionsSchema
	}
	return patch, nil
}

// validateOptionsSchemaDTO enforces the omit-entry invariants at the
// CRUD layer. Full schema validation (key uniqueness, merge-mode
// compatibility) happens later at config load time.
func validateOptionsSchemaDTO(schema []config.ProviderOption) error {
	for i, opt := range schema {
		if opt.Omit {
			// Omit entries must be key-only: no label/type/default/choices.
			if opt.Label != "" || opt.Type != "" || opt.Default != "" || len(opt.Choices) > 0 {
				return fmt.Errorf("options_schema[%d]: omit entries must be key-only (no label/type/default/choices)", i)
			}
			if opt.Key == "" {
				return fmt.Errorf("options_schema[%d]: omit entry requires key", i)
			}
		}
	}
	return nil
}
