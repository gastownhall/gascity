package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

// S4 of the keyset-cursor track: one pagination vocabulary, enforced by
// walking the live OpenAPI spec. Every list endpoint speaks keyset
// (cursor + limit) unless its exact legacy dialect is consciously
// grandfathered below. The audit that started this program found five
// different pagination dialects that had accreted silently; this guard
// makes a sixth loud to add: known names and pagination-shaped names
// (paginationSuspect) both trip it, so drift requires either adopting
// PaginationParam or writing a grandfather entry in review.

// paginationParamNames is the vocabulary of query params that express
// pagination. `since` is deliberately absent — it is a time FILTER
// (events?since=1h), not a page boundary.
var paginationParamNames = map[string]bool{
	"cursor": true, "limit": true,
	"offset": true, "page": true, "page_size": true, "per_page": true,
	"before": true, "after": true, "after_seq": true, "after_sequence": true,
	"tail": true,
}

// paginationSuspect widens the exact vocabulary with naming patterns so a
// novel dialect cannot slip past the guard just by picking a fresh name
// (next, page_token, resume_at, start_after...). A suspect param that is
// not plain keyset forces the same choice as a known legacy name: adopt
// PaginationParam or grandfather the exact set consciously. A genuine
// filter caught by the pattern (rare) gets grandfathered too — one
// visible entry beats a silent blind spot.
func paginationSuspect(name string) bool {
	if paginationParamNames[name] {
		return true
	}
	switch name {
	case "next", "marker":
		return true
	}
	return strings.Contains(name, "cursor") || strings.Contains(name, "token") ||
		strings.HasPrefix(name, "page") || strings.HasPrefix(name, "resume") ||
		strings.HasSuffix(name, "_after") || strings.HasSuffix(name, "_before")
}

// grandfatheredDialects maps "METHOD path" to the EXACT (sorted) set of
// pagination params that legacy endpoint is allowed to keep. Owner
// sign-off 2026-07-11: these dialects predate the keyset program and
// stay as-is; new endpoints must speak keyset. Changing a set here —
// or adding an entry — is a conscious contract decision that belongs
// in its own review, not a side effect.
var grandfatheredDialects = map[string][]string{
	"GET /v0/city/{cityName}/agent/{base}/output":       {"before", "tail"},
	"GET /v0/city/{cityName}/agent/{dir}/{base}/output": {"before", "tail"},
	"GET /v0/city/{cityName}/events/stream":             {"after_seq"},
	"GET /v0/events/stream":                             {"after_cursor"},
	"GET /v0/city/{cityName}/extmsg/transcript":         {"after_sequence", "limit"},
	"GET /v0/city/{cityName}/orders/history":            {"before", "limit"},
	"GET /v0/city/{cityName}/session/{id}/transcript":   {"after", "before", "tail"},
}

// boundedLimitOnlyFeeds is the "METHOD path" allowlist of endpoints that
// expose only `limit` (no cursor) and deliberately do NOT support a keyset
// walk — they return a bounded, most-recent-N view. Owner sign-off
// 2026-07-18: these predate or intentionally sit outside the keyset program
// and stay limit-only. A NEW limit-only endpoint must either adopt keyset
// (cursor + limit via PaginationParam) or be added here in its own review;
// otherwise a sixth silent pagination dialect could ship as a bare `limit`
// param without anyone noticing, the exact drift this guard exists to stop.
// Unlike keyset lists, these are not held to the unified default/maximum
// limit schema — each feed keeps its own documented bound.
var boundedLimitOnlyFeeds = map[string]bool{
	"GET /v0/city/{cityName}/formulas/feed":        true,
	"GET /v0/city/{cityName}/formulas/{name}/runs": true,
	"GET /v0/city/{cityName}/orders/feed":          true,
	"GET /v0/city/{cityName}/runs":                 true,
	"GET /v0/events":                               true,
}

type specParam struct {
	Name   string          `json:"name"`
	In     string          `json:"in"`
	Schema json.RawMessage `json:"schema"`
}

type specOperation struct {
	Parameters []specParam                `json:"parameters"`
	Responses  map[string]json.RawMessage `json:"responses"`
}

type limitSchema struct {
	Default *float64 `json:"default"`
	Maximum *float64 `json:"maximum"`
}

// checkPaginationDialects walks a parsed OpenAPI paths object and returns
// one human-readable violation per contract breach:
//   - a pagination param set that is neither keyset (subset of
//     {cursor, limit}) nor an exact grandfathered dialect
//   - a limit-only param set ({limit} with no cursor) on an operation that is
//     not allowlisted in boundedLimitOnlyFeeds (a new silent limit-only dialect)
//   - a cursor-speaking operation that does not declare a 400 response
//     (invalid cursors are a typed 400, never a silent page-1 restart)
//   - a cursor-speaking operation whose limit schema does not pin the
//     unified default (100) and maximum (1000)
func checkPaginationDialects(paths map[string]map[string]specOperation) []string {
	var violations []string
	keys := make([]string, 0, len(paths))
	for p := range paths {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	seenGrandfathered := map[string]bool{}
	seenBoundedFeed := map[string]bool{}
	for _, path := range keys {
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			op, ok := paths[path][method]
			if !ok {
				continue
			}
			opKey := strings.ToUpper(method) + " " + path
			var pag []string
			var hasCursor bool
			var limit *specParam
			for i, p := range op.Parameters {
				if p.In != "query" || !paginationSuspect(p.Name) {
					continue
				}
				pag = append(pag, p.Name)
				if p.Name == "cursor" {
					hasCursor = true
				}
				if p.Name == "limit" {
					limit = &op.Parameters[i]
				}
			}
			if len(pag) == 0 {
				continue
			}
			sort.Strings(pag)

			keyset := true
			for _, name := range pag {
				if name != "cursor" && name != "limit" {
					keyset = false
				}
			}
			if !keyset {
				want, grandfathered := grandfatheredDialects[opKey]
				if !grandfathered {
					violations = append(violations, fmt.Sprintf(
						"%s uses pagination params %v: new endpoints must speak keyset (cursor + limit via PaginationParam); if this is a conscious legacy dialect, grandfather its exact param set in grandfatheredDialects with owner sign-off",
						opKey, pag))
					continue
				}
				seenGrandfathered[opKey] = true
				if !equalStringSets(pag, want) {
					violations = append(violations, fmt.Sprintf(
						"%s pagination params drifted: grandfathered as %v, spec now has %v; dialect changes on legacy endpoints need their own review",
						opKey, want, pag))
				}
				continue
			}

			if !hasCursor {
				// Limit-only feed ({limit}, no cursor): a bounded read with no
				// keyset walk. Legitimate for a recent-N feed, but adding one
				// must be conscious — otherwise a sixth pagination dialect ships
				// as a bare limit param with no review. The exact operation must
				// be allowlisted in boundedLimitOnlyFeeds.
				if !boundedLimitOnlyFeeds[opKey] {
					violations = append(violations, fmt.Sprintf(
						"%s exposes a limit-only pagination feed that is not allowlisted: adopt keyset (cursor + limit via PaginationParam), or if this is an intentional bounded feed, add it to boundedLimitOnlyFeeds with owner sign-off",
						opKey))
					continue
				}
				seenBoundedFeed[opKey] = true
				continue
			}
			if _, ok := op.Responses["400"]; !ok {
				violations = append(violations, fmt.Sprintf(
					"%s speaks keyset but does not declare a 400 response: invalid cursors are a typed 400 (apierr.InvalidCursor), declare it via errorStatuses(http.StatusBadRequest, ...)",
					opKey))
			}
			if limit == nil {
				violations = append(violations, fmt.Sprintf(
					"%s has a cursor param without a limit param: embed PaginationParam instead of declaring cursor ad hoc", opKey))
			} else {
				var ls limitSchema
				_ = json.Unmarshal(limit.Schema, &ls)
				if ls.Default == nil || *ls.Default != 100 || ls.Maximum == nil || *ls.Maximum != 1000 {
					violations = append(violations, fmt.Sprintf(
						"%s limit schema must pin the unified page contract (default 100, maximum 1000): embed PaginationParam rather than declaring limit ad hoc", opKey))
				}
			}
		}
	}
	for opKey := range grandfatheredDialects {
		if !seenGrandfathered[opKey] {
			violations = append(violations, fmt.Sprintf(
				"%s is grandfathered but no longer in the spec (or went keyset): remove its grandfatheredDialects entry", opKey))
		}
	}
	for opKey := range boundedLimitOnlyFeeds {
		if !seenBoundedFeed[opKey] {
			violations = append(violations, fmt.Sprintf(
				"%s is allowlisted as a bounded limit-only feed but no longer appears as one in the spec (or adopted keyset): remove its boundedLimitOnlyFeeds entry", opKey))
		}
	}
	sort.Strings(violations)
	return violations
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPaginationDialectGuard walks the live spec and fails on any
// pagination-vocabulary drift.
func TestPaginationDialectGuard(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d", rec.Code)
	}
	var spec struct {
		Paths map[string]map[string]specOperation `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("parse live spec: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("live spec has no paths")
	}
	for _, v := range checkPaginationDialects(spec.Paths) {
		t.Error(v)
	}
}

// TestPaginationDialectCheckerCatchesViolations proves the checker
// actually bites, so a refactor cannot silently neuter the guard.
func TestPaginationDialectCheckerCatchesViolations(t *testing.T) {
	limitOK := json.RawMessage(`{"type":"integer","default":100,"maximum":1000}`)
	limitBad := json.RawMessage(`{"type":"integer"}`)
	resp400 := map[string]json.RawMessage{"200": {}, "400": {}}
	resp200 := map[string]json.RawMessage{"200": {}}
	cases := []struct {
		name  string
		paths map[string]map[string]specOperation
		want  string
	}{
		{
			name: "offset dialect rejected",
			paths: map[string]map[string]specOperation{
				"/v0/widgets": {"get": {Parameters: []specParam{
					{Name: "offset", In: "query"}, {Name: "limit", In: "query", Schema: limitOK},
				}, Responses: resp400}},
			},
			want: "must speak keyset",
		},
		{
			name: "novel cursor name rejected (next/page_token class)",
			paths: map[string]map[string]specOperation{
				"/v0/widgets": {"get": {Parameters: []specParam{
					{Name: "next", In: "query"}, {Name: "limit", In: "query", Schema: limitOK},
				}, Responses: resp400}},
			},
			want: "must speak keyset",
		},
		{
			name: "grandfathered dialect drift rejected",
			paths: map[string]map[string]specOperation{
				"/v0/city/{cityName}/orders/history": {"get": {Parameters: []specParam{
					{Name: "before", In: "query"}, {Name: "after", In: "query"}, {Name: "limit", In: "query", Schema: limitOK},
				}, Responses: resp400}},
			},
			want: "drifted",
		},
		{
			name: "keyset without 400 rejected",
			paths: map[string]map[string]specOperation{
				"/v0/widgets": {"get": {Parameters: []specParam{
					{Name: "cursor", In: "query"}, {Name: "limit", In: "query", Schema: limitOK},
				}, Responses: resp200}},
			},
			want: "does not declare a 400",
		},
		{
			name: "ad-hoc limit schema rejected",
			paths: map[string]map[string]specOperation{
				"/v0/widgets": {"get": {Parameters: []specParam{
					{Name: "cursor", In: "query"}, {Name: "limit", In: "query", Schema: limitBad},
				}, Responses: resp400}},
			},
			want: "unified page contract",
		},
		{
			name: "stale grandfather entry rejected",
			paths: map[string]map[string]specOperation{
				"/v0/other": {"get": {Parameters: []specParam{{Name: "limit", In: "query", Schema: limitOK}}, Responses: resp200}},
			},
			want: "no longer in the spec",
		},
		{
			name: "unlisted limit-only feed rejected",
			paths: map[string]map[string]specOperation{
				"/v0/gadgets": {"get": {Parameters: []specParam{
					{Name: "limit", In: "query", Schema: limitOK},
				}, Responses: resp200}},
			},
			want: "not allowlisted",
		},
		{
			name: "stale bounded-feed entry rejected",
			paths: map[string]map[string]specOperation{
				// A pure keyset endpoint with none of the allowlisted bounded
				// feeds present, so every boundedLimitOnlyFeeds entry reports
				// itself stale.
				"/v0/gadgets": {"get": {Parameters: []specParam{
					{Name: "cursor", In: "query"}, {Name: "limit", In: "query", Schema: limitOK},
				}, Responses: resp400}},
			},
			want: "no longer appears as one in the spec",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			violations := checkPaginationDialects(tc.paths)
			for _, v := range violations {
				if strings.Contains(v, tc.want) {
					return
				}
			}
			t.Fatalf("checker missed the violation (want substring %q), got: %v", tc.want, violations)
		})
	}
}
