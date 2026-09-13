package githubmonitor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGraphQLClientListOpenPullRequestsPaginates(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		calls++
		switch calls {
		case 1:
			if _, err := fmt.Fprint(w, `{
				"data": {"repository": {"pullRequests": {
					"pageInfo": {"hasNextPage": true, "endCursor": "cursor-1"},
					"nodes": [{"number": 1, "baseRefName": "main", "headRefOid": "abc", "mergeStateStatus": "CLEAN"}]
				}}}
			}`); err != nil {
				t.Fatalf("writing response: %v", err)
			}
		case 2:
			if _, err := fmt.Fprint(w, `{
				"data": {"repository": {"pullRequests": {
					"pageInfo": {"hasNextPage": false, "endCursor": null},
					"nodes": [{"number": 2, "baseRefName": "main", "headRefOid": "def", "mergeStateStatus": "DIRTY"}]
				}}}
			}`); err != nil {
				t.Fatalf("writing response: %v", err)
			}
		default:
			t.Fatalf("unexpected call %d", calls)
		}
	}))
	defer server.Close()

	client := NewGraphQLClient("test-token", WithEndpoint(server.URL))
	prs, err := client.ListOpenPullRequests(context.Background(), "partcleda", "partcl")
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(prs) != 2 || prs[0].Number != 1 || prs[1].Number != 2 {
		t.Fatalf("prs = %#v, want two decoded pages", prs)
	}
}

func TestGraphQLClientReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewGraphQLClient("test-token", WithEndpoint(server.URL))
	_, err := client.ListOpenPullRequests(context.Background(), "partcleda", "partcl")
	if err == nil {
		t.Fatal("ListOpenPullRequests error = nil, want HTTP failure")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error = %v, want HTTP 401", err)
	}
}
