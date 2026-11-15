package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

func TestInvocationUsageFamilyOpenCode(t *testing.T) {
	for _, provider := range []string{"opencode", "groq"} {
		family, ok := InvocationUsageFamily(provider)
		if family != "opencode" || !ok {
			t.Errorf("InvocationUsageFamily(%q) = %q, %v; want opencode, true", provider, family, ok)
		}
	}
	// A provider whose name merely contains claude keeps resolving to claude:
	// the claude check runs first and OpenCode must not shadow it.
	for _, provider := range []string{"claude-eco", "opencode-claude"} {
		if family, _ := InvocationUsageFamily(provider); family != "claude" {
			t.Errorf("%s = %q, want claude", provider, family)
		}
	}
	if _, ok := invocationUsageSpecs["opencode"]; !ok {
		t.Fatal("opencode spec missing from invocationUsageSpecs")
	}
}

func openCodeExportJSON(sessionID string, messages ...string) string {
	body := ""
	for i, m := range messages {
		if i > 0 {
			body += ","
		}
		body += m
	}
	return fmt.Sprintf(`{"info":{"id":%q,"directory":"/shared"},"messages":[%s]}`, sessionID, body)
}

func openCodeAssistantJSON(id, model string, input, output int, cost string, completed int64) string {
	return fmt.Sprintf(`{"info":{"id":%q,"role":"assistant","modelID":%q,"providerID":"openrouter",%s`+
		`"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}},`+
		`"time":{"created":%d,"completed":%d}},"parts":[]}`,
		id, model, cost, input, output, completed-1000, completed)
}

// TestFactorySweepSessionModelUsageOpenCodeSharedWorkdir sweeps two OpenCode
// sessions that share one working directory. Each must be attributed only its
// own mirrored export (matched by session_key), never the newest export in the
// shared workdir, and the provider-reported cost is used where the price table
// has no entry for the model.
func TestFactorySweepSessionModelUsageOpenCodeSharedWorkdir(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	factory, err := NewFactory(FactoryConfig{
		CityPath:    t.TempDir(),
		Store:       store,
		Provider:    runtime.NewFake(),
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	newSession := func(key string) (string, map[string]string) {
		t.Helper()
		h, err := factory.Session(SessionSpec{
			Profile:  ProfileOpenCodeTmuxCLI,
			Template: "probe",
			Title:    "Probe " + key,
			Command:  "opencode",
			WorkDir:  workDir,
			Provider: "opencode",
			Metadata: map[string]string{"agent_name": "myrig/" + key},
		})
		if err != nil {
			t.Fatalf("Session: %v", err)
		}
		if err := h.Start(context.Background(), handleActor(t, h, sessionpkg.ActorAgent)); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := store.SetMetadata(h.sessionID, "session_key", key); err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadata(h.sessionID, "molecule_id", "run-"+key); err != nil {
			t.Fatal(err)
		}
		b, err := store.Get(h.sessionID)
		if err != nil {
			t.Fatal(err)
		}
		return h.sessionID, b.Metadata
	}
	idA, metaA := newSession("ses_aaa")
	idB, metaB := newSession("ses_bbb")

	write := func(key, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(searchBase, key+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ses_aaa", openCodeExportJSON("ses_aaa",
		openCodeAssistantJSON("msg_a1", "deepseek/deepseek-v4.1-flash", 1000, 10, `"cost":0.5,`, 1792100000000),
		openCodeAssistantJSON("msg_a2", "deepseek/deepseek-v4.1-flash", 2000, 20, `"cost":0.25,`, 1792100100000)))
	// B is written last, so it is the newest export in the shared workdir.
	write("ses_bbb", openCodeExportJSON("ses_bbb",
		openCodeAssistantJSON("msg_b1", "unlisted/model", 7, 3, ``, 1792100200000)))

	now := time.Unix(1, 0).UTC()
	if emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), idA, metaA, now); err != nil || !settled || emitted != 2 {
		t.Fatalf("sweep A = %d, %v, %v; want 2, true, nil", emitted, settled, err)
	}
	if emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), idB, metaB, now); err != nil || !settled || emitted != 1 {
		t.Fatalf("sweep B = %d, %v, %v; want 1, true, nil", emitted, settled, err)
	}

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("ReadFacts: %v %v", err, warnings)
	}
	byReq := map[string]usage.Fact{}
	for _, f := range facts {
		byReq[f.UpstreamReqID] = f
	}
	if len(byReq) != 3 {
		t.Fatalf("want 3 distinct model facts, got %d: %+v", len(byReq), facts)
	}
	for req, wantRun := range map[string]string{"msg_a1": "run-ses_aaa", "msg_a2": "run-ses_aaa", "msg_b1": "run-ses_bbb"} {
		f := byReq[req]
		if f.Kind != usage.KindModel || f.Provider != "opencode" || f.RunID != wantRun {
			t.Errorf("%s: kind/provider/run = %q/%q/%q, want model/opencode/%s", req, f.Kind, f.Provider, f.RunID, wantRun)
		}
	}
	if f := byReq["msg_a1"]; f.Unpriced || f.CostUSDEstimate != 0.5 || f.InputTokens != 1000 || f.OutputTokens != 10 {
		t.Errorf("msg_a1 = %+v, want priced from the reported cost 0.5", f)
	}
	// No reported cost and no price-table entry: tokens are kept, cost flagged not zero-filled.
	if f := byReq["msg_b1"]; !f.Unpriced || f.CostUSDEstimate != 0 || f.InputTokens != 7 {
		t.Errorf("msg_b1 = %+v, want unpriced with tokens kept", f)
	}

	refreshed, err := store.Get(idA)
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "msg_a2" {
		t.Errorf("cursor = %q, want msg_a2", got)
	}
	if emitted, _, err := factory.SweepSessionModelUsage(context.Background(), idA, refreshed.Metadata, now); err != nil || emitted != 0 {
		t.Errorf("re-sweep = %d, %v; want 0, nil", emitted, err)
	}
}

// TestFactorySweepSessionModelUsageOpenCodeKeyedMissNoWorkdirFallback sweeps a
// lone OpenCode session whose own export is not mirrored yet while another
// conversation's export for the same workdir sits in the search root. The keyed
// miss must record nothing and leave the cursor alone rather than fall back to
// that stray export; once the session's own export lands, its facts are emitted.
func TestFactorySweepSessionModelUsageOpenCodeKeyedMissNoWorkdirFallback(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	factory, err := NewFactory(FactoryConfig{
		CityPath:    t.TempDir(),
		Store:       store,
		Provider:    runtime.NewFake(),
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	h, err := factory.Session(SessionSpec{
		Profile:  ProfileOpenCodeTmuxCLI,
		Template: "probe",
		Title:    "Probe ses_ccc",
		Command:  "opencode",
		WorkDir:  workDir,
		Provider: "opencode",
		Metadata: map[string]string{"agent_name": "myrig/ses_ccc"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background(), handleActor(t, h, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.SetMetadata(h.sessionID, "session_key", "ses_ccc"); err != nil {
		t.Fatal(err)
	}
	id := h.sessionID
	meta := func() map[string]string {
		t.Helper()
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return b.Metadata
	}

	write := func(key, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(searchBase, key+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The stray export records the session's own workdir, so a workdir fallback
	// would resolve it.
	write("ses_stray", fmt.Sprintf(`{"info":{"id":"ses_stray","directory":%q},"messages":[%s]}`, workDir,
		openCodeAssistantJSON("msg_stray", "deepseek/deepseek-v4.1-flash", 500, 5, `"cost":0.1,`, 1792100000000)))

	now := time.Unix(1, 0).UTC()
	if emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), id, meta(), now); err != nil || settled || emitted != 0 {
		t.Fatalf("keyed-miss sweep = %d, %v, %v; want 0, false, nil", emitted, settled, err)
	}
	if got := meta()[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "" {
		t.Fatalf("cursor advanced to %q on a keyed miss", got)
	}
	if facts, _, err := usage.ReadFacts(sinkPath); err == nil && len(facts) != 0 {
		t.Fatalf("keyed miss emitted facts: %+v", facts)
	}

	write("ses_ccc", openCodeExportJSON("ses_ccc",
		openCodeAssistantJSON("msg_c1", "deepseek/deepseek-v4.1-flash", 1000, 10, `"cost":0.5,`, 1792100100000)))
	if emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), id, meta(), now); err != nil || !settled || emitted != 1 {
		t.Fatalf("keyed-hit sweep = %d, %v, %v; want 1, true, nil", emitted, settled, err)
	}
	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(facts) != 1 || facts[0].UpstreamReqID != "msg_c1" {
		t.Fatalf("facts = %+v, want only msg_c1", facts)
	}
	if got := meta()[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "msg_c1" {
		t.Errorf("cursor = %q, want msg_c1", got)
	}
}

func TestInvocationUsageFamilyCopilot(t *testing.T) {
	for _, provider := range []string{"copilot", "github-copilot", "copilot/tmux-cli"} {
		if family, ok := InvocationUsageFamily(provider); family != "copilot" || !ok {
			t.Fatalf("%s = %s,%v", provider, family, ok)
		}
	}
}

// bindCopilotTranscript stands in for the hook's capture: copilot_transcript is
// a CAS-only key, so a fixture writes it by a conditional verb, scoped to the
// generation the row is at, exactly as the capture does.
func bindCopilotTranscript(t *testing.T, store *beads.MemStore, id, generation, conversation string) {
	t.Helper()
	if err := store.SetMetadata(id, "generation", generation); err != nil {
		t.Fatal(err)
	}
	row, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	value := ""
	if conversation != "" {
		value = sessionpkg.CopilotTranscriptRef{Generation: mustParseGeneration(t, generation), ConversationID: conversation}.String()
	}
	if err := store.UpdateIfMatch(id, row.Revision, beads.UpdateOpts{Metadata: map[string]string{sessionpkg.MetadataKeyCopilotTranscript: value}}); err != nil {
		t.Fatal(err)
	}
}

func mustParseGeneration(t *testing.T, generation string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(generation, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFactorySweepSessionModelUsageCopilotSharedWorkdir(t *testing.T) {
	root, workDir := t.TempDir(), t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")
	store := beads.NewMemStore()
	factory, err := NewFactory(FactoryConfig{CityPath: t.TempDir(), Store: store, Provider: runtime.NewFake(), SearchPaths: []string{root}, UsageSink: usage.NewLocalSink(sinkPath)})
	if err != nil {
		t.Fatal(err)
	}
	newSession := func(conversation string) *SessionHandle {
		t.Helper()
		h, err := factory.Session(SessionSpec{Profile: Profile("copilot/tmux-cli"), Template: "probe", Title: conversation, Command: "copilot", WorkDir: workDir, Provider: "copilot"})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Start(context.Background(), handleActor(t, h, sessionpkg.ActorAgent)); err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadata(h.sessionID, "molecule_id", "run-"+conversation); err != nil {
			t.Fatal(err)
		}
		bindCopilotTranscript(t, store, h.sessionID, "1", conversation)
		return h
	}
	a, b := newSession("seat-a"), newSession("seat-b")
	write := func(conversation, calls string) {
		t.Helper()
		dir := filepath.Join(root, conversation)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		start := fmt.Sprintf(`{"type":"session.start","data":{"sessionId":%q,"context":{"cwd":%q}}}`+"\n", conversation, workDir)
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(start+calls), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	call := func(id string) string {
		return fmt.Sprintf(`{"type":"session.usage_record","id":%q,"timestamp":"2026-11-10T21:22:22.735Z","data":{"usage":{"model":"fixture-unpriced-model","inputTokens":100,"outputTokens":10,"cacheReadTokens":30,"cacheWriteTokens":20,"cost":15,"apiCallId":%q}}}`+"\n", "event-"+id, id)
	}
	sweep := func(h *SessionHandle) int {
		t.Helper()
		row, err := store.Get(h.sessionID)
		if err != nil {
			t.Fatal(err)
		}
		emitted, _, err := factory.SweepSessionModelUsage(context.Background(), row.ID, row.Metadata, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return emitted
	}
	factCount := func() int {
		t.Helper()
		facts, warnings, err := usage.ReadFacts(sinkPath)
		if err != nil || len(warnings) != 0 {
			t.Fatalf("facts = %v,%v", warnings, err)
		}
		return len(facts)
	}
	write("seat-a", call("call-a1"))
	write("seat-b", call("call-b1"))
	// Prompt-operation accounting records A's latest completed invocation.
	a.recordInvocationTelemetry(context.Background())
	write("seat-a", call("call-a1")+call("call-a2")+call("call-a2"))
	if got := sweep(a); got != 1 {
		t.Fatalf("seat A sweep = %d, want its one new call", got)
	}
	if got := sweep(b); got != 1 {
		t.Fatalf("seat B sweep = %d, want its one call", got)
	}
	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil || len(warnings) != 0 || len(facts) != 1+2 {
		t.Fatalf("facts = %+v,%v,%v", facts, warnings, err)
	}
	for _, fact := range facts {
		wantID := a.sessionID
		wantRun := "run-seat-a"
		if fact.UpstreamReqID == "call-b1" {
			wantID = b.sessionID
			wantRun = "run-seat-b"
		}
		if fact.SessionID != wantID || fact.RunID != wantRun || fact.Provider != "copilot" || fact.InputTokens != 50 || fact.OutputTokens != 10 || fact.CacheReadTokens != 30 || fact.CacheCreationTokens != 20 || fact.CostUSDEstimate != 0 || !fact.Unpriced {
			t.Fatalf("attribution = %+v", fact)
		}
	}
	row, _ := store.Get(a.sessionID)
	if row.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor] != "call-a2" {
		t.Fatal("cursor did not advance")
	}
	// Repeated terminal sweeps do not emit previously recorded calls.
	if got := sweep(a); got != 0 {
		t.Fatalf("repeat sweep = %d", got)
	}
	if row.Metadata["session_key"] != "" {
		t.Fatalf("session_key = %q; usage attribution must not give a Copilot row a launch key", row.Metadata["session_key"])
	}

	// A fresh incarnation (generation 2) must not read the conversation the
	// earlier one captured, even though that log keeps growing in the shared
	// work directory.
	write("seat-a", call("call-a1")+call("call-a2")+call("call-a3"))
	if err := store.SetMetadata(a.sessionID, "generation", "2"); err != nil {
		t.Fatal(err)
	}
	if got := sweep(a); got != 0 {
		t.Fatalf("stale identity swept %d calls from the earlier conversation", got)
	}
	a.recordInvocationTelemetry(context.Background())
	if got := factCount(); got != 3 {
		t.Fatalf("stale identity recorded a prompt-operation fact: %d facts", got)
	}
	// Once the new incarnation captures its own conversation, only that log counts.
	write("seat-a-2", call("call-n1"))
	bindCopilotTranscript(t, store, a.sessionID, "2", "seat-a-2")
	if got := sweep(a); got != 1 {
		t.Fatalf("new conversation sweep = %d, want its one call", got)
	}
	// No identity and an identity with no log must never borrow either same-cwd log.
	for _, conversation := range []string{"missing", ""} {
		bindCopilotTranscript(t, store, a.sessionID, "2", conversation)
		if got := sweep(a); got != 0 {
			t.Fatalf("identity %q swept %d calls", conversation, got)
		}
	}
	if got := factCount(); got != 4 {
		t.Fatalf("facts = %d, want the three earlier ones and the new conversation's call", got)
	}
}
