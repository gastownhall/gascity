package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
