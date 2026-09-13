package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// structuredTranscriptWireAllowedKeys is the allowlist of JSON keys the typed
// structured transcript response can legitimately serialize. Any key outside it
// on the wire is a leaked provider-native key.
func structuredTranscriptWireAllowedKeys() map[string]struct{} {
	return worker.NeutralWireKeys(reflect.TypeOf(sessionTranscriptGetResponse{}))
}

// assertNoStructuredWireLeak fails the test if the serialized structured wire
// carries any provider-native shape. It applies both leakage gates: the
// canonical provider-native token denylist (plus any case-specific extras) and
// the schema allowlist, which catches future native keys the denylist does not
// yet name.
func assertNoStructuredWireLeak(t *testing.T, wire []byte, extraForbidden ...string) {
	t.Helper()
	leaked, err := structuredWireLeakage(wire, extraForbidden...)
	if err != nil {
		t.Fatalf("scan structured wire keys: %v", err)
	}
	if len(leaked) > 0 {
		t.Fatalf("structured response leaked provider-native data %v: %s", leaked, wire)
	}
}

func structuredWireLeakage(wire []byte, extraForbidden ...string) ([]string, error) {
	leaked := make(map[string]struct{})
	for _, token := range extraForbidden {
		if token != "" && bytes.Contains(wire, []byte(token)) {
			leaked["forbidden:"+token] = struct{}{}
		}
	}
	unexpected, err := worker.UnexpectedWireKeys(wire, structuredTranscriptWireAllowedKeys())
	if err != nil {
		return nil, err
	}
	for _, key := range unexpected {
		leaked["key:"+key] = struct{}{}
	}
	var decoded any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		return nil, err
	}
	collectStructuredArgumentLeakage(decoded, "", leaked)
	collectStructuredHistoryEnvelopeLeakage(decoded, leaked)
	if len(leaked) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(leaked))
	for item := range leaked {
		out = append(out, item)
	}
	sort.Strings(out)
	return out, nil
}

var neutralStructuredInputArgumentNames = map[string]struct{}{
	"path": {},
}

func collectStructuredArgumentLeakage(value any, path string, leaked map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			switch key {
			case "arguments":
				scanStructuredArguments(child, childPath, true, leaked)
			case "answers", "counts":
				scanStructuredArguments(child, childPath, false, leaked)
			}
			collectStructuredArgumentLeakage(child, childPath, leaked)
		}
	case []any:
		for i, child := range typed {
			collectStructuredArgumentLeakage(child, path+"["+strconv.Itoa(i)+"]", leaked)
		}
	}
}

// rawGenerationTokenPattern matches the worker's raw "<mtime>:<size>" generation
// token — file-observation evidence that must not reach the structured wire.
var rawGenerationTokenPattern = regexp.MustCompile(`^\d+:\d+$`)

// collectStructuredHistoryEnvelopeLeakage flags server-only filesystem evidence
// that legitimate envelope KEYS can smuggle as VALUES. The key-allowlist gate
// accepts transcript_stream_id and generation because they are real fields; it
// cannot see that transcript_stream_id must never be an absolute server path, or
// that generation must not carry the raw file mtime:size. A path separator in
// the stream identity, a bare "<int>:<int>" generation id, or a populated
// observed_at is such a leak. This is envelope-scoped, so it never
// false-positives on the legitimate file paths that appear inside tool
// inputs/results, which are real transcript content rather than server metadata.
func collectStructuredHistoryEnvelopeLeakage(value any, leaked map[string]struct{}) {
	root, ok := value.(map[string]any)
	if !ok {
		return
	}
	history, ok := root["history"].(map[string]any)
	if !ok {
		return
	}
	if streamID, ok := history["transcript_stream_id"].(string); ok && strings.ContainsAny(streamID, `/\`) {
		leaked["history.transcript_stream_id:server_path"] = struct{}{}
	}
	generation, ok := history["generation"].(map[string]any)
	if !ok {
		return
	}
	if id, ok := generation["id"].(string); ok && rawGenerationTokenPattern.MatchString(id) {
		leaked["history.generation.id:raw_mtime_size"] = struct{}{}
	}
	if observed, ok := generation["observed_at"].(string); ok && observed != "" {
		leaked["history.generation.observed_at:file_mtime"] = struct{}{}
	}
}

func scanStructuredArguments(value any, path string, restrictNames bool, leaked map[string]struct{}) {
	items, ok := value.([]any)
	if !ok {
		leaked[path+":not_array"] = struct{}{}
		return
	}
	for i, item := range items {
		itemPath := path + "[" + strconv.Itoa(i) + "]"
		argument, ok := item.(map[string]any)
		if !ok {
			leaked[itemPath+":not_object"] = struct{}{}
			continue
		}
		name, nameOK := argument["name"].(string)
		if !nameOK || strings.TrimSpace(name) == "" {
			leaked[itemPath+".name:missing"] = struct{}{}
		} else {
			if restrictNames {
				if _, allowed := neutralStructuredInputArgumentNames[name]; !allowed {
					leaked[itemPath+".name:"+name] = struct{}{}
				}
			}
			scanStructuredArgumentTokens(name, itemPath+".name", leaked)
		}

		argumentValue, exists := argument["value"]
		if !exists {
			leaked[itemPath+".value:missing"] = struct{}{}
			continue
		}
		valueText, valueOK := argumentValue.(string)
		if !valueOK {
			switch argumentValue.(type) {
			case map[string]any:
				leaked[itemPath+".value:json_object"] = struct{}{}
			case []any:
				leaked[itemPath+".value:json_array"] = struct{}{}
			default:
				leaked[itemPath+".value:not_string"] = struct{}{}
			}
			continue
		}
		scanStructuredArgumentTokens(valueText, itemPath+".value", leaked)
		if kind := jsonStringContainerKind(valueText); kind != "" {
			leaked[itemPath+".value:"+kind] = struct{}{}
		}
	}
}

func scanStructuredArgumentTokens(value, path string, leaked map[string]struct{}) {
	for _, token := range worker.ProviderNativeForbiddenTokens() {
		if token != "" && strings.Contains(value, token) {
			leaked[path+":provider_token="+token] = struct{}{}
		}
	}
}

func jsonStringContainerKind(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return ""
	}
	switch decoded.(type) {
	case map[string]any:
		return "json_object"
	case []any:
		return "json_array"
	default:
		return ""
	}
}

// TestStructuredWireTypesHaveNoMapFields enforces the load-bearing assumption
// behind the allowlist leakage gate: the structured wire payload must contain no
// map fields. NeutralWireKeys cannot enumerate a map's dynamic keys, so if one
// is added the allowlist would silently miss provider-native keys nested inside
// it. If this fails, exclude the new map subtree before calling
// UnexpectedWireKeys (and update assertNoStructuredWireLeak accordingly).
func TestStructuredWireTypesHaveNoMapFields(t *testing.T) {
	roots := []reflect.Type{
		reflect.TypeOf(SessionStructuredHistory{}),
		reflect.TypeOf(SessionStructuredMessage{}),
		reflect.TypeOf(SessionStreamStructuredMessageEvent{}),
	}
	for _, root := range roots {
		if path := firstMapField(root, map[reflect.Type]struct{}{}, root.Name()); path != "" {
			t.Fatalf("structured wire type carries a map field at %s; the allowlist leakage gate cannot enumerate its dynamic keys", path)
		}
	}
}

// firstMapField returns the dotted path to the first map-typed field reachable
// from t, or "" if none exists.
func firstMapField(t reflect.Type, seen map[reflect.Type]struct{}, path string) string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() == reflect.Map {
		return path
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	if _, ok := seen[t]; ok {
		return ""
	}
	seen[t] = struct{}{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		if hit := firstMapField(field.Type, seen, path+"."+field.Name); hit != "" {
			return hit
		}
	}
	return ""
}

// Inline-subagent lineage is intentionally outside session.structured.v1.
// Keep the reserved fields off the v1 wire until the versioned follow-up has
// real provider evidence and end-to-end coverage (ga-mb46n3).
func TestStructuredV1ExcludesInlineSubagentLineage(t *testing.T) {
	typ := reflect.TypeOf(SessionStructuredMessage{})
	for _, field := range []string{"IsSubagent", "ParentToolCallID"} {
		if _, ok := typ.FieldByName(field); ok {
			t.Fatalf("SessionStructuredMessage still exposes v1 lineage field %s", field)
		}
	}
}

func TestStructuredLeakageGateCatchesInjectedNativeKey(t *testing.T) {
	clean := sessionTranscriptGetResponse{
		ID:            "s1",
		Template:      "Chat",
		Provider:      "claude",
		Format:        "structured",
		SchemaVersion: sessionStructuredSchemaVersion,
		StructuredMessages: structuredMessagesField([]SessionStructuredMessage{{
			ID:     "m1",
			Role:   "assistant",
			Status: "final",
			Blocks: []SessionStructuredBlock{{
				Type:       "tool_result",
				ToolCallID: "call-1",
				Structured: &SessionStructuredToolResult{Kind: "edit", FilePath: "a.go", Patch: "@@ -1 +1 @@"},
			}},
		}}),
	}
	wire, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("marshal clean response: %v", err)
	}

	// Baseline: a real typed response passes both gates.
	assertNoStructuredWireLeak(t, wire)

	// A known provider-native key must be caught by BOTH gates.
	if leaked := worker.ScanForbiddenTokens(injectWireKey(t, wire, "toolUseResult")); len(leaked) == 0 {
		t.Fatal("denylist gate failed to catch injected toolUseResult")
	}
	if unexpected, _ := worker.UnexpectedWireKeys(injectWireKey(t, wire, "toolUseResult"), structuredTranscriptWireAllowedKeys()); len(unexpected) == 0 {
		t.Fatal("allowlist gate failed to catch injected toolUseResult")
	}

	// A novel native key the denylist has never seen must still be caught by
	// the allowlist gate — this is the future-proofing the denylist cannot give.
	novel := injectWireKey(t, wire, "someBrandNewProviderKey")
	if leaked := worker.ScanForbiddenTokens(novel); len(leaked) > 0 {
		t.Fatalf("denylist unexpectedly matched a novel key: %v", leaked)
	}
	unexpected, err := worker.UnexpectedWireKeys(novel, structuredTranscriptWireAllowedKeys())
	if err != nil {
		t.Fatalf("scan novel wire: %v", err)
	}
	if len(unexpected) != 1 || unexpected[0] != "someBrandNewProviderKey" {
		t.Fatalf("allowlist gate must catch a novel non-schema key, got %v", unexpected)
	}
}

// TestStructuredHistoryWireHidesServerPathAndGeneration pins the value-level
// contract Finding 3 raised: the structured history envelope must never emit the
// absolute server transcript path or the raw mtime:size generation data. The
// key-level allowlist gate cannot catch this because transcript_stream_id and
// generation are legitimate keys — only their VALUES leak.
func TestStructuredHistoryWireHidesServerPathAndGeneration(t *testing.T) {
	rawPath := "/home/ubuntu/.claude/projects/-data-projects-secret/9f1c2d3e-uuid.jsonl"
	snapshot := &worker.HistorySnapshot{
		GCSessionID:           "gc-session-1",
		LogicalConversationID: "logical-1",
		ProviderSessionID:     "provider-uuid-1",
		TranscriptStreamID:    rawPath,
		Generation:            worker.Generation{ID: "1749123456789012345:20481", ObservedAt: time.Date(2026, 6, 1, 2, 3, 4, 0, time.UTC)},
		Cursor:                worker.Cursor{AfterEntryID: "entry-1"},
		Continuity:            worker.Continuity{Status: worker.ContinuityStatusContinuous},
		TailState:             worker.TailState{Activity: worker.TailActivityIdle, LastEntryID: "entry-1"},
	}
	history := structuredHistoryFromSnapshot(snapshot)
	if history == nil {
		t.Fatal("structuredHistoryFromSnapshot returned nil")
	}
	wire, err := json.Marshal(history)
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}

	// The absolute path, its directory segments, and the raw mtime:size must not
	// appear anywhere on the wire.
	for _, secret := range []string{rawPath, "/home/ubuntu", ".claude/projects", "-data-projects-secret", "20481", "1749123456789012345"} {
		if bytes.Contains(wire, []byte(secret)) {
			t.Fatalf("structured history wire leaked server data %q: %s", secret, wire)
		}
	}
	// transcript_stream_id is an opaque, path-free identity: non-empty, not the
	// raw path, and carrying no filesystem separator.
	if history.TranscriptStreamID == "" || history.TranscriptStreamID == rawPath || strings.ContainsAny(history.TranscriptStreamID, `/\`) {
		t.Fatalf("transcript_stream_id is not an opaque identity: %q", history.TranscriptStreamID)
	}
	// generation carries no raw mtime:size and no observed_at timestamp.
	if history.Generation.ObservedAt != "" {
		t.Fatalf("generation.observed_at leaked file mtime: %q", history.Generation.ObservedAt)
	}
	if history.Generation.ID == snapshot.Generation.ID || rawGenerationTokenPattern.MatchString(history.Generation.ID) {
		t.Fatalf("generation.id still carries raw mtime:size: %q", history.Generation.ID)
	}
	// The reusable leak gate now also bites on an enveloped path/mtime value.
	if leaked, _ := structuredWireLeakage(wire); len(leaked) != 0 {
		t.Fatalf("sanitized history still flagged by leak gate: %v", leaked)
	}

	// Opaque identity is deterministic for a given stream and rotation-sensitive.
	if again := structuredHistoryFromSnapshot(snapshot); again.TranscriptStreamID != history.TranscriptStreamID || again.Generation.ID != history.Generation.ID {
		t.Fatal("opaque identity is not deterministic for the same stream")
	}
	rotated := *snapshot
	rotated.TranscriptStreamID = rawPath + ".rotated"
	if structuredHistoryFromSnapshot(&rotated).TranscriptStreamID == history.TranscriptStreamID {
		t.Fatal("transcript_stream_id did not change across transcript rotation")
	}
}

// TestStructuredHistoryEnvelopeLeakGateCatchesRawPathAndGeneration proves the
// envelope value-leak gate actually bites, so the sanitization above cannot
// silently regress without a test failing.
func TestStructuredHistoryEnvelopeLeakGateCatchesRawPathAndGeneration(t *testing.T) {
	leakyWire := []byte(`{"history":{"transcript_stream_id":"/home/ubuntu/.claude/x.jsonl","generation":{"id":"1749123456789012345:20481","observed_at":"2026-06-01T02:03:04Z"}}}`)
	leaked, err := structuredWireLeakage(leakyWire)
	if err != nil {
		t.Fatalf("scan leaky wire: %v", err)
	}
	for _, want := range []string{"history.transcript_stream_id:server_path", "history.generation.id:raw_mtime_size", "history.generation.observed_at:file_mtime"} {
		if !stringSliceContainsSubstring(leaked, want) {
			t.Fatalf("envelope leak gate missed %q, got %v", want, leaked)
		}
	}
}

func TestStructuredLeakageScanRejectsGenericArgumentCarriers(t *testing.T) {
	tests := []struct {
		name     string
		argument SessionStructuredArgument
		wantLeak string
	}{
		{
			name:     "unknown input argument name",
			argument: SessionStructuredArgument{Name: "scope", Value: "web"},
			wantLeak: "arguments[0].name",
		},
		{
			name:     "encoded object value",
			argument: SessionStructuredArgument{Name: "path", Value: `{"query":"provider-owned"}`},
			wantLeak: "arguments[0].value:json_object",
		},
		{
			name:     "encoded array value",
			argument: SessionStructuredArgument{Name: "path", Value: `["provider-owned"]`},
			wantLeak: "arguments[0].value:json_array",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := structuredLeakageTestResponse(SessionStructuredBlock{
				Type: "tool_use",
				ID:   "call-1",
				Input: &SessionStructuredToolInput{
					Kind:      "arguments",
					Arguments: []SessionStructuredArgument{tt.argument},
				},
			})
			wire, err := json.Marshal(response)
			if err != nil {
				t.Fatalf("marshal structured response: %v", err)
			}
			leaked, err := structuredWireLeakage(wire)
			if err != nil {
				t.Fatalf("scan structured response: %v", err)
			}
			if !stringSliceContainsSubstring(leaked, tt.wantLeak) {
				t.Fatalf("structuredWireLeakage() = %v, want leak containing %q", leaked, tt.wantLeak)
			}
		})
	}
}

func TestStructuredLeakageScanAllowsTypedJSONText(t *testing.T) {
	providerLookingText := `{"toolUseResult":{"source":"user-authored","type":"example"}}`
	response := structuredLeakageTestResponse(
		SessionStructuredBlock{Type: "text", Text: providerLookingText},
		SessionStructuredBlock{
			Type: "tool_use",
			ID:   "call-1",
			Input: &SessionStructuredToolInput{
				Kind:    "code",
				Code:    providerLookingText,
				Command: providerLookingText,
				Text:    providerLookingText,
			},
		},
		SessionStructuredBlock{
			Type:       "tool_result",
			ToolCallID: "call-1",
			Content:    providerLookingText,
			Structured: &SessionStructuredToolResult{
				Kind:   "bash",
				Text:   providerLookingText,
				Stdout: providerLookingText,
			},
		},
	)
	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal structured response: %v", err)
	}
	leaked, err := structuredWireLeakage(wire)
	if err != nil {
		t.Fatalf("scan structured response: %v", err)
	}
	if leaked != nil {
		t.Fatalf("legitimate typed JSON text flagged as provider leakage: %v", leaked)
	}
}

func TestStructuredRawResponsePreservesProviderNativeFrame(t *testing.T) {
	raw := json.RawMessage(`{"timestamp":9007199254740993,"type":"response_item","payload":{"action":{"type":"search","source":"web"},"scope":"web"}}`)
	response := sessionTranscriptGetResponse{
		ID:       "s1",
		Template: "Chat",
		Provider: "codex",
		Format:   "raw",
		Messages: rawMessagesField([]SessionRawMessageFrame{{Raw: raw}}),
	}
	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal raw response: %v", err)
	}
	var envelope struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatalf("decode raw response envelope: %v", err)
	}
	if len(envelope.Messages) != 1 || !bytes.Equal(envelope.Messages[0], raw) {
		t.Fatalf("raw frame = %s, want exact provider frame %s", envelope.Messages, raw)
	}
}

func TestStructuredCodexWebSearchOmitsNativeInputAndRawPreservesIt(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Chat",
		Command:  "codex",
		WorkDir:  workDir,
		Provider: "codex",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		Hints:     runtime.Config{},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	writeStructuredCodexWebSearchFixture(t, searchBase, info.WorkDir, info.SessionKey)

	structuredRecorder := httptest.NewRecorder()
	structuredRequest := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
	h.ServeHTTP(structuredRecorder, structuredRequest)
	if structuredRecorder.Code != http.StatusOK {
		t.Fatalf("structured status = %d, want %d; body: %s", structuredRecorder.Code, http.StatusOK, structuredRecorder.Body.String())
	}
	var structured sessionTranscriptGetResponse
	if err := json.Unmarshal(structuredRecorder.Body.Bytes(), &structured); err != nil {
		t.Fatalf("decode structured response: %v", err)
	}
	toolUse, _ := findStructuredToolPair(structuredTranscriptMessages(structured), "call-codex-web-search")
	if toolUse == nil || toolUse.Input == nil {
		t.Fatalf("structured response missing web-search input: %+v", structuredTranscriptMessages(structured))
	}
	if toolUse.Input.Kind != "search" || toolUse.Input.Query != "structured tool result formats" {
		t.Fatalf("structured input = %+v, want neutral search query", toolUse.Input)
	}
	if toolUse.Input.Text != "" || len(toolUse.Input.Arguments) != 0 {
		t.Fatalf("structured input leaked fallback carriers: %+v", toolUse.Input)
	}
	assertNoStructuredWireLeak(t, structuredRecorder.Body.Bytes())
	for _, native := range []string{`"action"`, `"scope"`, `"source"`} {
		if bytes.Contains(structuredRecorder.Body.Bytes(), []byte(native)) {
			t.Fatalf("structured response leaked native field %s: %s", native, structuredRecorder.Body.Bytes())
		}
	}

	rawRecorder := httptest.NewRecorder()
	rawRequest := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=raw&tail=0", nil)
	h.ServeHTTP(rawRecorder, rawRequest)
	if rawRecorder.Code != http.StatusOK {
		t.Fatalf("raw status = %d, want %d; body: %s", rawRecorder.Code, http.StatusOK, rawRecorder.Body.String())
	}
	var rawResponse sessionTranscriptGetResponse
	if err := json.Unmarshal(rawRecorder.Body.Bytes(), &rawResponse); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	wantRaw := []byte(`{"timestamp":"2026-06-01T00:04:01Z","type":"response_item","payload":{"type":"web_search_call","id":"call-codex-web-search","query":"structured tool result formats","input":{"query":"ignored fallback","scope":"web"},"action":{"type":"search","source":"web"}}}`)
	foundExact := false
	for _, frame := range rawTranscriptMessages(rawResponse) {
		if bytes.Equal(frame.Raw, wantRaw) {
			foundExact = true
			break
		}
	}
	if !foundExact {
		t.Fatalf("raw response did not preserve exact provider web-search frame: %s", rawRecorder.Body.Bytes())
	}
}

func structuredLeakageTestResponse(blocks ...SessionStructuredBlock) sessionTranscriptGetResponse {
	return sessionTranscriptGetResponse{
		ID:                 "s1",
		Template:           "Chat",
		Provider:           "codex",
		Format:             "structured",
		SchemaVersion:      sessionStructuredSchemaVersion,
		Operation:          "snapshot",
		StructuredMessages: structuredMessagesField([]SessionStructuredMessage{representativeStructuredMessage(blocks...)}),
	}
}

func representativeStructuredMessage(blocks ...SessionStructuredBlock) SessionStructuredMessage {
	return SessionStructuredMessage{
		ID:     "m1",
		Role:   "assistant",
		Status: "final",
		Blocks: blocks,
	}
}

func stringSliceContainsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

// injectWireKey decodes the wire, adds key (with a sentinel value) to the first
// structured block, and re-encodes it — simulating a provider-native key
// leaking into the structured projection.
func injectWireKey(t *testing.T, wire []byte, key string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(wire, &doc); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	messages, ok := doc["structured_messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("wire has no structured_messages to inject into: %s", wire)
	}
	message := messages[0].(map[string]any)
	blocks := message["blocks"].([]any)
	block := blocks[0].(map[string]any)
	block[key] = "leak"
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal injected wire: %v", err)
	}
	return out
}
