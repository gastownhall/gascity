package materialize

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func notesProjection(t *testing.T, provider, root string) MCPProjection {
	t.Helper()
	proj, err := BuildMCPProjection(provider, root, []MCPServer{{Name: "notes", Transport: MCPTransportStdio, Command: "uvx"}})
	if err != nil {
		t.Fatalf("BuildMCPProjection: %v", err)
	}
	return proj
}

func writeTarget(t *testing.T, proj MCPProjection, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(proj.Target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proj.Target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertRefusedAndUntouched applies proj, which must be refused, and checks
// that nothing under root changed and that Plan reports the same refusal.
func assertRefusedAndUntouched(t *testing.T, proj MCPProjection, root string) *MCPAdoptionRefusedError {
	t.Helper()
	before := treeSnapshot(t, root)
	err := proj.Apply(fsys.OSFS{})
	var refused *MCPAdoptionRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Apply error = %v, want *MCPAdoptionRefusedError", err)
	}
	if refused.Target != proj.Target || refused.Provider != proj.Provider {
		t.Fatalf("refusal = %+v, want target %s (%s)", refused, proj.Target, proj.Provider)
	}
	if after := treeSnapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused adoption wrote to the filesystem:\nbefore: %v\nafter:  %v", before, after)
	}
	plan, err := proj.Plan(fsys.OSFS{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Refused == nil || plan.Refused.Error() != refused.Error() || plan.Adopt || plan.Action != MCPPlanNone {
		t.Fatalf("plan = %+v, want the refusal %q and no action", plan, refused)
	}
	direct, err := proj.AdoptionRefusal(fsys.OSFS{})
	if err != nil || direct == nil || direct.Error() != refused.Error() {
		t.Fatalf("AdoptionRefusal = %v, %v; want %q", direct, err, refused)
	}
	return refused
}

// A file gc does not manage that holds a server of the user's own is left as
// it is: adopting it would replace that server with gc's.
func TestApplyMCPProjectionRefusesToAdoptAFileWithServersGCDoesNotManage(t *testing.T) {
	for provider, content := range teamProviderConfig {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			proj := notesProjection(t, provider, root)
			writeTarget(t, proj, content)

			refused := assertRefusedAndUntouched(t, proj, root)
			if !reflect.DeepEqual(refused.Servers, []string{"team"}) || refused.Redefined != nil || refused.Keys != nil {
				t.Fatalf("refusal = %+v, want only server team", refused)
			}
			if want := "not adopting " + proj.Target + `: gc does not manage it yet, and adopting it would drop MCP server "team"`; refused.Error() != want {
				t.Fatalf("Error() = %q\nwant      %q", refused.Error(), want)
			}
			if !strings.Contains(refused.Remedy(), "gc's MCP catalog (an mcp/ directory)") {
				t.Fatalf("Remedy() = %q", refused.Remedy())
			}
		})
	}
}

// A server the file defines under a name the catalog also uses, but
// differently, is the file's own: adopting the file would replace that
// definition with the catalog's, so gc leaves the file alone.
func TestApplyMCPProjectionRefusesToReplaceAServerTheFileDefinesDifferently(t *testing.T) {
	redefined := map[string]string{
		MCPProviderClaude:   `{"mcpServers":{"notes":{"command":"team-notes","env":{"TOKEN":"${NOTES_TOKEN}"}}}}`,
		MCPProviderCodex:    "[mcp_servers.notes]\ncommand = \"team-notes\"\n",
		MCPProviderGemini:   `{"mcpServers":{"notes":{"command":"uvx","args":["--from","team-notes"]}}}`,
		MCPProviderOpenCode: `{"mcp":{"notes":{"type":"local","command":["uvx"],"enabled":false}}}`,
		MCPProviderMimoCode: `{"mcp":{"notes":{"type":"remote","url":"https://notes.example.test"}}}`,
		MCPProviderCursor:   `{"mcpServers":{"notes":{"command":"uvx","env":{"DEBUG":""}}}}`,
	}
	for provider, content := range redefined {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			proj := notesProjection(t, provider, root)
			writeTarget(t, proj, content)

			refused := assertRefusedAndUntouched(t, proj, root)
			if refused.Servers != nil || !reflect.DeepEqual(refused.Redefined, []string{"notes"}) || refused.Keys != nil {
				t.Fatalf("refusal = %+v, want only the redefined server notes", refused)
			}
			if !strings.Contains(refused.Error(), `adopting it would drop its own definition of MCP server "notes"`) {
				t.Fatalf("Error() = %q", refused.Error())
			}
			if want := `to let gc manage it, make the catalog's definition of MCP server "notes" match the file's, or remove it from the file`; refused.Remedy() != want {
				t.Fatalf("Remedy() = %q\nwant      %q", refused.Remedy(), want)
			}
		})
	}
}

// Claude's .mcp.json is gc's whole: any other top-level key would be lost.
// An MCP key that holds something other than a table of servers would be
// replaced. Neither is a server, so the remedy is only to remove them.
func TestApplyMCPProjectionRefusesKeysItWouldDrop(t *testing.T) {
	for provider, content := range map[string]string{
		MCPProviderClaude: `{"mcpServers":{"notes":{"command":"uvx"}},"inputs":[]}`,
		MCPProviderGemini: `{"mcpServers":["notes"]}`,
	} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			proj := notesProjection(t, provider, root)
			writeTarget(t, proj, content)

			refused := assertRefusedAndUntouched(t, proj, root)
			want := map[string][]string{MCPProviderClaude: {"inputs"}, MCPProviderGemini: {"mcpServers"}}[provider]
			if refused.Servers != nil || refused.Redefined != nil || !reflect.DeepEqual(refused.Keys, want) {
				t.Fatalf("refusal = %+v, want keys %v", refused, want)
			}
			if got := refused.Remedy(); got != "to let gc manage it, remove those entries from the file" {
				t.Fatalf("Remedy() = %q", got)
			}
		})
	}

	root := t.TempDir()
	proj := notesProjection(t, MCPProviderClaude, root)
	writeTarget(t, proj, `{"$schema":"x","mcpServers":{"notes":{"command":"old"},"team":{"command":"team"}}}`)
	refused := assertRefusedAndUntouched(t, proj, root)
	want := `gc does not manage it yet, and adopting it would drop MCP server "team", its own definition of MCP server "notes", and top-level key "$schema"`
	if refused.Reason() != want {
		t.Fatalf("Reason() = %q\nwant       %q", refused.Reason(), want)
	}
	wantRemedy := "to let gc manage it, define the servers you still need in gc's MCP catalog (an mcp/ directory) and remove those entries from the file; " +
		`for MCP server "notes", which both define, you can instead make the catalog's definition match the file's`
	if refused.Remedy() != wantRemedy {
		t.Fatalf("Remedy() = %q\nwant      %q", refused.Remedy(), wantRemedy)
	}
}

// A file gc cannot parse might hold anything: gc leaves it alone. JSONC
// (comments, trailing commas) and a UTF-8 byte-order mark, which editors
// write and some providers accept, are not JSON to gc.
func TestApplyMCPProjectionRefusesUnparseableFiles(t *testing.T) {
	for provider, garbage := range map[string]string{
		MCPProviderClaude:   "{not json",
		MCPProviderCodex:    "= not toml",
		MCPProviderGemini:   "[1,",
		MCPProviderCursor:   "// a comment\n{}",
		MCPProviderOpenCode: `{"mcp":{},}`,
		MCPProviderMimoCode: "\ufeff{}",
	} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			proj := notesProjection(t, provider, root)
			writeTarget(t, proj, garbage)

			refused := assertRefusedAndUntouched(t, proj, root)
			if refused.ParseErr == nil || !strings.Contains(refused.Error(), "gc cannot parse it") {
				t.Fatalf("refusal = %v, want an unparseable-file refusal", refused)
			}
			want := "to let gc manage it, make it plain JSON (gc reads no JSONC comments or trailing commas, " +
				"and no UTF-8 byte-order mark) or remove it"
			if provider == MCPProviderCodex {
				want = "to let gc manage it, fix its TOML or remove it"
			}
			if got := refused.Remedy(); got != want {
				t.Fatalf("Remedy() = %q, want %q", got, want)
			}
		})
	}
}

// A parser's message quotes the text it choked on, and a provider file can
// hold a secret. gc's parse errors give only what went wrong and where, by
// line and column, whatever the file holds.
func TestMCPAdoptionParseErrorsQuoteNoFileContent(t *testing.T) {
	cases := []struct {
		provider, content, want string
	}{
		{MCPProviderCodex, "[mcp_servers.team.env]\nTOKEN = sk-live-0123456789\n", "invalid TOML at line 2, column 9"},
		{MCPProviderCodex, "TOKEN = \"sk-live-0123456789\nX = 1\n", ""},
		{MCPProviderCodex, "a = 1\na = \"sk-live-0123456789\"\n", ""},
		{MCPProviderCodex, "key = 'sk-live-0123456789' junk\n", ""},
		{MCPProviderClaude, "{\n  \"mcpServers\": {\"team\": {\"env\": {\"TOKEN\": sk-live-0123456789}}}\n}", "invalid JSON at line 2, column 44"},
		{MCPProviderClaude, `{"env":{"TOKEN":"sk-live-0123456789"}`, ""},
		{MCPProviderClaude, "{\"env\":{\"TOKEN\":\"sk-live-0123\x01\"}}", ""},
		{MCPProviderClaude, `{"a":"sk-live-0123456789" "b":1}`, ""},
		{MCPProviderClaude, `{"mcpServers":{"team":{"env":{"PORT":1e999}}}}`, "a JSON number out of range at line 1, column 38"},
		{MCPProviderGemini, `{"a":1e999}`, "a JSON number out of range at line 1, column 6"},
		{MCPProviderGemini, `  "sk-live-0123456789"`, "the top level is a JSON string, not an object, at line 1, column 3"},
		{MCPProviderGemini, "\n[1]", "the top level is a JSON array, not an object, at line 2, column 1"},
		{MCPProviderCursor, `12345`, "the top level is a JSON number, not an object, at line 1, column 1"},
		{MCPProviderOpenCode, `true`, "the top level is a JSON bool, not an object, at line 1, column 1"},
	}
	for _, tc := range cases {
		root := t.TempDir()
		proj := notesProjection(t, tc.provider, root)
		writeTarget(t, proj, tc.content)
		refused := assertRefusedAndUntouched(t, proj, root)
		if refused.ParseErr == nil {
			t.Fatalf("%s %q: refusal = %v, want a parse error", tc.provider, tc.content, refused)
		}
		if tc.want != "" && refused.ParseErr.Error() != tc.want {
			t.Fatalf("%s %q: ParseErr = %q, want %q", tc.provider, tc.content, refused.ParseErr, tc.want)
		}
		// Reason, not Error: Error names the file, and a temp dir's path
		// can hold any text.
		for _, leak := range []string{"sk-live", "0123", "1e999", "12345"} {
			if strings.Contains(refused.Reason(), leak) {
				t.Fatalf("%s %q: refusal quotes the file's content (%q): %s", tc.provider, tc.content, leak, refused.Reason())
			}
		}
	}
}

// gc writes MCP config only to regular files. A symlinked target it does not
// manage, such as a provider file linked to another provider's, is someone
// else's: gc leaves it alone when it has servers to project, and ignores it
// when it has none.
func TestApplyMCPProjectionLeavesAnUnmanagedSymlinkedTargetAlone(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, ".mcp.json")
	if err := os.WriteFile(shared, []byte(`{"mcpServers":{"team":{"command":"team"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := notesProjection(t, MCPProviderCursor, root)
	if err := os.MkdirAll(filepath.Dir(proj.Target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, proj.Target); err != nil {
		t.Fatal(err)
	}

	refused := assertRefusedAndUntouched(t, proj, root)
	if refused.Symlink != proj.Target || !strings.Contains(refused.Error(), "is a symlink") {
		t.Fatalf("refusal = %+v, want the symlink", refused)
	}

	empty, err := BuildMCPProjection(MCPProviderCursor, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply with an empty catalog on an unmanaged symlink: %v", err)
	}
	if plan, err := empty.Plan(fsys.OSFS{}); err != nil || plan.Action != MCPPlanNone || plan.Refused != nil {
		t.Fatalf("Plan with an empty catalog = %+v, %v; want nothing to do", plan, err)
	}

	// A target gc manages must stay a regular file: a symlink there is an
	// error, as before.
	markMCPManaged(t, root, MCPProviderCursor)
	if err := proj.Apply(fsys.OSFS{}); err == nil || !strings.Contains(err.Error(), "symlinked path") {
		t.Fatalf("Apply on a managed symlinked target = %v, want the symlink error", err)
	}
}

// Adoption goes ahead when it drops nothing gc does not manage: an empty or
// null document, one with only settings gc preserves, or one defining only
// servers the projection carries, the same way.
func TestApplyMCPProjectionAdoptsWhenNothingUnmanagedWouldBeLost(t *testing.T) {
	cases := map[string]struct{ provider, content string }{
		"claude whitespace":          {MCPProviderClaude, "  \n"},
		"claude same server":         {MCPProviderClaude, `{"mcpServers":{"notes":{"command":"uvx","args":[],"env":{}}}}`},
		"gemini settings only":       {MCPProviderGemini, `{"theme":"dark"}`},
		"gemini empty file":          {MCPProviderGemini, ""},
		"gemini null servers":        {MCPProviderGemini, `{"theme":"dark","mcpServers":null}`},
		"codex same server":          {MCPProviderCodex, "model = \"o3\"\n\n[mcp_servers.notes]\ncommand = \"uvx\"\n"},
		"cursor empty servers":       {MCPProviderCursor, `{"mcpServers":{}}`},
		"opencode same server":       {MCPProviderOpenCode, `{"mcp":{"notes":{"type":"local","command":["uvx"]}}}`},
		"mimocode null document":     {MCPProviderMimoCode, "null"},
		"opencode enabled explicit":  {MCPProviderOpenCode, `{"mcp":{"notes":{"type":"local","command":["uvx"],"enabled":true}}}`},
		"claude stdio type explicit": {MCPProviderClaude, `{"mcpServers":{"notes":{"type":"stdio","command":"uvx"}}}`},
		"cursor stdio type explicit": {MCPProviderCursor, `{"mcpServers":{"notes":{"type":"stdio","command":"uvx"}}}`},
		"codex enabled explicit":     {MCPProviderCodex, "[mcp_servers.notes]\ncommand = \"uvx\"\nenabled = true\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			proj := notesProjection(t, tc.provider, root)
			writeTarget(t, proj, tc.content)
			plan, err := proj.Plan(fsys.OSFS{})
			if err != nil || plan.Refused != nil || !plan.Adopt {
				t.Fatalf("plan = %+v, err = %v; want an adoption", plan, err)
			}
			if err := proj.Apply(fsys.OSFS{}); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, ".gc", "mcp-managed", tc.provider+".json")); err != nil {
				t.Fatalf("target not adopted: %v", err)
			}
			snapshots, err := os.ReadDir(filepath.Join(root, ".gc", "mcp-adopted", tc.provider))
			if err != nil || len(snapshots) != 1 {
				t.Fatalf("adoption snapshots = %v, %v; want one", snapshots, err)
			}
		})
	}
}

// The file can change between Apply's unlocked check and the lock. The check
// under the lock, on the content the snapshot would save, still refuses, and
// nothing but the lock file is written.
func TestApplyMCPProjectionRechecksTheTargetUnderTheLock(t *testing.T) {
	root := t.TempDir()
	proj := notesProjection(t, MCPProviderClaude, root)
	beforeMCPTargetLock = func() { writeTarget(t, proj, `{"mcpServers":{"team":{"command":"team"}}}`) }
	t.Cleanup(func() { beforeMCPTargetLock = nil })

	err := proj.Apply(fsys.OSFS{})
	var refused *MCPAdoptionRefusedError
	if !errors.As(err, &refused) || !reflect.DeepEqual(refused.Servers, []string{"team"}) {
		t.Fatalf("Apply error = %v, want a refusal naming the server written before the lock", err)
	}
	data, err := os.ReadFile(proj.Target)
	if err != nil || !strings.Contains(string(data), `"team"`) {
		t.Fatalf("target = %q (err %v), want the file as written", data, err)
	}
	for _, dir := range []string{"mcp-managed", "mcp-adopted"} {
		if _, err := os.Stat(filepath.Join(root, ".gc", dir)); !os.IsNotExist(err) {
			t.Fatalf(".gc/%s written by a refused adoption (stat err %v)", dir, err)
		}
	}
}

// Claude reads "streamable-http" as "http": a file that names the catalog's
// HTTP server that way defines it the same way.
func TestApplyMCPProjectionTreatsClaudesStreamableHTTPAsHTTP(t *testing.T) {
	root := t.TempDir()
	proj, err := BuildMCPProjection(MCPProviderClaude, root, []MCPServer{{Name: "search", Transport: MCPTransportHTTP, URL: "https://mcp.example.test/"}})
	if err != nil {
		t.Fatal(err)
	}
	writeTarget(t, proj, `{"mcpServers":{"search":{"type":"streamable-http","url":"https://mcp.example.test/"}}}`)
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gc", "mcp-managed", MCPProviderClaude+".json")); err != nil {
		t.Fatalf("target not adopted: %v", err)
	}
}

// A provider directory that is a symlink (.gemini/ linked elsewhere) is left
// alone like a symlinked file: gc writes nothing through it.
func TestApplyMCPProjectionLeavesASymlinkedProviderDirectoryAlone(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "settings.json"), []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, ".gemini")); err != nil {
		t.Fatal(err)
	}
	proj := notesProjection(t, MCPProviderGemini, root)
	before := treeSnapshot(t, elsewhere)

	refused := assertRefusedAndUntouched(t, proj, root)
	if refused.Symlink != filepath.Join(root, ".gemini") {
		t.Fatalf("refusal = %+v, want the symlinked .gemini directory", refused)
	}
	if got := refused.Remedy(); got != "to let gc manage it, replace the symlink with a regular file or directory" {
		t.Fatalf("Remedy() = %q", got)
	}
	if after := treeSnapshot(t, elsewhere); !reflect.DeepEqual(before, after) {
		t.Fatalf("Apply wrote through the symlinked directory:\nbefore: %v\nafter:  %v", before, after)
	}

	markMCPManaged(t, root, MCPProviderGemini)
	if err := proj.Apply(fsys.OSFS{}); err == nil || !strings.Contains(err.Error(), "symlinked path") {
		t.Fatalf("Apply on a managed target in a symlinked directory = %v, want the symlink error", err)
	}
	if after := treeSnapshot(t, elsewhere); !reflect.DeepEqual(before, after) {
		t.Fatalf("Apply wrote through the symlinked directory:\nbefore: %v\nafter:  %v", before, after)
	}
}

// The target can turn into a symlink between Apply's unlocked check and the
// lock. The check under the lock catches it: gc reads and writes nothing
// through the link.
func TestApplyMCPProjectionRechecksForASymlinkUnderTheLock(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(outside, []byte(`{"token":"s3cr3t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := notesProjection(t, MCPProviderGemini, root)
	beforeMCPTargetLock = func() {
		if err := os.MkdirAll(filepath.Dir(proj.Target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, proj.Target); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeMCPTargetLock = nil })

	err := proj.Apply(fsys.OSFS{})
	var refused *MCPAdoptionRefusedError
	if !errors.As(err, &refused) || refused.Symlink != proj.Target {
		t.Fatalf("Apply error = %v, want a refusal naming the symlink", err)
	}
	if info, err := os.Lstat(proj.Target); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced (lstat %v, err %v)", info, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != `{"token":"s3cr3t"}` {
		t.Fatalf("the link's target changed: %q (err %v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gc", "mcp-adopted")); !os.IsNotExist(err) {
		t.Fatalf("a snapshot was taken through the link (stat err %v)", err)
	}
}

// Once gc manages a target it owns the servers in it: a server added by hand
// afterwards is drift, and Apply reverts it.
func TestApplyMCPProjectionReplacesServersAddedByHandToAManagedTarget(t *testing.T) {
	root := t.TempDir()
	proj := notesProjection(t, MCPProviderGemini, root)
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	writeTarget(t, proj, `{"mcpServers":{"notes":{"command":"uvx"},"by-hand":{"command":"x"}}}`)
	if err := proj.Apply(fsys.OSFS{}); err != nil {
		t.Fatalf("Apply on a managed target: %v", err)
	}
	data, err := os.ReadFile(proj.Target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "by-hand") {
		t.Fatalf("drift survived on a managed target:\n%s", data)
	}
}

// DroppedAtAdoption lists servers an earlier adoption replaced that the
// catalog does not carry, from the snapshots of a file gc manages. A file gc
// does not manage reports nothing, whatever snapshots remain: AdoptionRefusal
// speaks for it.
func TestDroppedAtAdoptionListsServersAnEarlierAdoptionReplaced(t *testing.T) {
	root := t.TempDir()
	proj := notesProjection(t, MCPProviderClaude, root)
	snapshotDir := filepath.Join(root, ".gc", "mcp-adopted", MCPProviderClaude)
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"20260101T000000Z.json": `{"mcpServers":{"team":{"command":"team"},"notes":{"command":"old"}}}`,
		"20260102T000000Z.json": `{"mcpServers":{"lint":{"command":"lint"}}}`,
		"20260103T000000Z.json": `{not json`,
	} {
		if err := os.WriteFile(filepath.Join(snapshotDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dropped, dir, err := proj.DroppedAtAdoption(fsys.OSFS{})
	if err != nil || dropped != nil || dir != snapshotDir {
		t.Fatalf("unmanaged: DroppedAtAdoption = %v, %q, %v; want nothing", dropped, dir, err)
	}

	markMCPManaged(t, root, MCPProviderClaude)
	dropped, dir, err = proj.DroppedAtAdoption(fsys.OSFS{})
	if err != nil || !reflect.DeepEqual(dropped, []string{"lint", "team"}) || dir != snapshotDir {
		t.Fatalf("managed: DroppedAtAdoption = %v, %q, %v; want [lint team] under %s", dropped, dir, err, snapshotDir)
	}
}
