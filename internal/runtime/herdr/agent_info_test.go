package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// herdr's agent object carries two identifiers: "name", the registered agent
// name (omitted when nobody registered one), and "agent", the detected kind
// every pane of that kind shares. Only "name" decodes as the identity.
func TestAgentInfoDecodesNameAcrossHerdrVersions(t *testing.T) {
	for _, tc := range []struct {
		shape, body string
		want        []agentInfo
	}{
		{
			"named", `[{"agent":"claude","name":"mayor","pane_id":"w1:p1"},{"agent":"claude","name":"worker-1","pane_id":"w1:p2"}]`,
			[]agentInfo{{Name: "mayor", Kind: "claude", PaneID: "w1:p1"}, {Name: "worker-1", Kind: "claude", PaneID: "w1:p2"}},
		},
		{
			"nameless", `[{"agent":"claude","pane_id":"w1:p3"}]`,
			[]agentInfo{{Name: "", Kind: "claude", PaneID: "w1:p3"}},
		},
	} {
		var got []agentInfo
		if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
			t.Fatalf("%s: %v", tc.shape, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: decoded %+v; want %+v", tc.shape, got, tc.want)
		}
	}
}

// TestAgentInfoNamelessEntryDecodesUnnamed decodes herdr 0.9.1-shaped JSON
// through every channel gc reads agents from: `agent list`, the `agent get`
// wrapper, and the socket's agent.list.
func TestAgentInfoNamelessEntryDecodesUnnamed(t *testing.T) {
	const (
		named   = `{"agent":"opencode","name":"alpha","pane_id":"w1:p1"}`
		unnamed = `{"agent":"claude","pane_id":"w1:p3"}`
	)
	want := []agentInfo{{Name: "alpha", Kind: "opencode", PaneID: "w1:p1"}, {Kind: "claude", PaneID: "w1:p3"}}
	ctx := context.Background()

	bin, _ := agentLookupFake(t, "", named+","+unnamed)
	got, err := (&client{session: "gc-test", bin: bin}).listAgents(ctx)
	if err != nil {
		t.Fatalf("listAgents: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("agent list decoded %+v; want %+v", got, want)
	}

	for i, entry := range []string{named, unnamed} {
		bin, _ := agentLookupFake(t, `{"result":{"agent":`+entry+`}}`, "")
		a, ok, err := (&client{session: "gc-test", bin: bin}).getAgent(ctx, want[i].PaneID)
		if err != nil || !ok {
			t.Fatalf("getAgent(%q) = (%+v, %v, %v)", want[i].PaneID, a, ok, err)
		}
		if a != want[i] {
			t.Errorf("agent get decoded %+v; want %+v", a, want[i])
		}
	}

	f, sock := newFakeHerdrServer(t)
	f.setRawAgents(named + "," + unnamed)
	got, err = eventTestProvider(t, sock).c.sockAgentList(ctx)
	if err != nil {
		t.Fatalf("sockAgentList: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("socket agent.list decoded %+v; want %+v", got, want)
	}
}

// agentLookupFake writes a fake herdr whose `agent get` prints getAnswer, or
// herdr's not-found envelope when getAnswer is empty, and whose `agent list`
// returns entries, a comma-separated list of literal herdr agent objects. Every
// invocation's argv is appended to the returned trace path.
func agentLookupFake(t *testing.T, getAnswer, entries string) (bin, trace string) {
	t.Helper()
	if getAnswer == "" {
		getAnswer = `{"error":{"code":"agent_not_found","message":"agent not found"}}`
	}
	trace = filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + trace + "\n" +
		"case \"$*\" in\n" +
		"  *'agent get'*)\n" +
		"    echo '" + getAnswer + "'\n" +
		"    ;;\n" +
		"  *'agent list'*)\n" +
		"    echo '{\"result\":{\"agents\":[" + entries + "]}}'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	return writeFakeHerdr(t, script), trace
}

// herdr's "agent" key is the detected kind every pane of that kind shares, so
// neither a kind nor an empty target may resolve to a pane: only a registered
// name is an identity. An empty target never reaches herdr at all.
func TestGetAgentNeverResolvesAKindOrEmptyTarget(t *testing.T) {
	bin, trace := agentLookupFake(t, "",
		`{"agent":"opencode","pane_id":"w3:p1","agent_status":"working"},`+
			`{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"working"},`+
			`{"agent":"opencode","name":"beta","pane_id":"w2:p1","agent_status":"working"}`)
	c := &client{session: "gc-test", bin: bin}
	ctx := context.Background()

	if a, ok, err := c.getAgent(ctx, "opencode"); err != nil || ok || a.PaneID != "" {
		t.Errorf("getAgent(%q) = (%+v, %v, %v); want not-found with no pane", "opencode", a, ok, err)
	}
	if a, ok, err := c.getAgent(ctx, "alpha"); err != nil || !ok || a.PaneID != "w1:p1" {
		t.Errorf("getAgent(%q) = (%+v, %v, %v); want pane w1:p1", "alpha", a, ok, err)
	}

	if err := os.Remove(trace); err != nil {
		t.Fatalf("resetting the call trace: %v", err)
	}
	if a, ok, err := c.getAgent(ctx, ""); err != nil || ok || a.PaneID != "" {
		t.Errorf("getAgent(\"\") = (%+v, %v, %v); want not-found with no pane", a, ok, err)
	}
	if b, err := os.ReadFile(trace); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("getAgent(\"\") called herdr (trace err %v):\n%s", err, b)
	}
}
