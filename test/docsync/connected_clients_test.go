package docsync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const connectedClientsGuidePage = "guides/connected-clients"

func TestConnectedClientsGuideIsInGuidesNavigation(t *testing.T) {
	root := repoRoot()
	configPath := filepath.Join(root, "docs", "docs.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading docs.json: %v", err)
	}

	var decoded struct {
		Navigation struct {
			Groups []struct {
				Group string `json:"group"`
				Pages []any  `json:"pages"`
			} `json:"groups"`
		} `json:"navigation"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("parsing docs.json: %v", err)
	}

	for _, group := range decoded.Navigation.Groups {
		if group.Group != "Guides" {
			continue
		}
		if !containsMintPage(group.Pages, connectedClientsGuidePage) {
			t.Fatalf("docs.json Guides navigation must include %q", connectedClientsGuidePage)
		}
		return
	}

	t.Fatalf("docs.json navigation is missing the Guides group")
}

// TestAPIDocsCiteOnlySpecPaths fails when a docs page cites an API path, or a
// method on a path, that the published OpenAPI spec does not define. A client
// written from such a page fails on its first call (#6820).
func TestAPIDocsCiteOnlySpecPaths(t *testing.T) {
	root := repoRoot()
	spec := loadSpecOperations(t, filepath.Join(root, "docs", "reference", "schema", "openapi.json"))

	for _, page := range []string{connectedClientsGuidePage + ".md", "reference/api.md"} {
		data, err := os.ReadFile(filepath.Join(root, "docs", page))
		if err != nil {
			t.Fatalf("reading docs/%s: %v", page, err)
		}
		cited := citedAPIPaths(string(data))
		if len(cited) == 0 {
			t.Errorf("docs/%s cites no API paths; the path extractor is out of date", page)
		}
		for _, c := range cited {
			if unspecifiedAPIPaths[c.path] {
				continue
			}
			if !spec.defines(c) {
				t.Errorf("docs/%s cites %s, which docs/reference/schema/openapi.json does not define", page, c)
			}
		}
	}
}

// unspecifiedAPIPaths are real routes deliberately served outside the
// Huma-typed API, so the OpenAPI spec does not list them.
var unspecifiedAPIPaths = map[string]bool{
	"/v0/city/{cityName}/svc/": true, // raw workspace-service proxy
}

// citedAPIPath is an API path cited in docs, with its HTTP method when the
// citation names one. A path ending in "/" is a prefix of real paths.
type citedAPIPath struct {
	method string
	path   string
}

func (c citedAPIPath) String() string {
	if c.method == "" {
		return c.path
	}
	return c.method + " " + c.path
}

var citedAPIPathRE = regexp.MustCompile(`(?:\b(GET|POST|PUT|PATCH|DELETE)\s+)?(/v0/[A-Za-z0-9_{}$./-]*)`)

func citedAPIPaths(content string) []citedAPIPath {
	var cited []citedAPIPath
	for _, m := range citedAPIPathRE.FindAllStringSubmatch(content, -1) {
		cited = append(cited, citedAPIPath{
			method: m[1],
			path:   strings.TrimRight(m[2], "."),
		})
	}
	return cited
}

// specOperations maps each OpenAPI path template to its lowercase methods.
type specOperations map[string]map[string]bool

func loadSpecOperations(t *testing.T, path string) specOperations {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading OpenAPI spec: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing OpenAPI spec: %v", err)
	}
	ops := make(specOperations, len(doc.Paths))
	for p, methods := range doc.Paths {
		ops[p] = make(map[string]bool, len(methods))
		for m := range methods {
			ops[p][m] = true
		}
	}
	return ops
}

// defines reports whether the spec has a path (and method, when cited)
// matching c. A "{param}" or "$VAR" segment on either side matches any
// segment, so docs may cite templates or concrete example values.
func (s specOperations) defines(c citedAPIPath) bool {
	prefix := strings.HasSuffix(c.path, "/")
	want := strings.Split(strings.TrimSuffix(c.path, "/"), "/")
	for p, methods := range s {
		have := strings.Split(p, "/")
		if len(have) < len(want) || (!prefix && len(have) != len(want)) {
			continue
		}
		if !segmentsMatch(want, have[:len(want)]) {
			continue
		}
		if prefix || c.method == "" || methods[strings.ToLower(c.method)] {
			return true
		}
	}
	return false
}

func segmentsMatch(want, have []string) bool {
	for i := range want {
		if isPathVariable(want[i]) || isPathVariable(have[i]) {
			continue
		}
		if want[i] != have[i] {
			return false
		}
	}
	return true
}

func isPathVariable(segment string) bool {
	return strings.HasPrefix(segment, "$") ||
		(strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}"))
}

func containsMintPage(pages []any, want string) bool {
	for _, page := range pages {
		switch x := page.(type) {
		case string:
			if x == want {
				return true
			}
		case map[string]any:
			if nested, ok := x["pages"].([]any); ok && containsMintPage(nested, want) {
				return true
			}
		}
	}
	return false
}
