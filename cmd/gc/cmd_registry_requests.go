package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type registryRequestsOptions struct {
	RegistryURL string
	Token       string
	JSON        bool
}

func newRegistryRequestsCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts registryRequestsOptions
	cmd := &cobra.Command{
		Use:   "requests [request-id]",
		Short: "Show your Registry publish request status",
		Long: `Show recent publish requests you submitted to Registry, or one request with its feedback comments.

This command is read-only. Use a personal Registry token; run \"gc pack registry login\" if you have not logged in yet.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if doRegistryRequests(cmd.Context(), opts, stdout, stderr, args...) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.RegistryURL, "registry-url", "", "registry app base URL; defaults to GC_REGISTRY_URL, the stored login default, then "+defaultRegistryPublishURL)
	cmd.Flags().StringVar(&opts.Token, "token", "", "personal registry API token; defaults to GC_REGISTRY_TOKEN or stored login")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "emit one JSON response object")
	return cmd
}

func doRegistryRequests(ctx context.Context, opts registryRequestsOptions, stdout, stderr io.Writer, ids ...string) int {
	baseURL, err := resolveRegistryPublishBaseURL(opts.RegistryURL)
	if err != nil {
		fmt.Fprintf(stderr, "gc pack registry requests: %v\n", err) //nolint:errcheck
		return 1
	}
	token := strings.TrimSpace(registryFirstNonEmpty(opts.Token, os.Getenv("GC_REGISTRY_TOKEN")))
	if token == "" {
		token, err = readRegistryConfiguredToken(baseURL)
		if err != nil {
			fmt.Fprintf(stderr, "gc pack registry requests: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if strings.TrimSpace(token) == "" {
		fmt.Fprintln(stderr, "gc pack registry requests: not logged in; run `gc pack registry login` to create a personal token") //nolint:errcheck
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if len(ids) == 1 {
		response, err := registryGetRequest(ctx, registryPublishHTTPClient, baseURL, token, ids[0])
		if err != nil {
			writeRegistryRequestsError(stderr, err, false)
			return 1
		}
		if err := writeRegistryRequestDetail(stdout, baseURL, response, opts.JSON); err != nil {
			fmt.Fprintf(stderr, "gc pack registry requests: rendering publish request: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}

	response, err := registryListRequests(ctx, registryPublishHTTPClient, baseURL, token)
	if err != nil {
		writeRegistryRequestsError(stderr, err, true)
		return 1
	}
	if err := writeRegistryRequestsList(stdout, response, opts.JSON); err != nil {
		fmt.Fprintf(stderr, "gc pack registry requests: rendering publish requests: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

// registryRequestsListResponse is the Registry-owned JSON response returned by
// GET /api/v1/me/publish-requests.
type registryRequestsListResponse struct {
	PublishRequests []registryPublishRequestSummary `json:"publishRequests"`
	UnreadCount     int                             `json:"unreadCount"`
	Error           *registryRequestsAPIError       `json:"error,omitempty"`
}

func (r *registryRequestsListResponse) UnmarshalJSON(data []byte) error {
	type plain registryRequestsListResponse
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = registryRequestsListResponse(decoded)

	var envelope struct {
		PublishRequests json.RawMessage           `json:"publishRequests"`
		UnreadCount     json.RawMessage           `json:"unreadCount"`
		Error           *registryRequestsAPIError `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return nil
	}
	if err := registryRequireResponseField(envelope.PublishRequests, "publishRequests"); err != nil {
		return err
	}
	if err := registryRequireResponseField(envelope.UnreadCount, "unreadCount"); err != nil {
		return err
	}
	var summaries []json.RawMessage
	if err := json.Unmarshal(envelope.PublishRequests, &summaries); err != nil {
		return fmt.Errorf("registry response publishRequests must be an array: %w", err)
	}
	for _, summary := range summaries {
		if err := validateRegistryRequestSummaryJSON(summary); err != nil {
			return err
		}
	}
	return nil
}

// registryRequestDetailResponse is the Registry-owned JSON response returned
// by GET /api/v1/me/publish-requests/{request-id}.
type registryRequestDetailResponse struct {
	PublishRequest registryPublishRequestDetail `json:"publishRequest"`
	Error          *registryRequestsAPIError    `json:"error,omitempty"`
}

func (r *registryRequestDetailResponse) UnmarshalJSON(data []byte) error {
	type plain registryRequestDetailResponse
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = registryRequestDetailResponse(decoded)

	var envelope struct {
		PublishRequest json.RawMessage           `json:"publishRequest"`
		Error          *registryRequestsAPIError `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return nil
	}
	if err := registryRequireResponseField(envelope.PublishRequest, "publishRequest"); err != nil {
		return err
	}
	return validateRegistryRequestDetailJSON(envelope.PublishRequest)
}

type registryPublishRequestSummary struct {
	ID                string                     `json:"id"`
	Status            string                     `json:"status"`
	NextStep          string                     `json:"nextStep"`
	ActionRequiredBy  string                     `json:"actionRequiredBy,omitempty"`
	RequestedName     string                     `json:"requestedName"`
	RequestedVersion  string                     `json:"requestedVersion"`
	Repository        *registryRequestRepository `json:"repository,omitempty"`
	PackPath          string                     `json:"packPath"`
	Commit            string                     `json:"commit"`
	StatusReason      string                     `json:"statusReason,omitempty"`
	Unread            bool                       `json:"unread"`
	SubmitterUnreadAt string                     `json:"submitterUnreadAt,omitempty"`
	CreatedAt         string                     `json:"createdAt"`
	UpdatedAt         string                     `json:"updatedAt"`
}

type registryPublishRequestDetail struct {
	registryPublishRequestSummary
	RepoURL         string                   `json:"repoUrl,omitempty"`
	SourceURL       string                   `json:"sourceUrl,omitempty"`
	ValidationError string                   `json:"validationError,omitempty"`
	Comments        []registryRequestComment `json:"comments"`
}

type registryRequestRepository struct {
	FullName string `json:"fullName"`
}

type registryRequestComment struct {
	ID           string `json:"id"`
	AuthorHandle string `json:"authorHandle"`
	AuthorRole   string `json:"authorRole"`
	Body         string `json:"body"`
	CreatedAt    string `json:"createdAt"`
}

type registryRequestsAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registryRequestsErrorCarrier interface {
	registryRequestsError() *registryRequestsAPIError
}

func (r registryRequestsListResponse) registryRequestsError() *registryRequestsAPIError {
	return r.Error
}

func (r registryRequestDetailResponse) registryRequestsError() *registryRequestsAPIError {
	return r.Error
}

type registryRequestsHTTPError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *registryRequestsHTTPError) Error() string {
	if e.Message != "" {
		if e.Code == "" {
			return fmt.Sprintf("Registry returned HTTP %d: %s", e.StatusCode, e.Message)
		}
		return fmt.Sprintf("Registry returned HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("Registry returned HTTP %d", e.StatusCode)
}

func registryListRequests(ctx context.Context, client *http.Client, baseURL, token string) (registryRequestsListResponse, error) {
	var response registryRequestsListResponse
	err := registryRequestsJSON(ctx, client, baseURL+"/api/v1/me/publish-requests", token, &response)
	if err != nil {
		return response, err
	}
	return response, nil
}

func registryGetRequest(ctx context.Context, client *http.Client, baseURL, token, id string) (registryRequestDetailResponse, error) {
	var response registryRequestDetailResponse
	err := registryRequestsJSON(ctx, client, baseURL+"/api/v1/me/publish-requests/"+url.PathEscape(id), token, &response)
	if err != nil {
		return response, err
	}
	return response, nil
}

func validateRegistryRequestSummaryJSON(data json.RawMessage) error {
	var required struct {
		ID               *string `json:"id"`
		Status           *string `json:"status"`
		NextStep         *string `json:"nextStep"`
		RequestedName    *string `json:"requestedName"`
		RequestedVersion *string `json:"requestedVersion"`
		Unread           *bool   `json:"unread"`
	}
	if err := json.Unmarshal(data, &required); err != nil {
		return fmt.Errorf("registry response publish request must be an object: %w", err)
	}
	if required.ID == nil || strings.TrimSpace(*required.ID) == "" {
		return errors.New("registry response did not include a publish request ID")
	}
	if required.Status == nil || !registryRequestStatusValid(*required.Status) {
		status := ""
		if required.Status != nil {
			status = *required.Status
		}
		return fmt.Errorf("registry response included unknown publish request status %q", status)
	}
	if required.NextStep == nil || strings.TrimSpace(*required.NextStep) == "" {
		return errors.New("registry response did not include a next step")
	}
	if required.RequestedName == nil || strings.TrimSpace(*required.RequestedName) == "" {
		return errors.New("registry response did not include a requested pack name")
	}
	if required.RequestedVersion == nil || strings.TrimSpace(*required.RequestedVersion) == "" {
		return errors.New("registry response did not include a requested pack version")
	}
	if required.Unread == nil {
		return errors.New("registry response did not include unread status")
	}
	return nil
}

func validateRegistryRequestDetailJSON(data json.RawMessage) error {
	if err := validateRegistryRequestSummaryJSON(data); err != nil {
		return err
	}
	var detail struct {
		Comments json.RawMessage `json:"comments"`
	}
	if err := json.Unmarshal(data, &detail); err != nil {
		return fmt.Errorf("registry response publish request must be an object: %w", err)
	}
	if err := registryRequireResponseField(detail.Comments, "comments"); err != nil {
		return err
	}
	var comments []registryRequestComment
	if err := json.Unmarshal(detail.Comments, &comments); err != nil {
		return fmt.Errorf("registry response comments must be an array: %w", err)
	}
	return nil
}

func registryRequireResponseField(value json.RawMessage, name string) error {
	if len(value) == 0 || string(bytes.TrimSpace(value)) == "null" {
		return fmt.Errorf("registry response did not include %s", name)
	}
	return nil
}

func registryRequestsJSON(ctx context.Context, client *http.Client, endpoint, token string, out registryRequestsErrorCarrier) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("contacting Registry: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := registryDecodeJSONResponse(resp, out); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &registryRequestsHTTPError{StatusCode: resp.StatusCode, Message: err.Error()}
		}
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiError := out.registryRequestsError()
		if apiError == nil {
			return &registryRequestsHTTPError{StatusCode: resp.StatusCode}
		}
		return &registryRequestsHTTPError{StatusCode: resp.StatusCode, Code: apiError.Code, Message: apiError.Message}
	}
	return nil
}

func writeRegistryRequestsError(stderr io.Writer, err error, collection bool) {
	var responseErr *registryRequestsHTTPError
	if errors.As(err, &responseErr) {
		switch {
		case responseErr.StatusCode == http.StatusUnauthorized:
			fmt.Fprintln(stderr, "gc pack registry requests: not logged in; run `gc pack registry login` to create a personal token") //nolint:errcheck
			return
		case responseErr.StatusCode == http.StatusForbidden && responseErr.Code == "TOKEN_SCOPE_DENIED":
			fmt.Fprintln(stderr, "gc pack registry requests: this token cannot read publish requests; use a personal Registry token") //nolint:errcheck
			return
		case responseErr.StatusCode == http.StatusNotFound && collection:
			fmt.Fprintln(stderr, "gc pack registry requests: this Registry does not support publish-request status; upgrade the Registry or use its Account page") //nolint:errcheck
			return
		}
	}
	fmt.Fprintf(stderr, "gc pack registry requests: %v\n", err) //nolint:errcheck
}

func writeRegistryRequestsList(stdout io.Writer, response registryRequestsListResponse, jsonOutput bool) error {
	if jsonOutput {
		if response.PublishRequests == nil {
			response.PublishRequests = []registryPublishRequestSummary{}
		}
		return json.NewEncoder(stdout).Encode(response)
	}
	if len(response.PublishRequests) == 0 {
		_, err := fmt.Fprintln(stdout, "No publish requests found.")
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tPACK\tSTATUS\tNEXT\tUPDATED\tUNREAD"); err != nil {
		return err
	}
	for _, request := range response.PublishRequests {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", request.ID, strings.TrimSpace(request.RequestedName+" "+request.RequestedVersion), request.Status, registryRequestNextLabel(request.NextStep), registryRequestTimestamp(request.UpdatedAt), registryRequestUnreadLabel(request.Unread)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "\nUnread requests: %d\n", response.UnreadCount)
	return err
}

func writeRegistryRequestDetail(stdout io.Writer, baseURL string, response registryRequestDetailResponse, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(response)
	}
	request := response.PublishRequest
	if _, err := fmt.Fprintf(stdout, "Request: %s\nPack: %s\nStatus: %s\nNext: %s\n", request.ID, strings.TrimSpace(request.RequestedName+" "+request.RequestedVersion), request.Status, registryRequestNextLabel(request.NextStep)); err != nil {
		return err
	}
	if message := registryFirstNonEmpty(request.StatusReason, request.ValidationError); message != "" {
		if _, err := fmt.Fprintf(stdout, "Message: %s\n", message); err != nil {
			return err
		}
	}
	if len(request.Comments) > 0 {
		if _, err := fmt.Fprintln(stdout, "\nComments:"); err != nil {
			return err
		}
		for _, comment := range request.Comments {
			body := "  " + strings.ReplaceAll(comment.Body, "\n", "\n  ")
			if _, err := fmt.Fprintf(stdout, "%s  @%s (%s)\n%s\n", registryRequestTimestamp(comment.CreatedAt), comment.AuthorHandle, registryRequestRoleLabel(comment.AuthorRole), body); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(stdout, "\nAccount: %s/account\n", baseURL)
	return err
}

func registryRequestRoleLabel(role string) string {
	switch strings.ToLower(role) {
	case "registry":
		return "Registry"
	case "submitter":
		return "Submitter"
	default:
		return role
	}
}

func registryRequestStatusValid(status string) bool {
	switch status {
	case "pending_validation", "validation_failed", "pending_review", "approved", "rejected", "withdrawn":
		return true
	default:
		return false
	}
}

func registryRequestNextLabel(nextStep string) string {
	if label, ok := map[string]string{
		"await_validation":      "Awaiting validation",
		"fix_validation":        "Fix validation errors and submit a new request",
		"respond_to_feedback":   "Your response is needed",
		"await_registry_review": "Awaiting Registry review",
		"published":             "Published",
		"resubmit":              "Address the decision and submit a new request",
	}[nextStep]; ok {
		return label
	}
	return nextStep
}

func registryRequestTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format(time.RFC3339)
}

func registryRequestUnreadLabel(unread bool) string {
	if unread {
		return "yes"
	}
	return "no"
}
