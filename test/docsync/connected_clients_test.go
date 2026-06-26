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

func TestConnectedClientsAPIReferenceDocumentsEndpointFamily(t *testing.T) {
	root := repoRoot()
	path := filepath.Join(root, "docs", "reference", "api.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading docs/reference/api.md: %v", err)
	}
	text := string(data)

	for _, endpoint := range []string{
		"POST /v0/extmsg/clients",
		"POST /v0/extmsg/inbound",
		"GET /v0/extmsg/{provider}/{account_id}/{conversation_id}/subscribe",
	} {
		if !strings.Contains(text, endpoint) {
			t.Errorf("docs/reference/api.md must document %s", endpoint)
		}
	}

	if !providerLLMClientRE.MatchString(text) {
		t.Errorf("docs/reference/api.md must state that POST /v0/extmsg/inbound uses provider llm-client")
	}
}

func TestConnectedClientsGuideDocumentsRequiredSections(t *testing.T) {
	root := repoRoot()
	path := filepath.Join(root, "docs", connectedClientsGuidePage+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.ToSlash(filepath.Join("docs", connectedClientsGuidePage+".md")), err)
	}

	headings := markdownHeadings(string(data))
	for _, section := range []struct {
		name    string
		pattern string
	}{
		{name: "register", pattern: "register"},
		{name: "subscribe", pattern: "subscribe"},
		{name: "send", pattern: "send"},
		{name: "reconnect", pattern: "reconnect"},
		{name: "error catalog", pattern: "error catalog"},
		{name: "heartbeat", pattern: "heartbeat"},
		{name: "configuration", pattern: "configuration"},
	} {
		if !hasHeadingContaining(headings, section.pattern) {
			t.Errorf("%s must include a %q section heading", connectedClientsGuidePage+".md", section.name)
		}
	}
}

var (
	headingRE           = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	providerLLMClientRE = regexp.MustCompile(`(?is)provider\s*[:=]?\s*["']?llm-client`)
)

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

func markdownHeadings(content string) []string {
	matches := headingRE.FindAllStringSubmatch(content, -1)
	headings := make([]string, 0, len(matches))
	for _, match := range matches {
		heading := strings.TrimSpace(match[1])
		heading = strings.Trim(heading, "`*_ ")
		headings = append(headings, strings.ToLower(heading))
	}
	return headings
}

func hasHeadingContaining(headings []string, pattern string) bool {
	for _, heading := range headings {
		if strings.Contains(heading, pattern) {
			return true
		}
	}
	return false
}
