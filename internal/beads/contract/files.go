// Package contract owns canonical beads/Dolt config and connection resolution.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

// EndpointOrigin describes who owns a scope's endpoint definition.
type EndpointOrigin string

// Canonical endpoint origin values.
const (
	EndpointOriginManagedCity   EndpointOrigin = "managed_city"
	EndpointOriginCityCanonical EndpointOrigin = "city_canonical"
	EndpointOriginInheritedCity EndpointOrigin = "inherited_city"
	EndpointOriginExplicit      EndpointOrigin = "explicit"
)

// EndpointStatus records whether a canonical external endpoint has been validated.
type EndpointStatus string

// Canonical endpoint status values.
const (
	EndpointStatusVerified   EndpointStatus = "verified"
	EndpointStatusUnverified EndpointStatus = "unverified"
)

// ConfigState is the canonical endpoint-bearing subset of .beads/config.yaml.
type ConfigState struct {
	IssuePrefix    string
	EndpointOrigin EndpointOrigin
	EndpointStatus EndpointStatus
	DoltHost       string
	DoltPort       string
	DoltUser       string
}

// MetadataState is the canonical subset of .beads/metadata.json used by GC.
//
// Backend determines which backend-specific fields are meaningful. When
// returned from LoadMetadataState the populated backend-specific fields are
// guaranteed consistent with Backend — i.e. a state with Backend == "postgres"
// always has every Postgres field non-empty and PostgresPort already verified
// as a TCP-port-shaped string.
type MetadataState struct {
	Database         string `json:"database"`
	Backend          string `json:"backend"`
	DoltMode         string `json:"dolt_mode,omitempty"`
	DoltDatabase     string `json:"dolt_database,omitempty"`
	PostgresHost     string `json:"postgres_host,omitempty"`
	PostgresPort     string `json:"postgres_port,omitempty"`
	PostgresUser     string `json:"postgres_user,omitempty"`
	PostgresDatabase string `json:"postgres_database,omitempty"`
}

// MetadataParseError reports a failure to parse or validate metadata.json.
//
// Returned by LoadMetadataState for JSON parse failures and for every E1–E5
// rejection in the metadata contract. Callers may use errors.As to
// discriminate parse failures from I/O failures (which surface as plain OS
// errors).
type MetadataParseError struct {
	// Path is the absolute path to the metadata.json file that failed.
	Path string
	// Reason is the verbatim rejection reason text (the part after `: `).
	Reason string
}

func (e *MetadataParseError) Error() string {
	return fmt.Sprintf("load metadata %s: %s", e.Path, e.Reason)
}

var deprecatedMetadataKeys = []string{
	"dolt_host",
	"dolt_user",
	"dolt_password",
	"dolt_server_host",
	"dolt_server_port",
	"dolt_server_user",
	"dolt_port",
}

var doltBackendKeys = []string{
	"dolt_mode",
	"dolt_database",
}

var postgresBackendKeys = []string{
	"postgres_host",
	"postgres_port",
	"postgres_user",
	"postgres_database",
}

// crossBackendKeysToScrub returns the on-disk metadata keys that should be
// removed when canonicalising for the given backend. An empty backend
// preserves all backend-specific keys (today's behavior for unknown-shape
// metadata).
func crossBackendKeysToScrub(backend string) []string {
	switch backend {
	case "dolt":
		return postgresBackendKeys
	case "postgres":
		return doltBackendKeys
	default:
		return nil
	}
}

var deprecatedConfigKeys = []string{
	"dolt.password",
	"dolt_port",
	"dolt_server_port",
}

type configParseError struct {
	path string
	err  error
}

func (e *configParseError) Error() string {
	return fmt.Sprintf("parse config %s: %v", e.path, e.err)
}

func (e *configParseError) Unwrap() error {
	return e.err
}

// ReadIssuePrefix reads the canonical issue prefix from config when present.
func ReadIssuePrefix(fs fsys.FS, path string) (string, bool, error) {
	doc, err := readConfigDoc(fs, path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if prefix, ok := scanConfigLineValue(fs, path, "issue_prefix:", "issue-prefix:"); ok {
			return prefix, true, nil
		}
		return "", false, err
	}
	if prefix, ok := configStringValue(mappingRoot(doc), "issue_prefix", "issue-prefix"); ok {
		return prefix, true, nil
	}
	return "", false, nil
}

// ReadAutoStartDisabled reports whether dolt.auto-start is disabled in config.
func ReadAutoStartDisabled(fs fsys.FS, path string) (bool, error) {
	doc, err := readConfigDoc(fs, path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		if value, ok := scanConfigLineValue(fs, path, "dolt.auto-start:"); ok {
			return value == "false", nil
		}
		return false, err
	}
	if value, ok := configStringValue(mappingRoot(doc), "dolt.auto-start"); ok {
		return value == "false", nil
	}
	return false, nil
}

// ReadEndpointStatus reads gc.endpoint_status when present.
func ReadEndpointStatus(fs fsys.FS, path string) (EndpointStatus, bool, error) {
	doc, err := readConfigDoc(fs, path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if data, readErr := fs.ReadFile(path); readErr == nil {
			if value, ok := scanConfigLineValueFromData(data, "gc.endpoint_status:"); ok {
				status := EndpointStatus(value)
				switch status {
				case EndpointStatusVerified, EndpointStatusUnverified:
					return status, true, nil
				}
				return "", false, nil
			}
		}
		return "", false, err
	}
	if value, ok := configStringValue(mappingRoot(doc), "gc.endpoint_status"); ok {
		status := EndpointStatus(value)
		switch status {
		case EndpointStatusVerified, EndpointStatusUnverified:
			return status, true, nil
		}
	}
	return "", false, nil
}

// ReadConfigState reads canonical endpoint config from .beads/config.yaml.
func ReadConfigState(fs fsys.FS, path string) (ConfigState, bool, error) {
	doc, err := readConfigDoc(fs, path)
	if err != nil {
		if os.IsNotExist(err) {
			return ConfigState{}, false, nil
		}
		data, readErr := fs.ReadFile(path)
		if readErr != nil {
			return ConfigState{}, false, err
		}
		return readConfigStateFromData(data), true, nil
	}
	return readConfigStateFromRoot(mappingRoot(doc)), true, nil
}

// ScopeHasEndpointAuthority reports whether a scope config carries endpoint authority.
func ScopeHasEndpointAuthority(fs fsys.FS, scopeRoot string) bool {
	cfg, ok, err := ReadConfigState(fs, filepath.Join(scopeRoot, ".beads", "config.yaml"))
	if err != nil || !ok {
		return false
	}
	return ConfigHasEndpointAuthority(cfg)
}

// ReadDoltDatabase reads the pinned dolt_database from metadata.json.
func ReadDoltDatabase(fs fsys.FS, path string) (string, bool, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", false, nil
	}
	if value := trimmedString(meta["dolt_database"]); value != "" {
		return value, true, nil
	}
	return "", false, nil
}

// LoadMetadataState parses .beads/metadata.json at path and returns the
// canonical MetadataState if the file exists and validates.
//
// Returns (zero, false, nil) when the file does not exist — callers decide
// whether absence is an error in their context (mirrors ReadIssuePrefix and
// ReadDoltDatabase). Returns a non-nil error for read failures other than
// ENOENT and for any of the E1–E5 rejection cases. Validation failures are
// wrapped in *MetadataParseError; callers may use errors.As to discriminate.
//
// Validation order is deterministic: the operator always sees the same
// top-most message when several things are wrong. Order is JSON parse (E1) →
// mixed-backend (E3) → unknown backend (E2) → postgres-required (E4) →
// postgres-port-format (E5). An empty Backend is permitted at the parse
// layer; downstream consumers that need a backend must check
// state.Backend != "" themselves.
func LoadMetadataState(fs fsys.FS, path string) (MetadataState, bool, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return MetadataState{}, false, nil
		}
		return MetadataState{}, false, err
	}

	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		abs = path
	}

	var state MetadataState
	if err := json.Unmarshal(data, &state); err != nil {
		return MetadataState{}, false, &MetadataParseError{
			Path:   abs,
			Reason: fmt.Sprintf("invalid metadata.json: %v", err),
		}
	}

	if other, ok := mixedBackendField(state); ok {
		return MetadataState{}, false, &MetadataParseError{
			Path:   abs,
			Reason: fmt.Sprintf("cannot mix dolt and postgres fields in a single scope (backend=%s but %s is also set)", state.Backend, other),
		}
	}

	switch state.Backend {
	case "", "dolt", "postgres":
		// allowed
	default:
		return MetadataState{}, false, &MetadataParseError{
			Path:   abs,
			Reason: fmt.Sprintf("unsupported backend %q (supported: dolt, postgres)", state.Backend),
		}
	}

	if state.Backend == "postgres" {
		if state.PostgresHost == "" || state.PostgresPort == "" || state.PostgresUser == "" || state.PostgresDatabase == "" {
			return MetadataState{}, false, &MetadataParseError{
				Path:   abs,
				Reason: "backend=postgres requires postgres_host, postgres_port, postgres_user, postgres_database (all four must be non-empty)",
			}
		}
		port, err := strconv.Atoi(state.PostgresPort)
		if err != nil || port < 1 || port > 65535 {
			return MetadataState{}, false, &MetadataParseError{
				Path:   abs,
				Reason: fmt.Sprintf("postgres_port must be a TCP port (1..65535), got %q", state.PostgresPort),
			}
		}
	}

	return state, true, nil
}

// mixedBackendField reports the first populated "other-backend" field name
// (relative to state.Backend). For explicit backends, any populated field from
// the opposite backend is mixed. For empty or unknown backends, mixed still
// means both Dolt-shaped and Postgres-shaped fields are populated.
//
// Field-iteration order is the JSON-key declaration order on MetadataState
// (dolt_mode, dolt_database, postgres_host, postgres_port, postgres_user,
// postgres_database). When state.Backend is empty or unknown and both backend
// families appear, the first populated field across both backends wins (with
// Dolt fields preferred per declaration order).
func mixedBackendField(state MetadataState) (string, bool) {
	type entry struct {
		name    string
		value   string
		backend string
	}
	fields := []entry{
		{"dolt_mode", state.DoltMode, "dolt"},
		{"dolt_database", state.DoltDatabase, "dolt"},
		{"postgres_host", state.PostgresHost, "postgres"},
		{"postgres_port", state.PostgresPort, "postgres"},
		{"postgres_user", state.PostgresUser, "postgres"},
		{"postgres_database", state.PostgresDatabase, "postgres"},
	}
	var firstDolt, firstPostgres string
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		if f.backend == "dolt" && firstDolt == "" {
			firstDolt = f.name
		}
		if f.backend == "postgres" && firstPostgres == "" {
			firstPostgres = f.name
		}
	}
	switch state.Backend {
	case "postgres":
		if firstDolt != "" {
			return firstDolt, true
		}
	case "dolt":
		if firstPostgres != "" {
			return firstPostgres, true
		}
	default:
		if firstDolt != "" && firstPostgres != "" {
			return firstDolt, true
		}
	}
	return "", false
}

// EnsureCanonicalConfig rewrites config.yaml into canonical GC-managed form.
func EnsureCanonicalConfig(fs fsys.FS, path string, state ConfigState) (bool, error) {
	missing := false
	doc, err := readConfigDoc(fs, path)
	if err != nil {
		if isConfigParseError(err) {
			return ensureCanonicalConfigFallback(fs, path, state)
		}
		if !os.IsNotExist(err) {
			return false, err
		}
		missing = true
		doc = newConfigDoc()
	}

	root := mappingRoot(doc)
	existingPrefix, _ := configStringValue(root, "issue_prefix", "issue-prefix")
	prefix := strings.TrimSpace(state.IssuePrefix)
	if prefix == "" {
		prefix = existingPrefix
	}

	changed := missing
	if prefix != "" {
		changed = setString(root, "issue_prefix", prefix) || changed
		changed = setString(root, "issue-prefix", prefix) || changed
	}
	changed = setBool(root, "dolt.auto-start", false) || changed
	// Managed beads are Dolt-backed; issues.jsonl auto-export is redundant and
	// triggers a re-import cycle that stalls bd writes for minutes on large
	// datasets. BD_EXPORT_AUTO env-var suppression only covers gc's own calls,
	// so bake it into the on-disk config too.
	changed = setBool(root, "export.auto", false) || changed
	if state.EndpointOrigin != "" {
		changed = setString(root, "gc.endpoint_origin", string(state.EndpointOrigin)) || changed
	}
	if state.EndpointStatus != "" {
		changed = setString(root, "gc.endpoint_status", string(state.EndpointStatus)) || changed
	}

	host := strings.TrimSpace(state.DoltHost)
	port := strings.TrimSpace(state.DoltPort)
	user := strings.TrimSpace(state.DoltUser)
	if host != "" {
		changed = setString(root, "dolt.host", host) || changed
	} else {
		changed = deleteKeys(root, "dolt.host") || changed
	}
	if port != "" {
		changed = setPort(root, "dolt.port", port) || changed
	} else {
		changed = deleteKeys(root, "dolt.port") || changed
	}
	if user != "" {
		changed = setString(root, "dolt.user", user) || changed
	} else {
		changed = deleteKeys(root, "dolt.user") || changed
	}

	changed = deleteKeys(root, deprecatedConfigKeys...) || changed
	if !changed {
		return false, nil
	}

	encoded, err := marshalConfigDoc(doc)
	if err != nil {
		return false, err
	}
	return true, fs.WriteFile(path, encoded, 0o644)
}

// EnsureCanonicalMetadata rewrites metadata.json into canonical GC-managed form.
func EnsureCanonicalMetadata(fs fsys.FS, path string, state MetadataState) (bool, error) {
	meta := map[string]any{}
	data, err := fs.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &meta); err != nil {
			meta = map[string]any{}
		}
	case os.IsNotExist(err):
	case err != nil:
		return false, err
	}

	changed := false
	defaults := map[string]string{
		"database":          strings.TrimSpace(state.Database),
		"backend":           strings.TrimSpace(state.Backend),
		"dolt_mode":         strings.TrimSpace(state.DoltMode),
		"dolt_database":     strings.TrimSpace(state.DoltDatabase),
		"postgres_host":     strings.TrimSpace(state.PostgresHost),
		"postgres_port":     strings.TrimSpace(state.PostgresPort),
		"postgres_user":     strings.TrimSpace(state.PostgresUser),
		"postgres_database": strings.TrimSpace(state.PostgresDatabase),
	}
	for key, want := range defaults {
		if want == "" {
			continue
		}
		if trimmedString(meta[key]) != want {
			meta[key] = want
			changed = true
		}
	}
	for _, key := range deprecatedMetadataKeys {
		if _, ok := meta[key]; ok {
			delete(meta, key)
			changed = true
		}
	}
	for _, key := range crossBackendKeysToScrub(strings.TrimSpace(state.Backend)) {
		if _, ok := meta[key]; ok {
			delete(meta, key)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}

	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return false, err
	}
	encoded = append(encoded, '\n')
	return true, fs.WriteFile(path, encoded, 0o644)
}

func ensureCanonicalConfigFallback(fs fsys.FS, path string, state ConfigState) (bool, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return false, err
	}

	prefix := strings.TrimSpace(state.IssuePrefix)
	if prefix == "" {
		if existing, ok := scanConfigLineValueFromData(data, "issue_prefix:", "issue-prefix:"); ok {
			prefix = existing
		}
	}

	replacements := map[string]string{
		"dolt.auto-start": "dolt.auto-start: false",
		"export.auto":     "export.auto: false",
	}
	if prefix != "" {
		replacements["issue_prefix"] = "issue_prefix: " + prefix
		replacements["issue-prefix"] = "issue-prefix: " + prefix
	}
	if state.EndpointOrigin != "" {
		replacements["gc.endpoint_origin"] = "gc.endpoint_origin: " + string(state.EndpointOrigin)
	}
	if state.EndpointStatus != "" {
		replacements["gc.endpoint_status"] = "gc.endpoint_status: " + string(state.EndpointStatus)
	}

	host := strings.TrimSpace(state.DoltHost)
	port := strings.TrimSpace(state.DoltPort)
	user := strings.TrimSpace(state.DoltUser)
	deletions := map[string]struct{}{
		"dolt.password":    {},
		"dolt_port":        {},
		"dolt_server_port": {},
	}
	if host != "" {
		replacements["dolt.host"] = "dolt.host: " + host
	} else {
		deletions["dolt.host"] = struct{}{}
	}
	if port != "" {
		replacements["dolt.port"] = "dolt.port: " + port
	} else {
		deletions["dolt.port"] = struct{}{}
	}
	if user != "" {
		replacements["dolt.user"] = "dolt.user: " + user
	} else {
		deletions["dolt.user"] = struct{}{}
	}

	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines)+len(replacements))
	seen := make(map[string]bool, len(replacements))
	changed := false

	for _, line := range lines {
		key, _, ok := topLevelConfigLine(line)
		if !ok {
			out = append(out, line)
			continue
		}
		if _, drop := deletions[key]; drop {
			changed = true
			continue
		}
		want, manage := replacements[key]
		if !manage {
			out = append(out, line)
			continue
		}
		if seen[key] {
			changed = true
			continue
		}
		seen[key] = true
		if strings.TrimSpace(line) != want {
			out = append(out, want)
			changed = true
			continue
		}
		out = append(out, line)
	}

	orderedKeys := []string{
		"issue_prefix",
		"issue-prefix",
		"dolt.auto-start",
		"export.auto",
		"gc.endpoint_origin",
		"gc.endpoint_status",
		"dolt.host",
		"dolt.port",
		"dolt.user",
	}
	for _, key := range orderedKeys {
		want, ok := replacements[key]
		if !ok || seen[key] {
			continue
		}
		out = append(out, want)
		changed = true
	}

	if !changed {
		return false, nil
	}
	if len(out) == 0 || strings.TrimSpace(out[len(out)-1]) != "" {
		out = append(out, "")
	}
	return true, fs.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644)
}

func isConfigParseError(err error) bool {
	var target *configParseError
	return errors.As(err, &target)
}

func newConfigDoc() *yaml.Node {
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
}

func readConfigDoc(fs fsys.FS, path string) (*yaml.Node, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return newConfigDoc(), nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &configParseError{path: path, err: err}
	}
	if len(doc.Content) == 0 {
		return newConfigDoc(), nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, &configParseError{path: path, err: fmt.Errorf("root must be a mapping")}
	}
	return &doc, nil
}

func marshalConfigDoc(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mappingRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func configStringValue(root *yaml.Node, keys ...string) (string, bool) {
	for _, key := range keys {
		if node := findValue(root, key); node != nil {
			if value := strings.TrimSpace(node.Value); value != "" {
				return value, true
			}
		}
	}
	return "", false
}

func scanConfigLineValue(fs fsys.FS, path string, prefixes ...string) (string, bool) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return "", false
	}
	return scanConfigLineValueFromData(data, prefixes...)
}

func scanConfigLineValueFromData(data []byte, prefixes ...string) (string, bool) {
	for _, line := range strings.Split(string(data), string(rune(10))) {
		key, value, ok := topLevelConfigLine(line)
		if !ok {
			continue
		}
		candidate := key + ":"
		for _, prefix := range prefixes {
			if candidate == prefix && value != "" {
				return value, true
			}
		}
	}
	return "", false
}

func readConfigStateFromData(data []byte) ConfigState {
	return ConfigState{
		IssuePrefix:    scanConfigValueFromData(data, "issue_prefix:", "issue-prefix:"),
		EndpointOrigin: endpointOriginValue(scanConfigValueFromData(data, "gc.endpoint_origin:")),
		EndpointStatus: endpointStatusValue(scanConfigValueFromData(data, "gc.endpoint_status:")),
		DoltHost:       scanConfigValueFromData(data, "dolt.host:"),
		DoltPort:       scanConfigValueFromData(data, "dolt.port:"),
		DoltUser:       scanConfigValueFromData(data, "dolt.user:"),
	}
}

func readConfigStateFromRoot(root *yaml.Node) ConfigState {
	return ConfigState{
		IssuePrefix:    configValue(root, "issue_prefix", "issue-prefix"),
		EndpointOrigin: endpointOriginValue(configValue(root, "gc.endpoint_origin")),
		EndpointStatus: endpointStatusValue(configValue(root, "gc.endpoint_status")),
		DoltHost:       configValue(root, "dolt.host"),
		DoltPort:       configValue(root, "dolt.port"),
		DoltUser:       configValue(root, "dolt.user"),
	}
}

func configValue(root *yaml.Node, keys ...string) string {
	value, _ := configStringValue(root, keys...)
	return value
}

func scanConfigValueFromData(data []byte, prefixes ...string) string {
	value, _ := scanConfigLineValueFromData(data, prefixes...)
	return value
}

func topLevelConfigLine(line string) (key, value string, ok bool) {
	if strings.TrimSpace(line) == "" {
		return "", "", false
	}
	if strings.TrimLeft(line, " 	") != line {
		return "", "", false
	}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	key, value, ok = strings.Cut(trimmed, ":")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(key), strings.TrimSpace(value), true
}

func endpointOriginValue(value string) EndpointOrigin {
	origin := EndpointOrigin(strings.TrimSpace(value))
	switch origin {
	case EndpointOriginManagedCity, EndpointOriginCityCanonical, EndpointOriginInheritedCity, EndpointOriginExplicit:
		return origin
	default:
		return ""
	}
}

func endpointStatusValue(value string) EndpointStatus {
	status := EndpointStatus(strings.TrimSpace(value))
	switch status {
	case EndpointStatusVerified, EndpointStatusUnverified:
		return status
	default:
		return ""
	}
}

func findValue(root *yaml.Node, key string) *yaml.Node {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}

func setString(root *yaml.Node, key, value string) bool {
	return setScalar(root, key, value, "!!str")
}

func setBool(root *yaml.Node, key string, value bool) bool {
	if value {
		return setScalar(root, key, "true", "!!bool")
	}
	return setScalar(root, key, "false", "!!bool")
}

func setPort(root *yaml.Node, key, value string) bool {
	if _, err := strconv.Atoi(value); err == nil {
		return setScalar(root, key, value, "!!int")
	}
	return setScalar(root, key, value, "!!str")
}

func setScalar(root *yaml.Node, key, value, tag string) bool {
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	out := make([]*yaml.Node, 0, len(root.Content))
	seen := false
	changed := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != key {
			out = append(out, root.Content[i], root.Content[i+1])
			continue
		}
		if seen {
			changed = true
			continue
		}
		seen = true
		current := root.Content[i+1]
		if current.Kind != yaml.ScalarNode || current.Value != value || current.Tag != tag {
			current = &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
			changed = true
		}
		out = append(out, root.Content[i], current)
	}
	if seen {
		if changed {
			root.Content = out
		}
		return changed
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value},
	)
	return true
}

func deleteKeys(root *yaml.Node, keys ...string) bool {
	if root == nil || root.Kind != yaml.MappingNode || len(keys) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	out := make([]*yaml.Node, 0, len(root.Content))
	changed := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if _, ok := set[root.Content[i].Value]; ok {
			changed = true
			continue
		}
		out = append(out, root.Content[i], root.Content[i+1])
	}
	if changed {
		root.Content = out
	}
	return changed
}

func trimmedString(value any) string {
	trimmed := strings.TrimSpace(fmt.Sprint(value))
	if trimmed == "<nil>" {
		return ""
	}
	return trimmed
}
