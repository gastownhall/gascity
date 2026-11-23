package hooks

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/overlay"
)

// codexHookGeneration holds the four gc-managed Codex hook commands of one
// shipped generation of the core overlay's .codex/hooks.json, named by the md5
// prefix of that file, as the #5872 report names them.
type codexHookGeneration struct {
	name                                  string
	sessionStart, preCompact, nudge, mail string
}

const (
	appendPathPrefix  = `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && `
	prependPathPrefix = `export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && `
)

// codexHookGenerations returns three shipped generations of
// internal/bootstrap/packs/core/overlay/per-provider/codex/.codex/hooks.json,
// each command copied verbatim from the file at the commit named
// (`git show <commit>:<path> | md5sum`), current first:
//
//   - 8f8d64b5, 392bd91343 (#6579): what gc ships today, "${GC_BIN:-gc}"
//     behind the appending PATH prefix.
//   - a3558786, 1b123157ba (#3457), the file gc 1.4.1 ships: bare gc behind
//     the prepending PATH prefix, with --hook-format codex and the gc hook run
//     timeout guard.
//   - 46f61e93, 7979946cd1 (#1739): bare gc behind the prepending PATH prefix,
//     without the timeout guard.
func codexHookGenerations() []codexHookGeneration {
	return []codexHookGeneration{
		{
			name:         "8f8d64b5",
			sessionStart: appendPathPrefix + `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart "${GC_BIN:-gc}" prime --hook --hook-format codex`,
			preCompact:   appendPathPrefix + `"${GC_BIN:-gc}" handoff --auto --hook-format codex "context cycle"`,
			nudge:        appendPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex`,
			mail:         appendPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex`,
		},
		{
			name:         "a3558786",
			sessionStart: prependPathPrefix + `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook --hook-format codex`,
			preCompact:   prependPathPrefix + `gc handoff --auto --hook-format codex "context cycle"`,
			nudge:        prependPathPrefix + `gc hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex`,
			mail:         prependPathPrefix + `gc hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex`,
		},
		{
			name:         "46f61e93",
			sessionStart: prependPathPrefix + `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook --hook-format codex`,
			preCompact:   prependPathPrefix + `gc handoff --auto --hook-format codex "context cycle"`,
			nudge:        prependPathPrefix + `gc nudge drain --inject --hook-format codex`,
			mail:         prependPathPrefix + `gc mail check --inject --hook-format codex`,
		},
	}
}

type testHookCmd struct{ event, command string }

// codexHooksDoc builds a Codex hooks document with one group per command.
func codexHooksDoc(t *testing.T, cmds ...testHookCmd) []byte {
	t.Helper()
	events := map[string]any{}
	for _, c := range cmds {
		group := map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": c.command}}}
		existing, _ := events[c.event].([]any)
		events[c.event] = append(existing, group)
	}
	data, err := json.Marshal(map[string]any{"hooks": events})
	if err != nil {
		t.Fatalf("marshal hooks doc: %v", err)
	}
	return data
}

// withCodexCity binds cmd, a current-form gc command, to cityPath.
func withCodexCity(cmd, cityPath string) string {
	return strings.Replace(cmd, `"${GC_BIN:-gc}" `, `"${GC_BIN:-gc}" --city `+cityPath+` `, 1)
}

func (g codexHookGeneration) cmds() []testHookCmd {
	return []testHookCmd{
		{"SessionStart", g.sessionStart},
		{"PreCompact", g.preCompact},
		{"UserPromptSubmit", g.nudge},
		{"UserPromptSubmit", g.mail},
	}
}

func embeddedCodexOverlay(t *testing.T) []byte {
	t.Helper()
	data, err := core.PackFS.ReadFile("overlay/per-provider/codex/.codex/hooks.json")
	if err != nil {
		t.Fatalf("read embedded codex overlay: %v", err)
	}
	return data
}

// TestCodexHookGenerationCurrentIsTheEmbeddedOverlay pins the first
// generation to the overlay gc ships: it holds exactly the embedded overlay's
// commands, and each of them reads as the current form.
func TestCodexHookGenerationCurrentIsTheEmbeddedOverlay(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(embeddedCodexOverlay(t), &doc); err != nil {
		t.Fatalf("decode embedded codex overlay: %v", err)
	}
	shipped := map[testHookCmd]int{}
	if !walkHookEntries(doc, func(e hookEntry) { shipped[testHookCmd{e.event, e.command}]++ }) {
		t.Fatal("embedded codex overlay is not a hooks document")
	}
	cur := codexHookGenerations()[0]
	fixture := map[testHookCmd]int{}
	for _, c := range cur.cmds() {
		fixture[c]++
	}
	if !maps.Equal(fixture, shipped) {
		t.Fatalf("generation %s holds\n%v\nbut the embedded overlay ships\n%v", cur.name, fixture, shipped)
	}
	for c := range shipped {
		if !codexManagedHookCurrent(c.event, c.command, "") {
			t.Errorf("shipped %s command %q does not read as the current form", c.event, c.command)
		}
	}
}

func TestCodexManagedHookKindAcrossGenerations(t *testing.T) {
	for _, g := range codexHookGenerations() {
		t.Run(g.name, func(t *testing.T) {
			want := map[string]string{
				g.sessionStart: codexKindSessionStartPrime,
				g.preCompact:   codexKindPreCompactHandoff,
				g.nudge:        codexKindPromptNudgeDrain,
				g.mail:         codexKindPromptMailCheck,
			}
			for _, c := range g.cmds() {
				kind, city, ok := codexManagedHookKind(c.event, c.command)
				if !ok || kind != want[c.command] || city != "" {
					t.Errorf("codexManagedHookKind(%q, %q) = %q, %q, %v; want %q, \"\", true", c.event, c.command, kind, city, ok, want[c.command])
				}
			}
		})
	}
}

func TestCodexManagedHookKindRejectsUnmanagedAndMisfiled(t *testing.T) {
	cases := []testHookCmd{
		{"SessionStart", `echo hello`},
		{"SessionStart", `FOO=1 gc prime --hook`},
		{"UserPromptSubmit", `gc prime --hook`},
		{"SessionStart", `gc nudge drain --inject`},
		{"PreCompact", `gc handoff --auto "something else"`},
		{"UserPromptSubmit", `gc mail check --inject --hook-format claude`},
		{"Stop", `gc prime --hook`},
	}
	for _, c := range cases {
		if kind, _, ok := codexManagedHookKind(c.event, c.command); ok {
			t.Errorf("codexManagedHookKind(%q, %q) = %q, true; want unmanaged", c.event, c.command, kind)
		}
	}
}

func TestCodexManagedHookKindExtractsCity(t *testing.T) {
	cases := map[string]string{
		`gc --city /srv/city prime --hook --hook-format codex`:                  "/srv/city",
		`gc --city=/srv/other mail check --inject --hook-format codex`:          "/srv/other",
		appendPathPrefix + `"${GC_BIN:-gc}" --city "/a b" nudge drain --inject`: "/a b",
	}
	events := map[string]string{
		`gc --city /srv/city prime --hook --hook-format codex`:                  "SessionStart",
		`gc --city=/srv/other mail check --inject --hook-format codex`:          "UserPromptSubmit",
		appendPathPrefix + `"${GC_BIN:-gc}" --city "/a b" nudge drain --inject`: "UserPromptSubmit",
	}
	for command, wantCity := range cases {
		_, city, ok := codexManagedHookKind(events[command], command)
		if !ok || city != wantCity {
			t.Errorf("codexManagedHookKind(%q) city = %q, %v; want %q", command, city, ok, wantCity)
		}
	}
}

func TestStackedUserHooksPath(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	cases := []struct {
		name   string
		env    map[string]string
		want   string
		wantOK bool
	}{
		{"codex home wins", map[string]string{"CODEX_HOME": "/ch", "HOME": "/h"}, filepath.Join("/ch", "hooks.json"), true},
		{"home fallback", map[string]string{"HOME": "/h"}, filepath.Join("/h", ".codex", "hooks.json"), true},
		{"relative codex home", map[string]string{"CODEX_HOME": "rel", "HOME": "/h"}, "", false},
		{"no home", map[string]string{}, "", false},
		{"relative home", map[string]string{"HOME": "h"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := StackedUserHooksPath("codex", env(tc.env))
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("StackedUserHooksPath = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
	if _, ok := StackedUserHooksPath("claude", env(map[string]string{"HOME": "/h"})); ok {
		t.Fatal("claude reported as a stacking provider")
	}
	if rel, ok := StackedProjectHookPath("codex"); !ok || rel != ".codex/hooks.json" {
		t.Fatalf("StackedProjectHookPath(codex) = %q, %v", rel, ok)
	}
}

func TestCodexManagedHookCurrent(t *testing.T) {
	cur := codexHookGenerations()[0]
	bind := func(cmd, flag string) string {
		return strings.Replace(cmd, `"${GC_BIN:-gc}" `, `"${GC_BIN:-gc}" `+flag+` `, 1)
	}
	type testCase struct {
		name  string
		event string
		cmd   string
		city  string
		want  bool
	}
	// Each "not current" case below differs from the current form in one
	// respect only.
	cases := []testCase{
		{"current SessionStart", "SessionStart", cur.sessionStart, "", true},
		{"current PreCompact", "PreCompact", cur.preCompact, "", true},
		{"current nudge drain", "UserPromptSubmit", cur.nudge, "", true},
		{"current mail check", "UserPromptSubmit", cur.mail, "", true},
		{"current with no PATH prefix", "UserPromptSubmit", strings.TrimPrefix(cur.mail, appendPathPrefix), "", true},
		{"current with the managed env reordered", "SessionStart", appendPathPrefix + `GC_HOOK_EVENT_NAME=SessionStart GC_MANAGED_SESSION_HOOK=1 "${GC_BIN:-gc}" prime --hook --hook-format codex`, "", true},
		{"bound, quoted as gc writes it", "SessionStart", bind(cur.sessionStart, `--city '/srv/city'`), "/srv/city", true},
		{"bound, unquoted", "UserPromptSubmit", bind(cur.nudge, `--city /srv/city`), "/srv/city", true},
		{"bound, --city= spelling", "PreCompact", bind(cur.preCompact, `--city=/srv/city`), "/srv/city", true},
		{"bare gc", "SessionStart", strings.Replace(cur.sessionStart, `"${GC_BIN:-gc}"`, "gc", 1), "", false},
		{"bare gc with no PATH prefix", "UserPromptSubmit", strings.Replace(strings.TrimPrefix(cur.nudge, appendPathPrefix), `"${GC_BIN:-gc}"`, "gc", 1), "", false},
		{"the prepending PATH prefix", "PreCompact", prependPathPrefix + strings.TrimPrefix(cur.preCompact, appendPathPrefix), "", false},
		{"no gc hook run timeout guard", "UserPromptSubmit", appendPathPrefix + `"${GC_BIN:-gc}" nudge drain --inject --hook-format codex`, "", false},
		{"no managed SessionStart env", "SessionStart", appendPathPrefix + `"${GC_BIN:-gc}" prime --hook --hook-format codex`, "", false},
		{"no --hook-format codex", "UserPromptSubmit", appendPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject`, "", false},
		{"no --auto on PreCompact", "PreCompact", appendPathPrefix + `"${GC_BIN:-gc}" handoff "context cycle"`, "", false},
		{"SessionStart wrapped in gc hook run", "SessionStart", appendPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- prime --hook --hook-format codex`, "", false},
		{"not a gc-managed command", "SessionStart", "echo hello", "", false},
	}
	// No command of an older shipped generation is current.
	for _, g := range codexHookGenerations()[1:] {
		cases = append(cases,
			testCase{"generation " + g.name + " SessionStart", "SessionStart", g.sessionStart, "", false},
			testCase{"generation " + g.name + " PreCompact", "PreCompact", g.preCompact, "", false},
			testCase{"generation " + g.name + " nudge drain", "UserPromptSubmit", g.nudge, "", false},
			testCase{"generation " + g.name + " mail check", "UserPromptSubmit", g.mail, "", false},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexManagedHookCurrent(tc.event, tc.cmd, tc.city); got != tc.want {
				t.Fatalf("codexManagedHookCurrent(%q, %q, %q) = %v; want %v", tc.event, tc.cmd, tc.city, got, tc.want)
			}
		})
	}
}

func TestProjectHooksCoveredByUserLayer(t *testing.T) {
	overlayDoc := embeddedCodexOverlay(t)
	const city = "/srv/city"
	userExtra := testHookCmd{"UserPromptSubmit", "my-own-hook"}

	// Only the current generation covers. An older one would cover if it were
	// current, so it is stale and the project file stays: gc 1.4.1's a3558786
	// runs every gc hook as a bare gc behind the prepending PATH prefix.
	wantByGeneration := map[string]UserLayerCoverage{
		"8f8d64b5": UserLayerCovers,
		"a3558786": UserLayerStale,
		"46f61e93": UserLayerStale,
	}
	for _, g := range codexHookGenerations() {
		t.Run("generation "+g.name, func(t *testing.T) {
			user := codexHooksDoc(t, append(g.cmds(), userExtra)...)
			got, err := ProjectHooksCoveredByUserLayer("codex", city, user, [][]byte{overlayDoc})
			if err != nil || got != wantByGeneration[g.name] {
				t.Fatalf("coverage = %v, %v; want %v, nil", got, err, wantByGeneration[g.name])
			}
		})
	}

	cur := codexHookGenerations()[0]
	older := codexHookGenerations()[1]
	cases := []struct {
		name    string
		user    []byte
		sources [][]byte
		want    UserLayerCoverage
		wantErr string
	}{
		{
			name:    "bound to this city",
			user:    codexHooksDoc(t, testHookCmd{"SessionStart", withCodexCity(cur.sessionStart, city)}, testHookCmd{"PreCompact", cur.preCompact}, testHookCmd{"UserPromptSubmit", cur.nudge}, testHookCmd{"UserPromptSubmit", cur.mail}),
			sources: [][]byte{overlayDoc},
			want:    UserLayerCovers,
		},
		{
			name:    "bound to another city",
			user:    codexHooksDoc(t, testHookCmd{"SessionStart", withCodexCity(cur.sessionStart, "/elsewhere")}, testHookCmd{"PreCompact", cur.preCompact}, testHookCmd{"UserPromptSubmit", cur.nudge}, testHookCmd{"UserPromptSubmit", cur.mail}),
			sources: [][]byte{overlayDoc},
		},
		{
			name:    "missing PreCompact",
			user:    codexHooksDoc(t, testHookCmd{"SessionStart", cur.sessionStart}, testHookCmd{"UserPromptSubmit", cur.nudge}, testHookCmd{"UserPromptSubmit", cur.mail}),
			sources: [][]byte{overlayDoc},
		},
		{
			name:    "managed command under the wrong event",
			user:    codexHooksDoc(t, testHookCmd{"Stop", cur.sessionStart}, testHookCmd{"PreCompact", cur.preCompact}, testHookCmd{"UserPromptSubmit", cur.nudge}, testHookCmd{"UserPromptSubmit", cur.mail}),
			sources: [][]byte{overlayDoc},
		},
		{
			name:    "SessionStart under a narrower matcher",
			user:    withGroupField(t, codexHooksDoc(t, cur.cmds()...), "SessionStart", "matcher", "startup"),
			sources: [][]byte{overlayDoc},
		},
		{
			name:    "prompt hooks under a narrower matcher",
			user:    withGroupField(t, codexHooksDoc(t, cur.cmds()...), "UserPromptSubmit", "matcher", "foo"),
			sources: [][]byte{overlayDoc},
		},
		{
			name:    "group without a matcher",
			user:    withoutGroupField(t, codexHooksDoc(t, cur.cmds()...), "PreCompact", "matcher"),
			sources: [][]byte{overlayDoc},
		},
		{
			name:    "entry with a field the overlay entry lacks",
			user:    withEntryField(t, codexHooksDoc(t, cur.cmds()...), "UserPromptSubmit", "timeout", 5),
			sources: [][]byte{overlayDoc},
		},
		{
			name:    "one kind only in an older form",
			user:    codexHooksDoc(t, testHookCmd{"SessionStart", cur.sessionStart}, testHookCmd{"PreCompact", cur.preCompact}, testHookCmd{"UserPromptSubmit", older.nudge}, testHookCmd{"UserPromptSubmit", cur.mail}),
			sources: [][]byte{overlayDoc},
			want:    UserLayerStale,
		},
		{
			name:    "an older copy beside the current one",
			user:    codexHooksDoc(t, append(cur.cmds(), testHookCmd{"UserPromptSubmit", older.nudge})...),
			sources: [][]byte{overlayDoc},
			want:    UserLayerCovers,
		},
		{
			name:    "older form under a narrower matcher",
			user:    withGroupField(t, codexHooksDoc(t, older.cmds()...), "SessionStart", "matcher", "startup"),
			sources: [][]byte{overlayDoc},
		},
		{
			name:    "source carries a pack-authored hook",
			user:    codexHooksDoc(t, cur.cmds()...),
			sources: [][]byte{overlayDoc, codexHooksDoc(t, testHookCmd{"Stop", "pack-hook"})},
		},
		{
			name:    "source carries a gc hook bound to another city",
			user:    codexHooksDoc(t, cur.cmds()...),
			sources: [][]byte{overlayDoc, codexHooksDoc(t, testHookCmd{"UserPromptSubmit", withCodexCity(cur.mail, "/elsewhere")})},
		},
		{
			name:    "source hooks bound to this city",
			user:    codexHooksDoc(t, cur.cmds()...),
			sources: [][]byte{codexHooksDoc(t, testHookCmd{"SessionStart", withCodexCity(cur.sessionStart, city)}, testHookCmd{"PreCompact", withCodexCity(cur.preCompact, city)}, testHookCmd{"UserPromptSubmit", withCodexCity(cur.nudge, city)}, testHookCmd{"UserPromptSubmit", withCodexCity(cur.mail, city)})},
			want:    UserLayerCovers,
		},
		{
			name:    "source carries a non-hooks key",
			user:    codexHooksDoc(t, cur.cmds()...),
			sources: [][]byte{[]byte(`{"hooks":{},"other":true}`)},
		},
		{
			name:    "sources contribute no gc hook",
			user:    codexHooksDoc(t, cur.cmds()...),
			sources: [][]byte{[]byte(`{"hooks":{"SessionStart":[]}}`)},
		},
		{
			name:    "unparseable user file",
			user:    []byte(`{`),
			sources: [][]byte{overlayDoc},
			wantErr: "parsing user hooks",
		},
		{
			name:    "user file with data after its object",
			user:    append(codexHooksDoc(t, cur.cmds()...), []byte("\n}\n")...),
			sources: [][]byte{overlayDoc},
			wantErr: "parsing user hooks",
		},
		{
			name:    "user event that is not a list of groups",
			user:    mergedHooksDoc(t, codexHooksDoc(t, cur.cmds()...), `{"hooks":{"Stop":{"matcher":""}}}`),
			sources: [][]byte{overlayDoc},
			wantErr: "not a hooks document",
		},
		{
			name:    "user hook entry that is not an object",
			user:    mergedHooksDoc(t, codexHooksDoc(t, cur.cmds()...), `{"hooks":{"Stop":[{"matcher":"","hooks":["stop-hook"]}]}}`),
			sources: [][]byte{overlayDoc},
			wantErr: "not a hooks document",
		},
		{
			name:    "unparseable source",
			user:    codexHooksDoc(t, cur.cmds()...),
			sources: [][]byte{[]byte(`{`)},
			wantErr: "parsing overlay hooks source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ProjectHooksCoveredByUserLayer("codex", city, tc.user, tc.sources)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want containing %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("coverage = %v; want %v", got, tc.want)
			}
		})
	}
	if got, err := ProjectHooksCoveredByUserLayer("claude", city, codexHooksDoc(t, cur.cmds()...), [][]byte{overlayDoc}); got != UserLayerMissesHooks || err != nil {
		t.Fatalf("non-stacking family coverage = %v, %v; want %v, nil", got, err, UserLayerMissesHooks)
	}
}

// editHooksDoc decodes doc, applies edit to every group under event and
// re-encodes it.
func editHooksDoc(t *testing.T, doc []byte, event string, edit func(group map[string]any)) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatalf("decode hooks doc: %v", err)
	}
	events, _ := root["hooks"].(map[string]any)
	groups, _ := events[event].([]any)
	if len(groups) == 0 {
		t.Fatalf("hooks doc has no %s groups", event)
	}
	for _, g := range groups {
		edit(g.(map[string]any))
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("encode hooks doc: %v", err)
	}
	return out
}

// withGroupField sets field to value on every group under event.
func withGroupField(t *testing.T, doc []byte, event, field string, value any) []byte {
	t.Helper()
	return editHooksDoc(t, doc, event, func(group map[string]any) { group[field] = value })
}

// withoutGroupField removes field from every group under event.
func withoutGroupField(t *testing.T, doc []byte, event, field string) []byte {
	t.Helper()
	return editHooksDoc(t, doc, event, func(group map[string]any) { delete(group, field) })
}

// withEntryField sets field to value on every hook entry under event.
func withEntryField(t *testing.T, doc []byte, event, field string, value any) []byte {
	t.Helper()
	return editHooksDoc(t, doc, event, func(group map[string]any) {
		for _, e := range group["hooks"].([]any) {
			e.(map[string]any)[field] = value
		}
	})
}

// mergedHooksDoc adds the events of extra, a JSON hooks document, to doc.
func mergedHooksDoc(t *testing.T, doc []byte, extra string) []byte {
	t.Helper()
	var root, add map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatalf("decode hooks doc: %v", err)
	}
	if err := json.Unmarshal([]byte(extra), &add); err != nil {
		t.Fatalf("decode extra hooks: %v", err)
	}
	events := root["hooks"].(map[string]any)
	for event, groups := range add["hooks"].(map[string]any) {
		events[event] = groups
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("encode hooks doc: %v", err)
	}
	return out
}

// codexHookGroup is a Codex hooks-document group under matcher that runs
// commands.
func codexHookGroup(matcher string, commands ...string) map[string]any {
	entries := make([]any, 0, len(commands))
	for _, c := range commands {
		entries = append(entries, map[string]any{"type": "command", "command": c})
	}
	return map[string]any{"matcher": matcher, "hooks": entries}
}

// codexHookGroupsDoc builds a Codex hooks document from its groups by event.
func codexHookGroupsDoc(t *testing.T, events map[string][]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"hooks": events})
	if err != nil {
		t.Fatalf("marshal hooks doc: %v", err)
	}
	return data
}

func TestStripManagedProjectHooks(t *testing.T) {
	const city = "/srv/city"
	cur := codexHookGenerations()[0]
	// user runs every gc hook the core overlay stages, in the current form.
	user := codexHooksDoc(t, cur.cmds()...)

	for _, g := range codexHookGenerations() {
		t.Run("all managed/"+g.name, func(t *testing.T) {
			rest, empty, changed, err := StripManagedProjectHooks("codex", city, user, codexHooksDoc(t, g.cmds()...))
			if err != nil || !empty || !changed || rest != nil {
				t.Fatalf("Strip = %q, empty=%v changed=%v err=%v; want nil, true, true, nil", rest, empty, changed, err)
			}
		})
	}

	t.Run("mixed keeps user entries", func(t *testing.T) {
		doc := codexHooksDoc(t, append(cur.cmds(), testHookCmd{"UserPromptSubmit", "my-own-hook"}, testHookCmd{"Stop", "stop-hook"})...)
		rest, empty, changed, err := StripManagedProjectHooks("codex", city, user, doc)
		if err != nil || empty || !changed {
			t.Fatalf("Strip empty=%v changed=%v err=%v", empty, changed, err)
		}
		want := codexHooksDoc(t, testHookCmd{"UserPromptSubmit", "my-own-hook"}, testHookCmd{"Stop", "stop-hook"})
		wantCanon, err := canonicalForTest(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(rest) != string(wantCanon) {
			t.Fatalf("rest =\n%s\nwant\n%s", rest, wantCanon)
		}
	})

	t.Run("managed and user entries in one group", func(t *testing.T) {
		doc := []byte(`{"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":` + jsonString(t, cur.nudge) + `},{"type":"command","command":"mine"}]}]}}`)
		rest, empty, changed, err := StripManagedProjectHooks("codex", city, user, doc)
		if err != nil || empty || !changed {
			t.Fatalf("Strip empty=%v changed=%v err=%v", empty, changed, err)
		}
		wantCanon, err := canonicalForTest([]byte(`{"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"mine"}]}]}}`))
		if err != nil {
			t.Fatal(err)
		}
		if string(rest) != string(wantCanon) {
			t.Fatalf("rest =\n%s\nwant\n%s", rest, wantCanon)
		}
	})

	t.Run("keeps gc entries the user layer does not run", func(t *testing.T) {
		// The user layer runs every gc hook but PreCompact. The project file
		// also runs prime under a narrower matcher and mail check for another
		// city; only the entries the user layer runs, unbound or bound to this
		// city, go.
		userWithoutPreCompact := codexHooksDoc(t, testHookCmd{"SessionStart", cur.sessionStart}, testHookCmd{"UserPromptSubmit", cur.nudge}, testHookCmd{"UserPromptSubmit", cur.mail})
		doc := codexHookGroupsDoc(t, map[string][]any{
			"SessionStart":     {codexHookGroup("", cur.sessionStart), codexHookGroup("startup", cur.sessionStart)},
			"PreCompact":       {codexHookGroup("", cur.preCompact)},
			"UserPromptSubmit": {codexHookGroup("", withCodexCity(cur.nudge, city), cur.mail, withCodexCity(cur.mail, "/elsewhere"))},
		})
		rest, empty, changed, err := StripManagedProjectHooks("codex", city, userWithoutPreCompact, doc)
		if err != nil || empty || !changed {
			t.Fatalf("Strip empty=%v changed=%v err=%v", empty, changed, err)
		}
		wantCanon, err := canonicalForTest(codexHookGroupsDoc(t, map[string][]any{
			"SessionStart":     {codexHookGroup("startup", cur.sessionStart)},
			"PreCompact":       {codexHookGroup("", cur.preCompact)},
			"UserPromptSubmit": {codexHookGroup("", withCodexCity(cur.mail, "/elsewhere"))},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if string(rest) != string(wantCanon) {
			t.Fatalf("rest =\n%s\nwant\n%s", rest, wantCanon)
		}
	})

	t.Run("a user layer in an older form strips nothing", func(t *testing.T) {
		doc := codexHooksDoc(t, cur.cmds()...)
		rest, empty, changed, err := StripManagedProjectHooks("codex", city, codexHooksDoc(t, codexHookGenerations()[1].cmds()...), doc)
		if err != nil || empty || changed || string(rest) != string(doc) {
			t.Fatalf("Strip = %q empty=%v changed=%v err=%v; want input unchanged", rest, empty, changed, err)
		}
	})

	t.Run("no managed entries is unchanged", func(t *testing.T) {
		doc := codexHooksDoc(t, testHookCmd{"Stop", "stop-hook"})
		rest, empty, changed, err := StripManagedProjectHooks("codex", city, user, doc)
		if err != nil || empty || changed || string(rest) != string(doc) {
			t.Fatalf("Strip = %q empty=%v changed=%v err=%v; want input unchanged", rest, empty, changed, err)
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		if _, _, _, err := StripManagedProjectHooks("codex", city, user, []byte(`{`)); err == nil {
			t.Fatal("want parse error")
		}
	})

	t.Run("data after the object", func(t *testing.T) {
		doc := append(codexHooksDoc(t, cur.cmds()...), []byte("}")...)
		if _, _, _, err := StripManagedProjectHooks("codex", city, user, doc); err == nil {
			t.Fatal("want parse error")
		}
	})

	t.Run("unparseable user layer", func(t *testing.T) {
		if _, _, _, err := StripManagedProjectHooks("codex", city, []byte(`{`), codexHooksDoc(t, cur.cmds()...)); err == nil || !strings.Contains(err.Error(), "parsing user hooks") {
			t.Fatalf("err = %v; want a user hooks parse error", err)
		}
	})
}

func canonicalForTest(data []byte) ([]byte, error) {
	return overlay.CanonicalJSON(data)
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
