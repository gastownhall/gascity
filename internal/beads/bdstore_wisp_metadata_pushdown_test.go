package beads_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// wispQueryArg returns the expression argument of the first `bd query` call.
func wispQueryArg(t *testing.T, calls [][]string) string {
	t.Helper()
	for _, args := range calls {
		if len(args) > 2 && args[0] == "query" {
			return args[2]
		}
	}
	t.Fatalf("no bd query call in %q", calls)
	return ""
}

func recordingEmptyRunner(calls *[][]string) beads.CommandRunner {
	return func(_, _ string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		return []byte(`[]`), nil
	}
}

func TestBdStoreListEphemeralPushesMetadataIntoBdQuery(t *testing.T) {
	for _, tier := range []beads.TierMode{beads.TierBoth, beads.TierWisps} {
		var calls [][]string
		s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
		_, err := s.List(beads.ListQuery{
			Status:   "open",
			TierMode: tier,
			Metadata: map[string]string{
				"session_name": "rig/agent.name",
				"gc.routed_to": "rig/pool",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := `ephemeral=true AND status=open AND metadata.gc.routed_to="rig/pool" AND metadata.session_name="rig/agent.name"`
		if got := wispQueryArg(t, calls); got != want {
			t.Fatalf("tier %v: bd query expression = %q, want %q", tier, got, want)
		}
	}
}

func TestBdStoreListEphemeralQuotesMetadataValues(t *testing.T) {
	var calls [][]string
	s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
	_, err := s.List(beads.ListQuery{
		Status:   "open",
		TierMode: beads.TierBoth,
		Metadata: map[string]string{"alias": `q"uo\te AND x=1`},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `ephemeral=true AND status=open AND metadata.alias="q\"uo\\te AND x=1"`
	if got := wispQueryArg(t, calls); got != want {
		t.Fatalf("bd query expression = %q, want %q", got, want)
	}
}

// Metadata filters bd query cannot evaluate exactly like the client filter
// on every supported bd stay client-side only.
func TestBdStoreListEphemeralLeavesUnpushableMetadataClientSide(t *testing.T) {
	cases := map[string]map[string]string{
		// The client filter matches a missing key against ""; SQL cannot.
		"empty value": {"alias": ""},
		// bd v1.0.4 lowercases the key of a metadata.<key> field.
		"mixed-case key": {"Alias": "x"},
		// bd v1.0.4 rejects / in a query identifier.
		"slash key": {"jira/sprint": "x"},
		// bd's lexer reads quoted strings a byte at a time.
		"non-ASCII value": {"alias": "café"},
		"control char":    {"alias": "a\nb"},
		"JSON object":     {"alias": `{"a":1}`},
		"JSON array":      {"alias": "[1]"},
	}
	for name, md := range cases {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
			if _, err := s.List(beads.ListQuery{Status: "open", TierMode: beads.TierBoth, Metadata: md}); err != nil {
				t.Fatal(err)
			}
			if got, want := wispQueryArg(t, calls), "ephemeral=true AND status=open"; got != want {
				t.Fatalf("bd query expression = %q, want %q", got, want)
			}
		})
	}
	t.Run("mixed with pushable", func(t *testing.T) {
		var calls [][]string
		s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
		md := map[string]string{"alias": "", "session_name": "s"}
		if _, err := s.List(beads.ListQuery{Status: "open", TierMode: beads.TierBoth, Metadata: md}); err != nil {
			t.Fatal(err)
		}
		if got, want := wispQueryArg(t, calls), `ephemeral=true AND status=open AND metadata.session_name="s"`; got != want {
			t.Fatalf("bd query expression = %q, want %q", got, want)
		}
	})
}

// fakeWispQueryBd answers `bd query` over a fixed set of wisp rows. When
// evalMetadata is set it evaluates metadata.<key>="<value>" clauses the way
// bd does (JSON_UNQUOTE(JSON_EXTRACT(metadata, key)) = value, so a missing key
// never matches and a non-string scalar compares by its JSON text); otherwise
// it ignores them, which is the result bd returned before pushdown. bd list
// returns no rows.
type fakeWispQueryBd struct {
	rows         []map[string]any
	evalMetadata bool
}

func (f fakeWispQueryBd) run(_, _ string, args ...string) ([]byte, error) {
	if len(args) < 3 || args[0] != "query" {
		return []byte(`[]`), nil
	}
	var out []map[string]any
	for _, row := range f.rows {
		if f.matches(row, args[2]) {
			out = append(out, row)
		}
	}
	if out == nil {
		return []byte(`[]`), nil
	}
	return json.Marshal(out)
}

func (f fakeWispQueryBd) matches(row map[string]any, expr string) bool {
	for _, clause := range splitBdQueryAnd(expr) {
		field, value, ok := strings.Cut(clause, "=")
		if !ok {
			return false
		}
		switch {
		case field == "ephemeral":
		case field == "status":
			if row["status"] != value {
				return false
			}
		case strings.HasPrefix(field, "metadata."):
			if !f.evalMetadata {
				continue
			}
			want, err := unquoteBdQueryString(value)
			if err != nil {
				return false
			}
			md, _ := row["metadata"].(map[string]any)
			raw, ok := md[strings.TrimPrefix(field, "metadata.")]
			if !ok {
				return false
			}
			got, isString := raw.(string)
			if !isString {
				text, _ := json.Marshal(raw)
				got = string(text)
			}
			if got != want {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// splitBdQueryAnd splits on " AND " outside double-quoted strings.
func splitBdQueryAnd(expr string) []string {
	var parts []string
	inQuote, escaped, start := false, false, 0
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inQuote:
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case !inQuote && strings.HasPrefix(expr[i:], " AND "):
			parts = append(parts, expr[start:i])
			i += len(" AND ") - 1
			start = i + 1
		}
	}
	return append(parts, expr[start:])
}

func unquoteBdQueryString(v string) (string, error) {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", fmt.Errorf("not a quoted bd query string: %s", v)
	}
	var b strings.Builder
	for i := 1; i < len(v)-1; i++ {
		if v[i] == '\\' && i+1 < len(v)-1 {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String(), nil
}

func TestBdStoreListEphemeralMetadataPushdownMatchesClientFilter(t *testing.T) {
	row := func(id string, md map[string]any) map[string]any {
		r := map[string]any{"id": id, "title": id, "status": "open", "issue_type": "task", "created_at": "2026-05-01T00:00:00Z"}
		if md != nil {
			r["metadata"] = md
		}
		return r
	}
	rows := []map[string]any{
		row("w-a", map[string]any{"session_name": "rig/agent.a", "gc.routed_to": "rig/pool", "mail.read": true, "Alias": "x"}),
		row("w-b", map[string]any{"session_name": "rig/agent.b", "gc.routed_to": "rig/pool", "n": 42, "alias": `q"uo\te`}),
		row("w-c", map[string]any{"alias": "", "jira/sprint": "7", "note": "café"}),
		row("w-d", nil),
	}
	filters := []map[string]string{
		{"session_name": "rig/agent.a"},
		{"gc.routed_to": "rig/pool"},
		{"gc.routed_to": "rig/pool", "session_name": "rig/agent.b"},
		{"gc.routed_to": "rig/other"},
		{"mail.read": "true"},
		{"n": "42"},
		{"alias": `q"uo\te`},
		{"alias": ""},
		{"alias": "", "session_name": "rig/agent.a"},
		{"Alias": "x"},
		{"jira/sprint": "7"},
		{"note": "café"},
		{"session_name": "RIG/AGENT.A"},
	}
	ids := func(bs []beads.Bead) []string {
		out := make([]string, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.ID)
		}
		sort.Strings(out)
		return out
	}
	for _, md := range filters {
		q := beads.ListQuery{Status: "open", TierMode: beads.TierBoth, Metadata: md}
		pushed, err := beads.NewBdStore("/city", fakeWispQueryBd{rows: rows, evalMetadata: true}.run).List(q)
		if err != nil {
			t.Fatalf("%v: pushdown List: %v", md, err)
		}
		unpushed, err := beads.NewBdStore("/city", fakeWispQueryBd{rows: rows}.run).List(q)
		if err != nil {
			t.Fatalf("%v: unfiltered List: %v", md, err)
		}
		if got, want := ids(pushed), ids(unpushed); !reflect.DeepEqual(got, want) {
			t.Errorf("%v: with pushdown = %v, client filter alone = %v", md, got, want)
		}
	}
}
