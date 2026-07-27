package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

const registryRequestsListJSON = `{"publishRequests":[{"id":"prq_one","status":"pending_review","nextStep":"respond_to_feedback","actionRequiredBy":"submitter","requestedName":"demo-pack","requestedVersion":"1.2.0","unread":true,"submitterUnreadAt":"2026-07-26T11:00:00Z","updatedAt":"2026-07-26T11:00:00Z"}],"unreadCount":2}`

const registryRequestDetailJSON = `{"publishRequest":{"id":"prq_one","status":"withdrawn","nextStep":"resubmit","requestedName":"demo-pack","requestedVersion":"1.2.0","unread":false,"statusReason":"Withdrawn by submitter","comments":[{"id":"prc_one","authorHandle":"reviewer","authorRole":"registry","body":"Please clarify the README.","createdAt":"2026-07-26T11:00:00Z"}]}}`

const registryRequestSummaryJSON = `{"id":"prq_one","status":"pending_review","nextStep":"respond_to_feedback","requestedName":"demo-pack","requestedVersion":"1.2.0","unread":false}`

func TestRegistryRequestsListHumanAndJSON(t *testing.T) {
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		if got, want := r.Method+" "+r.URL.RequestURI(), "GET /api/v1/me/publish-requests"; got != want {
			t.Fatalf("request = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer personal-token" {
			t.Fatalf("Authorization = %q", got)
		}
		return registryRequestsHTTPResponse(r, http.StatusOK, registryRequestsListJSON), nil
	})

	for _, jsonOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonOutput], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token", JSON: jsonOutput}, &stdout, &stderr); code != 0 {
				t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
			}
			wants := []string{"prq_one", "pending_review"}
			if jsonOutput {
				wants = append(wants, `"unreadCount":2`)
			} else {
				wants = append(wants, "Your response is needed", "Unread requests: 2", "yes")
			}
			for _, want := range wants {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
				}
			}
			if jsonOutput && strings.Contains(stdout.String(), "nextCursor") {
				t.Fatalf("list JSON unexpectedly contains pagination: %s", stdout.String())
			}
		})
	}
}

func TestRegistryRequestsDetailIncludesCommentsAndResubmitGuidance(t *testing.T) {
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		if got, want := r.URL.Path, "/api/v1/me/publish-requests/prq_one"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		return registryRequestsHTTPResponse(r, http.StatusOK, registryRequestDetailJSON), nil
	})

	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr, "prq_one"); code != 0 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"Status: withdrawn", "Address the decision and submit a new request", "Comments:", "@reviewer (Registry)", "Please clarify the README."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRegistryRequestsDetailJSONRetainsComments(t *testing.T) {
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		return registryRequestsHTTPResponse(r, http.StatusOK, registryRequestDetailJSON), nil
	})
	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token", JSON: true}, &stdout, &stderr, "prq_one"); code != 0 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	for _, want := range []string{`"publishRequest"`, `"comments"`, `"id":"prc_one"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("JSON missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRegistryRequestsEmptyList(t *testing.T) {
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		return registryRequestsHTTPResponse(r, http.StatusOK, `{"publishRequests":[],"unreadCount":0}`), nil
	})
	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "No publish requests found." {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRegistryRequestsAcceptsEmptyDetailComments(t *testing.T) {
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		return registryRequestsHTTPResponse(r, http.StatusOK, `{"publishRequest":`+registryRequestSummaryJSON[:len(registryRequestSummaryJSON)-1]+`,"comments":[]}}`), nil
	})
	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token", JSON: true}, &stdout, &stderr, "prq_one"); code != 0 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"comments":[]`) {
		t.Fatalf("detail JSON did not preserve empty comments: %s", stdout.String())
	}
}

func TestRegistryRequestsGuidesAuthenticationAndOldRegistry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		token   string
		status  int
		payload string
		want    string
	}{
		{name: "missing token", want: "run `gc pack registry login` to create a personal token"},
		{name: "unauthorized", token: "personal-token", status: http.StatusUnauthorized, payload: `{"error":{"code":"UNAUTHORIZED","message":"expired"}}`, want: "run `gc pack registry login` to create a personal token"},
		{name: "old registry", token: "personal-token", status: http.StatusNotFound, payload: `{"error":{"code":"NOT_FOUND","message":"missing"}}`, want: "does not support publish-request status; upgrade the Registry or use its Account page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.token != "" {
				withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
					return registryRequestsHTTPResponse(r, tc.status, tc.payload), nil
				})
			}
			var stdout, stderr bytes.Buffer
			code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: tc.token}, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want %q", code, stdout.String(), stderr.String(), tc.want)
			}
		})
	}
}

func TestRegistryRequestsDetail404IsNotAnOldRegistry(t *testing.T) {
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		return registryRequestsHTTPResponse(r, http.StatusNotFound, `{"error":{"code":"NOT_FOUND","message":"request not found"}}`), nil
	})
	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr, "prq_missing"); code != 1 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "request not found") || strings.Contains(got, "does not support publish-request status") {
		t.Fatalf("detail 404 guidance = %q", got)
	}
}

func TestRegistryRequestsRejectsMalformedOrInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "malformed", body: `{"publishRequests":`, want: "unexpected end of JSON input"},
		{name: "missing ID", body: `{"publishRequests":[{"status":"pending_review","nextStep":"respond_to_feedback"}],"unreadCount":0}`, want: "did not include a publish request ID"},
		{name: "unknown status", body: `{"publishRequests":[{"id":"prq_one","status":"future","nextStep":"respond_to_feedback"}],"unreadCount":0}`, want: "unknown publish request status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
				return registryRequestsHTTPResponse(r, http.StatusOK, tc.body), nil
			})
			var stdout, stderr bytes.Buffer
			if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("code=%d stderr=%q, want %q", code, stderr.String(), tc.want)
			}
		})
	}
}

func TestRegistryRequestsRequiresPublicResponseFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		body string
		want string
	}{
		{name: "missing list fields", body: `{}`, want: "publishRequests"},
		{name: "missing unread count", body: `{"publishRequests":[]}`, want: "unreadCount"},
		{name: "missing requested name", body: `{"publishRequests":[{"id":"prq_one","status":"pending_review","nextStep":"respond_to_feedback","requestedVersion":"1.2.0","unread":false}],"unreadCount":0}`, want: "requested pack name"},
		{name: "missing requested version", body: `{"publishRequests":[{"id":"prq_one","status":"pending_review","nextStep":"respond_to_feedback","requestedName":"demo-pack","unread":false}],"unreadCount":0}`, want: "requested pack version"},
		{name: "missing unread", body: `{"publishRequests":[{"id":"prq_one","status":"pending_review","nextStep":"respond_to_feedback","requestedName":"demo-pack","requestedVersion":"1.2.0"}],"unreadCount":0}`, want: "unread status"},
		{name: "missing comments", id: "prq_one", body: `{"publishRequest":` + registryRequestSummaryJSON + `}`, want: "comments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
				return registryRequestsHTTPResponse(r, http.StatusOK, tc.body), nil
			})
			var stdout, stderr bytes.Buffer
			opts := registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}
			var code int
			if tc.id == "" {
				code = doRegistryRequests(t.Context(), opts, &stdout, &stderr)
			} else {
				code = doRegistryRequests(t.Context(), opts, &stdout, &stderr, tc.id)
			}
			if code != 1 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("code=%d stderr=%q, want %q", code, stderr.String(), tc.want)
			}
		})
	}
}

func TestRegistryRequestsEscapesDetailPath(t *testing.T) {
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		if got, want := r.URL.EscapedPath(), "/api/v1/me/publish-requests/prq%2Fone"; got != want {
			t.Fatalf("escaped path = %q, want %q", got, want)
		}
		return registryRequestsHTTPResponse(r, http.StatusOK, registryRequestDetailJSON), nil
	})
	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr, "prq/one"); code != 0 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
}

func TestRegistryRequestsPublicSchema(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pack", "registry", "requests", "--json-schema=result"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr=%q", code, stderr.String())
	}
	for _, want := range []string{`"x-gc-raw-json": true`, `"publishRequests"`, `"unreadCount"`, `"comments"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("schema missing %q:\n%s", want, stdout.String())
		}
	}
}

func withRegistryRequestsClient(t *testing.T, transport roundTripperFunc) {
	t.Helper()
	oldClient := registryPublishHTTPClient
	registryPublishHTTPClient = &http.Client{Transport: transport}
	t.Cleanup(func() { registryPublishHTTPClient = oldClient })
}

func registryRequestsHTTPResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}
