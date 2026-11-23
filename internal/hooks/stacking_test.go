package hooks

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/overlay"
)

// codexHookGeneration holds the four gc-managed Codex hook commands of one
// shipped generation of the core overlay.
type codexHookGeneration struct {
	name                                  string
	sessionStart, preCompact, nudge, mail string
}

const appendPathPrefix = `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && `

// codexHookGenerations returns the three shipped generations the #5872 report
// names: a3558786 (current: --hook-format codex and the gc hook run timeout
// guard), 46f61e93 (--hook-format codex, no timeout guard) and bbd0d76e
// (neither).
func codexHookGenerations() []codexHookGeneration {
	return []codexHookGeneration{
		{
			name:         "a3558786",
			sessionStart: appendPathPrefix + `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart "${GC_BIN:-gc}" prime --hook --hook-format codex`,
			preCompact:   appendPathPrefix + `"${GC_BIN:-gc}" handoff --auto --hook-format codex "context cycle"`,
			nudge:        appendPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex`,
			mail:         appendPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex`,
		},
		{
			name:         "46f61e93",
			sessionStart: appendPathPrefix + `gc prime --hook --hook-format codex`,
			preCompact:   appendPathPrefix + `gc handoff --auto --hook-format codex "context cycle"`,
			nudge:        appendPathPrefix + `gc nudge drain --inject --hook-format codex`,
			mail:         appendPathPrefix + `gc mail check --inject --hook-format codex`,
		},
		{
			name:         "bbd0d76e",
			sessionStart: canonicalGCPathPrefix + `gc prime --hook`,
			preCompact:   canonicalGCPathPrefix + `gc handoff "context cycle"`,
			nudge:        canonicalGCPathPrefix + `gc nudge drain --inject`,
			mail:         canonicalGCPathPrefix + `gc mail check --inject`,
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

func TestProjectHooksCoveredByUserLayer(t *testing.T) {
	overlayDoc := embeddedCodexOverlay(t)
	const city = "/srv/city"
	userExtra := testHookCmd{"UserPromptSubmit", "my-own-hook"}

	for _, g := range codexHookGenerations() {
		t.Run("covered/"+g.name, func(t *testing.T) {
			user := codexHooksDoc(t, append(g.cmds(), userExtra)...)
			covered, err := ProjectHooksCoveredByUserLayer("codex", city, user, [][]byte{overlayDoc})
			if err != nil || !covered {
				t.Fatalf("covered = %v, %v; want true, nil", covered, err)
			}
		})
	}

	cur := codexHookGenerations()[0]
	withCity := func(cmd, cityPath string) string {
		return strings.Replace(cmd, `"${GC_BIN:-gc}" `, `"${GC_BIN:-gc}" --city `+cityPath+` `, 1)
	}
	cases := []struct {
		name    string
		user    []byte
		sources [][]byte
		want    bool
		wantErr string
	}{
		{
			name:    "bound to this city",
			user:    codexHooksDoc(t, testHookCmd{"SessionStart", withCity(cur.sessionStart, city)}, testHookCmd{"PreCompact", cur.preCompact}, testHookCmd{"UserPromptSubmit", cur.nudge}, testHookCmd{"UserPromptSubmit", cur.mail}),
			sources: [][]byte{overlayDoc},
			want:    true,
		},
		{
			name:    "bound to another city",
			user:    codexHooksDoc(t, testHookCmd{"SessionStart", withCity(cur.sessionStart, "/elsewhere")}, testHookCmd{"PreCompact", cur.preCompact}, testHookCmd{"UserPromptSubmit", cur.nudge}, testHookCmd{"UserPromptSubmit", cur.mail}),
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
			name:    "source carries a pack-authored hook",
			user:    codexHooksDoc(t, cur.cmds()...),
			sources: [][]byte{overlayDoc, codexHooksDoc(t, testHookCmd{"Stop", "pack-hook"})},
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
			name:    "unparseable source",
			user:    codexHooksDoc(t, cur.cmds()...),
			sources: [][]byte{[]byte(`{`)},
			wantErr: "parsing overlay hooks source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			covered, err := ProjectHooksCoveredByUserLayer("codex", city, tc.user, tc.sources)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want containing %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if covered != tc.want {
				t.Fatalf("covered = %v; want %v", covered, tc.want)
			}
		})
	}
	if covered, err := ProjectHooksCoveredByUserLayer("claude", city, codexHooksDoc(t, cur.cmds()...), [][]byte{overlayDoc}); covered || err != nil {
		t.Fatalf("non-stacking family covered = %v, %v", covered, err)
	}
}

func TestStripManagedProjectHooks(t *testing.T) {
	for _, g := range codexHookGenerations() {
		t.Run("all managed/"+g.name, func(t *testing.T) {
			rest, empty, changed, err := StripManagedProjectHooks("codex", codexHooksDoc(t, g.cmds()...))
			if err != nil || !empty || !changed || rest != nil {
				t.Fatalf("Strip = %q, empty=%v changed=%v err=%v; want nil, true, true, nil", rest, empty, changed, err)
			}
		})
	}

	t.Run("mixed keeps user entries", func(t *testing.T) {
		cur := codexHookGenerations()[0]
		doc := codexHooksDoc(t, append(cur.cmds(), testHookCmd{"UserPromptSubmit", "my-own-hook"}, testHookCmd{"Stop", "stop-hook"})...)
		rest, empty, changed, err := StripManagedProjectHooks("codex", doc)
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
		cur := codexHookGenerations()[0]
		doc := []byte(`{"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":` + jsonString(t, cur.nudge) + `},{"type":"command","command":"mine"}]}]}}`)
		rest, empty, changed, err := StripManagedProjectHooks("codex", doc)
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

	t.Run("no managed entries is unchanged", func(t *testing.T) {
		doc := codexHooksDoc(t, testHookCmd{"Stop", "stop-hook"})
		rest, empty, changed, err := StripManagedProjectHooks("codex", doc)
		if err != nil || empty || changed || string(rest) != string(doc) {
			t.Fatalf("Strip = %q empty=%v changed=%v err=%v; want input unchanged", rest, empty, changed, err)
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		if _, _, _, err := StripManagedProjectHooks("codex", []byte(`{`)); err == nil {
			t.Fatal("want parse error")
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
