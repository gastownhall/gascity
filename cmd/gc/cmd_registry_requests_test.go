package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/credentialprovider"
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
		{name: "missing token", want: "configure a native registry credential for any other registry"},
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
		{name: "missing status", body: `{"publishRequests":[{"id":"prq_one","nextStep":"respond_to_feedback","requestedName":"demo-pack","requestedVersion":"1.2.0","unread":false}],"unreadCount":0}`, want: "did not include a publish request status"},
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

func TestRegistryRequestsAcceptsTerminalInvalidStatus(t *testing.T) {
	// `invalid` is a terminal status the publish path already recognizes
	// (registryPublishValidationRejectedStatuses); requests must render it
	// rather than reject the whole response.
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		return registryRequestsHTTPResponse(r, http.StatusOK, `{"publishRequests":[{"id":"prq_bad","status":"invalid","nextStep":"resubmit","requestedName":"demo-pack","requestedVersion":"1.2.0","unread":false}],"unreadCount":0}`), nil
	})
	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "invalid") {
		t.Fatalf("stdout missing terminal status:\n%s", stdout.String())
	}
}

func TestRegistryRequestsUsesGasworksCredentialFallbackAndRefreshes(t *testing.T) {
	// A user who published through the Gasworks credential provider (no
	// explicit/env/stored token) must be able to inspect that request; requests
	// reuses the same provider fallback and 401-refresh wrapper as publish/whoami.
	clearRegistryEnv(t)
	oldClient := registryPublishHTTPClient
	oldFactory := registryNewCredentialSource
	t.Cleanup(func() {
		registryPublishHTTPClient = oldClient
		registryNewCredentialSource = oldFactory
	})

	var forceRefresh []bool
	registryNewCredentialSource = func(_ []string, _ credentialprovider.Request) (registryCredentialSource, error) {
		return func(_ context.Context, force bool) (string, error) {
			forceRefresh = append(forceRefresh, force)
			if force {
				return "sts-refreshed", nil
			}
			return "sts-initial", nil
		}, nil
	}

	requests := 0
	registryPublishHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			if got := r.Header.Get("Authorization"); got != "Bearer sts-initial" {
				t.Fatalf("first Authorization = %q", got)
			}
			return registryRequestsHTTPResponse(r, http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"expired"}}`), nil
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sts-refreshed" {
			t.Fatalf("retry Authorization = %q", got)
		}
		return registryRequestsHTTPResponse(r, http.StatusOK, registryRequestsListJSON), nil
	})}

	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: defaultRegistryPublishURL}, &stdout, &stderr); code != 0 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	if len(forceRefresh) != 2 || forceRefresh[0] || !forceRefresh[1] {
		t.Fatalf("force refresh calls = %v, want [false true]", forceRefresh)
	}
	if !strings.Contains(stdout.String(), "prq_one") {
		t.Fatalf("stdout missing refreshed result:\n%s", stdout.String())
	}
}

func TestRegistryRequestsSurfacesErrorEnvelopeOn2xx(t *testing.T) {
	// A 200 response carrying an error envelope must surface the error, not
	// render as an empty list (Don't Swallow Errors).
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		return registryRequestsHTTPResponse(r, http.StatusOK, `{"error":{"code":"RATE_LIMITED","message":"slow down"}}`), nil
	})
	var stdout, stderr bytes.Buffer
	code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRegistryRequests = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "No publish requests found.") {
		t.Fatalf("error envelope masked as empty list:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "slow down") {
		t.Fatalf("stderr missing surfaced error: %q", stderr.String())
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

func TestRegistryRequestsRendersServerExpandedStatusVerbatim(t *testing.T) {
	// status is a Registry-owned passthrough. A lifecycle value the binary does
	// not model (here `failed`, which publish already treats as terminal) must
	// render verbatim like nextStep, not fail the whole response — otherwise a
	// backward-compatible server enum growth breaks the publish->requests handoff.
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		return registryRequestsHTTPResponse(r, http.StatusOK, `{"publishRequests":[{"id":"prq_new","status":"failed","nextStep":"resubmit","requestedName":"demo-pack","requestedVersion":"1.2.0","unread":false}],"unreadCount":0}`), nil
	})
	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "failed") {
		t.Fatalf("stdout missing server-expanded status verbatim:\n%s", stdout.String())
	}
}

func TestRegistryRequestsValidatesDetailComments(t *testing.T) {
	detailWithComment := func(comment string) string {
		return `{"publishRequest":` + registryRequestSummaryJSON[:len(registryRequestSummaryJSON)-1] + `,"comments":[` + comment + `]}}`
	}
	for _, tc := range []struct {
		name    string
		comment string
		want    string
	}{
		{name: "missing id", comment: `{"authorHandle":"reviewer","authorRole":"registry","body":"hi","createdAt":"2026-07-26T11:00:00Z"}`, want: "comment did not include an ID"},
		{name: "blank id", comment: `{"id":"  ","authorHandle":"reviewer","authorRole":"registry","body":"hi","createdAt":"2026-07-26T11:00:00Z"}`, want: "comment did not include an ID"},
		{name: "missing author handle", comment: `{"id":"prc_one","authorRole":"registry","body":"hi","createdAt":"2026-07-26T11:00:00Z"}`, want: "comment did not include an author handle"},
		{name: "missing created timestamp", comment: `{"id":"prc_one","authorHandle":"reviewer","authorRole":"registry","body":"hi"}`, want: "comment did not include a created timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
				return registryRequestsHTTPResponse(r, http.StatusOK, detailWithComment(tc.comment)), nil
			})
			var stdout, stderr bytes.Buffer
			if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr, "prq_one"); code != 1 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("code=%d stderr=%q, want %q", code, stderr.String(), tc.want)
			}
		})
	}
}

func TestRegistryRequestsRendersHTTPStatusOnceForNonJSONError(t *testing.T) {
	// A proxy/CDN answering a non-2xx status with a non-JSON body must report the
	// HTTP status exactly once, not doubled through registryDecodeJSONResponse and
	// registryRequestsHTTPError both prefixing it.
	withRegistryRequestsClient(t, func(r *http.Request) (*http.Response, error) {
		return registryRequestsHTTPResponse(r, http.StatusInternalServerError, `<html>bad gateway</html>`), nil
	})
	var stdout, stderr bytes.Buffer
	if code := doRegistryRequests(t.Context(), registryRequestsOptions{RegistryURL: "http://127.0.0.1:8080", Token: "personal-token"}, &stdout, &stderr); code != 1 {
		t.Fatalf("doRegistryRequests = %d, stderr=%q", code, stderr.String())
	}
	if got := strings.Count(stderr.String(), "HTTP 500"); got != 1 {
		t.Fatalf("HTTP 500 appears %d times, want 1:\n%s", got, stderr.String())
	}
}
