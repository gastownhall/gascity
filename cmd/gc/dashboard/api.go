package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// CommandRequest is the JSON request body for /api/run.
type CommandRequest struct {
	// Command is the command to run (without the binary prefix).
	// Example: "status --json" or "mail inbox"
	Command string `json:"command"`
	// Timeout in seconds (optional; see default/max run timeouts)
	Timeout int `json:"timeout,omitempty"`
	// Confirmed must be true for commands that require confirmation.
	Confirmed bool `json:"confirmed,omitempty"`
}

// CommandResponse is the JSON response from /api/run.
type CommandResponse struct {
	Success    bool   `json:"success"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Command    string `json:"command"`
}

// CommandListResponse is the JSON response from /api/commands.
type CommandListResponse struct {
	Commands []CommandInfo `json:"commands"`
}

// APIHandler handles API requests for the dashboard.
type APIHandler struct {
	// cityPath is the path to the city directory (used as working directory for gc commands).
	cityPath string
	// cityName is the human-readable city name.
	cityName string
	// apiURL is the GC API server URL. When set, handlers route through
	// the API instead of spawning subprocesses.
	apiURL string
	// cityScope is the supervisor city scope. When set, API paths are
	// rewritten from /v0/x to /v0/city/{scope}/x for supervisor routing.
	cityScope string
	// apiClient is the shared HTTP client for API calls (nil when apiURL is empty).
	apiClient *http.Client
	// defaultRunTimeout is the default timeout for command execution.
	defaultRunTimeout time.Duration
	// maxRunTimeout is the maximum allowed timeout for command execution.
	maxRunTimeout time.Duration
	// Options cache
	optionsCache     *OptionsResponse
	optionsCacheTime time.Time
	optionsCacheMu   sync.RWMutex
	// cmdSem limits concurrent command executions to prevent resource exhaustion.
	cmdSem chan struct{}
	// csrfToken is validated on POST requests to prevent cross-site request forgery.
	csrfToken string
}

const optionsCacheTTL = 30 * time.Second

// maxConcurrentCommands limits how many subprocesses can run at once
// (used by /api/run command execution).
const maxConcurrentCommands = 12

// NewAPIHandler creates a new API handler with the given configuration.
func NewAPIHandler(cityPath, cityName, apiURL, cityScope string, defaultRunTimeout, maxRunTimeout time.Duration, csrfToken string) *APIHandler {
	if csrfToken == "" {
		log.Printf("WARNING: APIHandler created with empty CSRF token — POST requests will not be protected")
	}
	h := &APIHandler{
		cityPath:          cityPath,
		cityName:          cityName,
		apiURL:            strings.TrimRight(apiURL, "/"),
		cityScope:         cityScope,
		defaultRunTimeout: defaultRunTimeout,
		maxRunTimeout:     maxRunTimeout,
		cmdSem:            make(chan struct{}, maxConcurrentCommands),
		csrfToken:         csrfToken,
	}
	if h.apiURL != "" {
		h.apiClient = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 20,
			},
		}
	}
	return h
}

// withCityScope returns a new APIHandler that routes API calls through
// /v0/city/{scope}/... for supervisor mode. Shared state (client, cache,
// semaphore) is referenced by pointer, not copied.
func (h *APIHandler) withCityScope(scope string) *APIHandler {
	return &APIHandler{
		cityPath:          h.cityPath,
		cityName:          h.cityName,
		apiURL:            h.apiURL,
		cityScope:         scope,
		apiClient:         h.apiClient,
		defaultRunTimeout: h.defaultRunTimeout,
		maxRunTimeout:     h.maxRunTimeout,
		cmdSem:            h.cmdSem,
		csrfToken:         h.csrfToken,
		// optionsCache, optionsCacheTime, optionsCacheMu are not copied —
		// the scoped handler creates its own zero-value mutex and cache.
		// This is acceptable: the options cache is a minor optimization
		// and per-city options may differ anyway.
	}
}

// apiGet performs a GET against the GC API server and returns the body.
func (h *APIHandler) apiGet(path string) ([]byte, error) {
	resp, err := h.apiClient.Get(h.apiURL + scopedPath(path, h.cityScope))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("API %s: status %d", path, resp.StatusCode)
	}
	return body, nil
}

// apiPost performs a POST against the GC API server and returns the body.
func (h *APIHandler) apiPost(path string, payload any) ([]byte, error) {
	var reqBody io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest(http.MethodPost, h.apiURL+scopedPath(path, h.cityScope), reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "1")

	resp, err := h.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("API POST %s: status %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

// apiGetListRaw returns the raw "items" from an API list response.
func (h *APIHandler) apiGetListRaw(path string) (json.RawMessage, error) {
	body, err := h.apiGet(path)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Items, nil
}

// ServeHTTP routes API requests to the appropriate handler.
func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// No CORS headers — the dashboard is served from the same origin.

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Validate CSRF token on all POST requests.
	if r.Method == http.MethodPost && h.csrfToken != "" {
		if r.Header.Get("X-Dashboard-Token") != h.csrfToken {
			h.sendError(w, "Invalid or missing dashboard token", http.StatusForbidden)
			return
		}
	}

	// Per-request city scope for supervisor routing. When the JS
	// includes ?city=X, set the scope for this request so all upstream
	// API calls route to that city. We use a wrapper that delegates to
	// the original handler but overrides cityScope.
	handler := h
	if city := r.URL.Query().Get("city"); city != "" {
		handler = h.withCityScope(city)
	}

	path := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case path == "/run" && r.Method == http.MethodPost:
		handler.handleRun(w, r)
	case path == "/commands" && r.Method == http.MethodGet:
		handler.handleCommands(w, r)
	case path == "/options" && r.Method == http.MethodGet:
		handler.handleOptions(w, r)
	case path == "/mail/inbox" && r.Method == http.MethodGet:
		handler.handleMailInbox(w, r)
	case path == "/mail/threads" && r.Method == http.MethodGet:
		handler.handleMailThreads(w, r)
	case path == "/mail/read" && r.Method == http.MethodGet:
		handler.handleMailRead(w, r)
	case path == "/mail/send" && r.Method == http.MethodPost:
		handler.handleMailSend(w, r)
	case path == "/issues/show" && r.Method == http.MethodGet:
		handler.handleIssueShow(w, r)
	case path == "/issues/create" && r.Method == http.MethodPost:
		handler.handleIssueCreate(w, r)
	case path == "/issues/close" && r.Method == http.MethodPost:
		handler.handleIssueClose(w, r)
	case path == "/issues/update" && r.Method == http.MethodPost:
		handler.handleIssueUpdate(w, r)
	case path == "/pr/show" && r.Method == http.MethodGet:
		handler.handlePRShow(w, r)
	case path == "/crew" && r.Method == http.MethodGet:
		handler.handleCrew(w, r)
	case path == "/ready" && r.Method == http.MethodGet:
		handler.handleReady(w, r)
	case path == "/events" && r.Method == http.MethodGet:
		handler.handleSSE(w, r)
	case path == "/session/preview" && r.Method == http.MethodGet:
		handler.handleSessionPreview(w, r)
	case path == "/agent/output" && r.Method == http.MethodGet:
		handler.handleAgentOutput(w, r)
	case path == "/agent/output/stream" && r.Method == http.MethodGet:
		handler.handleAgentOutputStream(w, r)
	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// handleRun executes a command and returns the result.
func (h *APIHandler) handleRun(w http.ResponseWriter, r *http.Request) {
	var req CommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate command against whitelist
	meta, err := ValidateCommand(req.Command)
	if err != nil {
		h.sendError(w, fmt.Sprintf("Command blocked: %v", err), http.StatusForbidden)
		return
	}

	// Enforce server-side confirmation for dangerous commands
	if meta.Confirm && !req.Confirmed {
		h.sendError(w, "This command requires confirmation (set confirmed: true)", http.StatusForbidden)
		return
	}

	// Try API fast-path for recognized commands.
	if output, ok := h.runViaAPI(req.Command); ok {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CommandResponse{
			Command: req.Command,
			Success: true,
			Output:  output,
		})
		return
	}

	// Determine timeout
	timeout := h.defaultRunTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
		if timeout > h.maxRunTimeout {
			timeout = h.maxRunTimeout
		}
	}

	// Parse command into args
	args := parseCommandArgs(req.Command)
	if len(args) == 0 {
		h.sendError(w, "Empty command", http.StatusBadRequest)
		return
	}

	// Sanitize args
	args = SanitizeArgs(args)

	// Execute command using the binary specified in command metadata
	start := time.Now()
	output, err := h.runValidatedCommand(r.Context(), timeout, meta.Binary, args)
	duration := time.Since(start)

	resp := CommandResponse{
		Command:    req.Command,
		DurationMs: duration.Milliseconds(),
	}

	if err != nil {
		resp.Success = false
		resp.Error = err.Error()
		resp.Output = output // Include partial output on error
	} else {
		resp.Success = true
		resp.Output = output
	}

	// Log command execution (but not for safe read-only commands to reduce noise)
	if !meta.Safe || !resp.Success {
		_ = meta // silence unused warning for now
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// runViaAPI attempts to execute a recognized command via the GC API instead of
// spawning a subprocess. Returns (output, true) if the command was handled, or
// ("", false) to fall through to subprocess execution.
func (h *APIHandler) runViaAPI(command string) (string, bool) {
	parts := parseCommandArgs(command)
	if len(parts) == 0 {
		return "", false
	}

	switch parts[0] {
	case "status":
		body, err := h.apiGet("/v0/status")
		if err != nil {
			return "", false
		}
		return prettyJSON(body), true

	case "agent":
		if len(parts) >= 2 && parts[1] == "list" {
			body, err := h.apiGet("/v0/sessions")
			if err != nil {
				return "", false
			}
			return prettyJSON(body), true
		}

	case "list":
		// bd list
		body, err := h.apiGet("/v0/beads")
		if err != nil {
			return "", false
		}
		return prettyJSON(body), true

	case "show":
		// bd show <id>
		if len(parts) >= 2 {
			body, err := h.apiGet("/v0/bead/" + parts[1])
			if err != nil {
				return "", false
			}
			return prettyJSON(body), true
		}

	case "mail":
		if len(parts) >= 2 {
			switch parts[1] {
			case "inbox", "check":
				body, err := h.apiGet("/v0/mail")
				if err != nil {
					return "", false
				}
				return prettyJSON(body), true
			}
		}

	case "convoy":
		if len(parts) >= 2 {
			switch parts[1] {
			case "list":
				body, err := h.apiGet("/v0/convoys")
				if err != nil {
					return "", false
				}
				return prettyJSON(body), true
			case "show", "status":
				if len(parts) >= 3 {
					body, err := h.apiGet("/v0/convoy/" + parts[2])
					if err != nil {
						return "", false
					}
					return prettyJSON(body), true
				}
			}
		}

	case "rig":
		if len(parts) >= 2 && parts[1] == "list" {
			body, err := h.apiGet("/v0/rigs")
			if err != nil {
				return "", false
			}
			return prettyJSON(body), true
		}

	case "hooks":
		if len(parts) >= 2 && parts[1] == "list" {
			body, err := h.apiGet("/v0/beads?status=in_progress")
			if err != nil {
				return "", false
			}
			return prettyJSON(body), true
		}

	case "sling":
		// POST /v0/sling — syntax: sling <bead-id> <target-agent> [--rig=X]
		// API expects: {target (required), bead, rig, formula}
		payload := map[string]interface{}{}
		for _, p := range parts[1:] {
			if strings.HasPrefix(p, "--rig=") {
				payload["rig"] = strings.TrimPrefix(p, "--rig=")
			} else if strings.HasPrefix(p, "--formula=") {
				payload["formula"] = strings.TrimPrefix(p, "--formula=")
			} else if _, ok := payload["bead"]; !ok {
				payload["bead"] = p
			} else if _, ok := payload["target"]; !ok {
				payload["target"] = p
			}
		}
		if _, ok := payload["target"]; !ok {
			return "", false // target is required by API
		}
		body, err := h.apiPost("/v0/sling", payload)
		if err != nil {
			return "", false
		}
		return prettyJSON(body), true
	}

	return "", false
}

// handleCommands returns the list of available commands for the palette.
func (h *APIHandler) handleCommands(w http.ResponseWriter, _ *http.Request) {
	resp := CommandListResponse{
		Commands: GetCommandList(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// runValidatedCommand executes a command with the appropriate binary (gc or bd).
// The binary string comes from CommandMeta.Binary and determines the working directory.
func (h *APIHandler) runValidatedCommand(ctx context.Context, timeout time.Duration, binary string, args []string) (string, error) {
	dir := h.cityPath
	if binary == "bd" {
		// bd commands need to run from a rig directory where .beads/ lives.
		// Use the first rig's path as default. The caller can override via args.
		dir = h.cityPath
	}
	return h.runCommandWithSem(ctx, timeout, binary, args, dir)
}

// runCommandWithSem executes a command with semaphore-based concurrency limiting.
func (h *APIHandler) runCommandWithSem(ctx context.Context, timeout time.Duration, binary string, args []string, dir string) (string, error) {
	// Apply timeout first so it bounds both semaphore wait and command execution.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Acquire semaphore slot to limit concurrent subprocess spawns.
	select {
	case h.cmdSem <- struct{}{}:
		defer func() { <-h.cmdSem }()
	case <-ctx.Done():
		return "", fmt.Errorf("command slot unavailable: %w", ctx.Err())
	}

	return runCommand(ctx, binary, args, dir)
}

// runCommand creates an exec.CommandContext, sets Dir if provided, runs, and returns combined output.
func runCommand(ctx context.Context, binary string, args []string, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Ensure the command doesn't wait for stdin
	cmd.Stdin = nil

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Combine stdout and stderr for output
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("command timed out")
	}

	if err != nil {
		return output, fmt.Errorf("command failed: %w", err)
	}

	return output, nil
}

// sendError sends a JSON error response.
func (h *APIHandler) sendError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(CommandResponse{
		Success: false,
		Error:   message,
	})
}

// ---------- Mail types and handlers ----------

// MailMessage represents a mail message for the API.
type MailMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body,omitempty"`
	Timestamp string `json:"timestamp"`
	Read      bool   `json:"read"`
	Priority  string `json:"priority,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
}

// MailInboxResponse is the response for /api/mail/inbox.
type MailInboxResponse struct {
	Messages    []MailMessage `json:"messages"`
	UnreadCount int           `json:"unread_count"`
	Total       int           `json:"total"`
}

// MailThread represents a group of messages in a conversation thread.
type MailThread struct {
	ThreadID    string        `json:"thread_id"`
	Subject     string        `json:"subject"`
	LastMessage MailMessage   `json:"last_message"`
	Messages    []MailMessage `json:"messages"`
	Count       int           `json:"count"`
	UnreadCount int           `json:"unread_count"`
}

// MailThreadsResponse is the response for /api/mail/threads.
type MailThreadsResponse struct {
	Threads     []MailThread `json:"threads"`
	UnreadCount int          `json:"unread_count"`
	Total       int          `json:"total"`
}

// handleMailInbox returns the user's inbox.
func (h *APIHandler) handleMailInbox(w http.ResponseWriter, r *http.Request) {
	h.handleMailInboxAPI(w, r)
}

func (h *APIHandler) handleMailInboxAPI(w http.ResponseWriter, _ *http.Request) {
	itemsRaw, err := h.apiGetListRaw("/v0/mail")
	if err != nil {
		h.sendError(w, "Failed to fetch inbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var apiMsgs []apiMailMessage
	if err := json.Unmarshal(itemsRaw, &apiMsgs); err != nil {
		h.sendError(w, "Failed to parse inbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	messages := make([]MailMessage, 0, len(apiMsgs))
	unread := 0
	for _, m := range apiMsgs {
		msg := MailMessage{
			ID:        m.ID,
			From:      m.From,
			To:        m.To,
			Subject:   m.Subject,
			Body:      m.Body,
			Timestamp: m.CreatedAt.Format(time.RFC3339),
			Read:      m.Read,
			ThreadID:  m.ThreadID,
			ReplyTo:   m.ReplyTo,
		}
		if !m.Read {
			unread++
		}
		messages = append(messages, msg)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(MailInboxResponse{
		Messages:    messages,
		UnreadCount: unread,
		Total:       len(messages),
	})
}

// handleMailThreads returns the inbox grouped by conversation threads.
func (h *APIHandler) handleMailThreads(w http.ResponseWriter, _ *http.Request) {
	h.handleMailThreadsAPI(w)
}

func (h *APIHandler) handleMailThreadsAPI(w http.ResponseWriter) {
	itemsRaw, err := h.apiGetListRaw("/v0/mail")
	if err != nil {
		h.sendError(w, "Failed to fetch inbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var apiMsgs []apiMailMessage
	if err := json.Unmarshal(itemsRaw, &apiMsgs); err != nil {
		h.sendError(w, "Failed to parse inbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	messages := make([]MailMessage, 0, len(apiMsgs))
	for _, m := range apiMsgs {
		messages = append(messages, MailMessage{
			ID:        m.ID,
			From:      m.From,
			To:        m.To,
			Subject:   m.Subject,
			Body:      m.Body,
			Timestamp: m.CreatedAt.Format(time.RFC3339),
			Read:      m.Read,
			ThreadID:  m.ThreadID,
			ReplyTo:   m.ReplyTo,
		})
	}

	threads := groupIntoThreads(messages)
	unread := 0
	for _, t := range threads {
		unread += t.UnreadCount
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(MailThreadsResponse{
		Threads:     threads,
		UnreadCount: unread,
		Total:       len(messages),
	})
}

// groupIntoThreads groups messages into conversation threads.
// Messages are grouped by ThreadID when available, otherwise by ReplyTo chain,
// and finally by subject similarity as a fallback.
func groupIntoThreads(messages []MailMessage) []MailThread {
	// Map from thread key to slice of messages
	threadMap := make(map[string][]MailMessage)
	// Track message ID -> thread key for reply-to chaining
	msgToThread := make(map[string]string)
	// Maintain insertion order of thread keys
	var threadOrder []string
	threadSeen := make(map[string]bool)

	for _, msg := range messages {
		var threadKey string

		switch {
		case msg.ThreadID != "":
			// Priority 1: Use ThreadID if present
			threadKey = "thread:" + msg.ThreadID
		case msg.ReplyTo != "":
			// Priority 2: Follow reply-to chain
			if parentKey, ok := msgToThread[msg.ReplyTo]; ok {
				threadKey = parentKey
			} else {
				// Start a new thread anchored to the reply-to ID
				threadKey = "reply:" + msg.ReplyTo
			}
		default:
			// Priority 3: Standalone message (its own thread)
			threadKey = "msg:" + msg.ID
		}

		threadMap[threadKey] = append(threadMap[threadKey], msg)
		msgToThread[msg.ID] = threadKey

		if !threadSeen[threadKey] {
			threadOrder = append(threadOrder, threadKey)
			threadSeen[threadKey] = true
		}
	}

	// Build thread structs, ordered by most recent message
	var threads []MailThread
	for _, key := range threadOrder {
		msgs := threadMap[key]
		if len(msgs) == 0 {
			continue
		}

		// Last message is the most recent (messages come in chronological order)
		last := msgs[len(msgs)-1]

		// Use the first message's subject as the thread subject (strip Re: prefixes)
		subject := msgs[0].Subject
		subject = strings.TrimPrefix(subject, "Re: ")
		subject = strings.TrimPrefix(subject, "RE: ")

		unread := 0
		for _, m := range msgs {
			if !m.Read {
				unread++
			}
		}

		threadID := key
		if last.ThreadID != "" {
			threadID = last.ThreadID
		}

		threads = append(threads, MailThread{
			ThreadID:    threadID,
			Subject:     subject,
			LastMessage: last,
			Messages:    msgs,
			Count:       len(msgs),
			UnreadCount: unread,
		})
	}

	return threads
}

// handleMailRead reads a specific message by ID.
func (h *APIHandler) handleMailRead(w http.ResponseWriter, r *http.Request) {
	msgID := r.URL.Query().Get("id")
	if msgID == "" {
		h.sendError(w, "Missing message ID", http.StatusBadRequest)
		return
	}
	if !isValidID(msgID) {
		h.sendError(w, "Invalid message ID format", http.StatusBadRequest)
		return
	}

	body, err := h.apiGet("/v0/mail/" + msgID)
	if err != nil {
		h.sendError(w, "Failed to read message: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Mark as read via API.
	_, _ = h.apiPost("/v0/mail/"+msgID+"/read", nil)

	// Transform mail.Message JSON into dashboard MailMessage shape.
	var apiMsg apiMailMessage
	if err := json.Unmarshal(body, &apiMsg); err != nil {
		h.sendError(w, "Failed to parse message: "+err.Error(), http.StatusInternalServerError)
		return
	}
	msg := MailMessage{
		ID:        apiMsg.ID,
		From:      apiMsg.From,
		To:        apiMsg.To,
		Subject:   apiMsg.Subject,
		Body:      apiMsg.Body,
		Timestamp: apiMsg.CreatedAt.Format(time.RFC3339),
		Read:      true, // just marked as read
		ThreadID:  apiMsg.ThreadID,
		ReplyTo:   apiMsg.ReplyTo,
		Priority:  mailPriorityString(apiMsg.Priority),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

// MailSendRequest is the request body for /api/mail/send.
type MailSendRequest struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"`
}

// handleMailSend sends a new message.
func (h *APIHandler) handleMailSend(w http.ResponseWriter, r *http.Request) {
	var req MailSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.To == "" || req.Subject == "" {
		h.sendError(w, "Missing required fields (to, subject)", http.StatusBadRequest)
		return
	}
	if !isValidMailAddress(req.To) {
		h.sendError(w, "Invalid recipient format", http.StatusBadRequest)
		return
	}
	if req.ReplyTo != "" && !isValidID(req.ReplyTo) {
		h.sendError(w, "Invalid reply-to ID format", http.StatusBadRequest)
		return
	}

	const maxSubjectLen = 500
	const maxBodyLen = 100_000
	if len(req.Subject) > maxSubjectLen {
		h.sendError(w, fmt.Sprintf("Subject too long (max %d bytes)", maxSubjectLen), http.StatusBadRequest)
		return
	}
	if len(req.Body) > maxBodyLen {
		h.sendError(w, fmt.Sprintf("Body too long (max %d bytes)", maxBodyLen), http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Subject, "\x00") || strings.Contains(req.Body, "\x00") {
		h.sendError(w, "Subject and body cannot contain null bytes", http.StatusBadRequest)
		return
	}

	var err error
	if req.ReplyTo != "" {
		// Use the reply endpoint for threaded replies.
		apiReq := map[string]string{
			"from":    "dashboard",
			"subject": req.Subject,
			"body":    req.Body,
		}
		_, err = h.apiPost("/v0/mail/"+req.ReplyTo+"/reply", apiReq)
	} else {
		apiReq := map[string]string{
			"from":    "dashboard",
			"to":      req.To,
			"subject": req.Subject,
			"body":    req.Body,
		}
		_, err = h.apiPost("/v0/mail", apiReq)
	}
	if err != nil {
		h.sendError(w, "Failed to send message: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Message sent",
	})
}

// ---------- Options handler ----------

// OptionItem represents an option with name and status.
type OptionItem struct {
	Name    string `json:"name"`
	Status  string `json:"status,omitempty"`
	Running bool   `json:"running,omitempty"`
}

// OptionsResponse is the JSON response from /api/options.
type OptionsResponse struct {
	Rigs        []string     `json:"rigs,omitempty"`
	Agents      []OptionItem `json:"agents,omitempty"`
	Convoys     []string     `json:"convoys,omitempty"`
	Hooks       []string     `json:"hooks,omitempty"`
	Messages    []string     `json:"messages,omitempty"`
	Crew        []string     `json:"crew,omitempty"`
	Escalations []string     `json:"escalations,omitempty"`
}

// handleOptions returns dynamic options for command arguments.
// Results are cached for 30 seconds to avoid slow repeated fetches.
func (h *APIHandler) handleOptions(w http.ResponseWriter, _ *http.Request) {
	// Check cache first — serialize under RLock to a buffer so we don't
	// hold the lock while writing to the ResponseWriter (which can block
	// on slow clients).
	h.optionsCacheMu.RLock()
	if h.optionsCache != nil && time.Since(h.optionsCacheTime) < optionsCacheTTL {
		data, err := json.Marshal(h.optionsCache)
		h.optionsCacheMu.RUnlock()
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			_, _ = w.Write(data)
			_, _ = w.Write([]byte("\n"))
			return
		}
		// Marshal failure is unexpected; fall through to refetch.
	} else {
		h.optionsCacheMu.RUnlock()
	}

	resp := h.fetchOptionsAPI()

	// Update cache
	h.optionsCacheMu.Lock()
	h.optionsCache = resp
	h.optionsCacheTime = time.Now()
	h.optionsCacheMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	_ = json.NewEncoder(w).Encode(resp)
}

// fetchOptionsAPI fetches options data from the GC API server.
func (h *APIHandler) fetchOptionsAPI() *OptionsResponse {
	resp := &OptionsResponse{}
	var wg sync.WaitGroup
	var mu sync.Mutex

	wg.Add(4)

	// Fetch rigs
	go func() {
		defer wg.Done()
		itemsRaw, err := h.apiGetListRaw("/v0/rigs")
		if err != nil {
			log.Printf("warning: handleOptions API: rigs: %v", err)
			return
		}
		var rigs []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(itemsRaw, &rigs) == nil {
			mu.Lock()
			for _, r := range rigs {
				resp.Rigs = append(resp.Rigs, r.Name)
			}
			mu.Unlock()
		}
	}()

	// Fetch sessions (provides agents, crew lists)
	go func() {
		defer wg.Done()
		itemsRaw, err := h.apiGetListRaw("/v0/sessions")
		if err != nil {
			log.Printf("warning: handleOptions API: sessions: %v", err)
			return
		}
		var sessions []apiSessionResponse
		if json.Unmarshal(itemsRaw, &sessions) == nil {
			mu.Lock()
			for _, s := range sessions {
				state := "stopped"
				if s.Running {
					state = "running"
				} else if s.State == "suspended" {
					state = "suspended"
				}
				resp.Agents = append(resp.Agents, OptionItem{
					Name:    s.Template,
					Status:  state,
					Running: s.Running,
				})
				resp.Crew = append(resp.Crew, s.Template)
			}
			mu.Unlock()
		}
	}()

	// Fetch hooked beads (provides hooks list)
	go func() {
		defer wg.Done()
		itemsRaw, err := h.apiGetListRaw("/v0/beads?status=in_progress")
		if err != nil {
			log.Printf("warning: handleOptions API: hooks: %v", err)
			return
		}
		var beads []apiBead
		if json.Unmarshal(itemsRaw, &beads) == nil {
			mu.Lock()
			for _, b := range beads {
				resp.Hooks = append(resp.Hooks, b.ID)
			}
			mu.Unlock()
		}
	}()

	// Fetch mail (provides message IDs)
	go func() {
		defer wg.Done()
		itemsRaw, err := h.apiGetListRaw("/v0/mail")
		if err != nil {
			log.Printf("warning: handleOptions API: mail: %v", err)
			return
		}
		var msgs []apiMailMessage
		if json.Unmarshal(itemsRaw, &msgs) == nil {
			mu.Lock()
			for _, m := range msgs {
				resp.Messages = append(resp.Messages, m.ID)
			}
			mu.Unlock()
		}
	}()

	wg.Wait()
	return resp
}

// ---------- Issue types and handlers ----------

// IssueShowResponse is the response for /api/issues/show.
type IssueShowResponse struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Type        string   `json:"type,omitempty"`
	Status      string   `json:"status,omitempty"`
	Priority    string   `json:"priority,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Description string   `json:"description,omitempty"`
	Created     string   `json:"created,omitempty"`
	Updated     string   `json:"updated,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	Blocks      []string `json:"blocks,omitempty"`
	RawOutput   string   `json:"raw_output"`
}

// handleIssueShow returns details for a specific issue/bead.
func (h *APIHandler) handleIssueShow(w http.ResponseWriter, r *http.Request) {
	issueID := r.URL.Query().Get("id")
	if issueID == "" {
		h.sendError(w, "Missing issue ID", http.StatusBadRequest)
		return
	}

	showID := extractIssueID(issueID)
	if strings.HasPrefix(issueID, "external:") && showID == issueID {
		h.sendError(w, "Malformed external issue ID (expected external:prefix:id)", http.StatusBadRequest)
		return
	}
	if !isValidID(showID) {
		h.sendError(w, "Invalid issue ID format", http.StatusBadRequest)
		return
	}

	body, err := h.apiGet("/v0/bead/" + showID)
	if err != nil {
		h.sendError(w, "Failed to fetch issue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var bead apiBead
	if err := json.Unmarshal(body, &bead); err != nil {
		h.sendError(w, "Failed to parse issue: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := IssueShowResponse{
		ID:          issueID,
		Title:       bead.Title,
		Type:        bead.Type,
		Status:      bead.Status,
		Description: bead.Description,
		Created:     bead.CreatedAt.Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// IssueCreateRequest is the request body for creating an issue.
type IssueCreateRequest struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Priority    int    `json:"priority,omitempty"` // 1-4, default 2
	Rig         string `json:"rig,omitempty"`      // required when multiple rigs configured
}

// IssueCreateResponse is the response from creating an issue.
type IssueCreateResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handleIssueCreate creates a new issue.
func (h *APIHandler) handleIssueCreate(w http.ResponseWriter, r *http.Request) {
	var req IssueCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Title == "" {
		h.sendError(w, "Title is required", http.StatusBadRequest)
		return
	}

	const maxTitleLen = 500
	const maxDescriptionLen = 100_000
	if len(req.Title) > maxTitleLen {
		h.sendError(w, fmt.Sprintf("Title too long (max %d bytes)", maxTitleLen), http.StatusBadRequest)
		return
	}
	if len(req.Description) > maxDescriptionLen {
		h.sendError(w, fmt.Sprintf("Description too long (max %d bytes)", maxDescriptionLen), http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(req.Title, "\n\r\x00") {
		h.sendError(w, "Title cannot contain newlines or control characters", http.StatusBadRequest)
		return
	}
	if req.Description != "" && strings.Contains(req.Description, "\x00") {
		h.sendError(w, "Description cannot contain null characters", http.StatusBadRequest)
		return
	}

	apiReq := map[string]interface{}{
		"title":       req.Title,
		"description": req.Description,
	}
	if req.Rig != "" {
		apiReq["rig"] = req.Rig
	}
	body, err := h.apiPost("/v0/beads", apiReq)
	resp := IssueCreateResponse{}
	if err != nil {
		resp.Success = false
		resp.Error = "Failed to create issue: " + err.Error()
	} else {
		resp.Success = true
		resp.Message = "Issue created"
		var bead apiBead
		if json.Unmarshal(body, &bead) == nil {
			resp.ID = bead.ID
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// IssueCloseRequest is the request body for closing an issue.
type IssueCloseRequest struct {
	ID string `json:"id"`
}

// handleIssueClose closes an issue.
func (h *APIHandler) handleIssueClose(w http.ResponseWriter, r *http.Request) {
	var req IssueCloseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.ID == "" {
		h.sendError(w, "Issue ID is required", http.StatusBadRequest)
		return
	}
	if !isValidID(req.ID) {
		h.sendError(w, "Invalid issue ID format", http.StatusBadRequest)
		return
	}

	_, err := h.apiPost("/v0/bead/"+req.ID+"/close", nil)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to close issue: " + err.Error(),
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Issue closed",
	})
}

// IssueUpdateRequest is the request body for updating an issue.
type IssueUpdateRequest struct {
	ID       string `json:"id"`
	Status   string `json:"status,omitempty"`   // "open", "in_progress"
	Priority int    `json:"priority,omitempty"` // 1-4
	Assignee string `json:"assignee,omitempty"`
}

// handleIssueUpdate updates issue fields.
func (h *APIHandler) handleIssueUpdate(w http.ResponseWriter, r *http.Request) {
	var req IssueUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.sendError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.ID == "" {
		h.sendError(w, "Issue ID is required", http.StatusBadRequest)
		return
	}
	if !isValidID(req.ID) {
		h.sendError(w, "Invalid issue ID format", http.StatusBadRequest)
		return
	}

	hasUpdate := req.Status != "" || (req.Priority >= 1 && req.Priority <= 4) || req.Assignee != ""
	if !hasUpdate {
		h.sendError(w, "No update fields provided", http.StatusBadRequest)
		return
	}

	if req.Status != "" {
		switch req.Status {
		case "open", "in_progress":
		default:
			h.sendError(w, "Invalid status (allowed: open, in_progress)", http.StatusBadRequest)
			return
		}
	}

	if req.Assignee != "" && !isValidID(req.Assignee) {
		h.sendError(w, "Invalid assignee format", http.StatusBadRequest)
		return
	}

	apiReq := make(map[string]interface{})
	if req.Assignee != "" {
		apiReq["assignee"] = req.Assignee
	}
	_, err := h.apiPost("/v0/bead/"+req.ID+"/update", apiReq)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to update issue: " + err.Error(),
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Issue updated",
	})
}

// ---------- PR handler ----------

// PRShowResponse is the response for /api/pr/show.
type PRShowResponse struct {
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	Author       string   `json:"author"`
	URL          string   `json:"url"`
	Body         string   `json:"body"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Additions    int      `json:"additions"`
	Deletions    int      `json:"deletions"`
	ChangedFiles int      `json:"changed_files"`
	Mergeable    string   `json:"mergeable"`
	BaseRef      string   `json:"base_ref"`
	HeadRef      string   `json:"head_ref"`
	Labels       []string `json:"labels,omitempty"`
	Checks       []string `json:"checks,omitempty"`
	RawOutput    string   `json:"raw_output,omitempty"`
}

// handlePRShow returns details for a specific PR.
func (h *APIHandler) handlePRShow(w http.ResponseWriter, r *http.Request) {
	// Accept either repo/number or full URL
	repo := r.URL.Query().Get("repo")
	number := r.URL.Query().Get("number")
	prURL := r.URL.Query().Get("url")

	if prURL == "" && (repo == "" || number == "") {
		h.sendError(w, "Missing repo/number or url parameter", http.StatusBadRequest)
		return
	}

	// Validate inputs to prevent argument injection.
	if prURL != "" {
		const maxURLLen = 2000
		if len(prURL) > maxURLLen {
			h.sendError(w, fmt.Sprintf("PR URL too long (max %d bytes)", maxURLLen), http.StatusBadRequest)
			return
		}
		if strings.ContainsAny(prURL, "\x00\n\r") {
			h.sendError(w, "PR URL cannot contain null bytes or newlines", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(prURL, "https://") {
			h.sendError(w, "PR URL must start with https://", http.StatusBadRequest)
			return
		}
	} else {
		if !isNumeric(number) {
			h.sendError(w, "Invalid PR number format", http.StatusBadRequest)
			return
		}
		if !isValidRepoRef(repo) {
			h.sendError(w, "Invalid repo format (expected owner/repo)", http.StatusBadRequest)
			return
		}
	}

	var args []string
	if prURL != "" {
		args = []string{"pr", "view", prURL, "--json", "number,title,state,author,url,body,createdAt,updatedAt,additions,deletions,changedFiles,mergeable,baseRefName,headRefName,labels,statusCheckRollup"}
	} else {
		args = []string{"pr", "view", number, "--repo", repo, "--json", "number,title,state,author,url,body,createdAt,updatedAt,additions,deletions,changedFiles,mergeable,baseRefName,headRefName,labels,statusCheckRollup"}
	}

	output, err := h.runGhCommand(r.Context(), 15*time.Second, args)
	if err != nil {
		h.sendError(w, "Failed to fetch PR: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse the JSON output
	resp := parsePRShowOutput(output)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// runGhCommand executes a gh command with the given args.
func (h *APIHandler) runGhCommand(ctx context.Context, timeout time.Duration, args []string) (string, error) {
	return h.runCommandWithSem(ctx, timeout, "gh", args, h.cityPath)
}

// parsePRShowOutput parses the JSON output from "gh pr view --json".
func parsePRShowOutput(jsonStr string) PRShowResponse {
	resp := PRShowResponse{
		RawOutput: jsonStr,
	}

	var data struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		URL          string `json:"url"`
		Body         string `json:"body"`
		CreatedAt    string `json:"createdAt"`
		UpdatedAt    string `json:"updatedAt"`
		Additions    int    `json:"additions"`
		Deletions    int    `json:"deletions"`
		ChangedFiles int    `json:"changedFiles"`
		Mergeable    string `json:"mergeable"`
		BaseRefName  string `json:"baseRefName"`
		HeadRefName  string `json:"headRefName"`
		Labels       []struct {
			Name string `json:"name"`
		} `json:"labels"`
		StatusCheckRollup []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"statusCheckRollup"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return resp
	}

	resp.Number = data.Number
	resp.Title = data.Title
	resp.State = data.State
	resp.Author = data.Author.Login
	resp.URL = data.URL
	resp.Body = data.Body
	resp.CreatedAt = data.CreatedAt
	resp.UpdatedAt = data.UpdatedAt
	resp.Additions = data.Additions
	resp.Deletions = data.Deletions
	resp.ChangedFiles = data.ChangedFiles
	resp.Mergeable = data.Mergeable
	resp.BaseRef = data.BaseRefName
	resp.HeadRef = data.HeadRefName

	for _, label := range data.Labels {
		resp.Labels = append(resp.Labels, label.Name)
	}

	for _, check := range data.StatusCheckRollup {
		status := check.Name + ": "
		if check.Conclusion != "" {
			status += check.Conclusion
		} else {
			status += check.Status
		}
		resp.Checks = append(resp.Checks, status)
	}

	// Clear raw output if parsing succeeded
	resp.RawOutput = ""

	return resp
}

// ---------- Crew handler ----------

// CrewMember represents a crew member's status for the dashboard.
type CrewMember struct {
	Name       string `json:"name"`
	Rig        string `json:"rig"`
	State      string `json:"state"` // spinning, finished, ready, questions
	Hook       string `json:"hook,omitempty"`
	HookTitle  string `json:"hook_title,omitempty"`
	Session    string `json:"session"` // attached, detached, none
	LastActive string `json:"last_active"`
}

// CrewResponse is the response for /api/crew.
type CrewResponse struct {
	Crew  []CrewMember            `json:"crew"`
	ByRig map[string][]CrewMember `json:"by_rig"`
	Total int                     `json:"total"`
}

// handleCrew returns crew status across all rigs with proper state detection.
func (h *APIHandler) handleCrew(w http.ResponseWriter, _ *http.Request) {
	h.handleCrewAPI(w)
}

func (h *APIHandler) handleCrewAPI(w http.ResponseWriter) {
	resp := CrewResponse{
		Crew:  make([]CrewMember, 0),
		ByRig: make(map[string][]CrewMember),
	}

	itemsRaw, err := h.apiGetListRaw("/v0/sessions?state=active&peek=true")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	var sessions []apiSessionResponse
	if err := json.Unmarshal(itemsRaw, &sessions); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	for _, sess := range sessions {
		state := "ready"
		sessionStatus := "none"
		lastActive := ""

		if sess.Running {
			if sess.Attached {
				sessionStatus = "attached"
			} else {
				sessionStatus = "detached"
			}
			la := parseTime(sess.LastActive)
			if !la.IsZero() {
				lastActive = formatTimestamp(la)
				activityAge := time.Since(la)
				if activityAge < 10*time.Minute {
					state = "spinning"
				} else {
					state = "questions"
				}
			} else {
				state = "spinning"
			}
		} else if sess.ActiveBead != "" {
			state = "finished"
		}

		// Refine "questions" state: check for pending interaction via session API.
		if state == "questions" || state == "finished" {
			if h.hasSessionPendingInteraction(sess.ID) {
				state = "questions"
			}
		}

		member := CrewMember{
			Name:       sess.Template,
			Rig:        sess.Rig,
			State:      state,
			Hook:       sess.ActiveBead,
			Session:    sessionStatus,
			LastActive: lastActive,
		}
		resp.Crew = append(resp.Crew, member)
		resp.ByRig[sess.Rig] = append(resp.ByRig[sess.Rig], member)
	}
	resp.Total = len(resp.Crew)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// hasSessionPendingInteraction checks if a session has a pending interaction
// (permission prompt, question) via the session pending API.
func (h *APIHandler) hasSessionPendingInteraction(sessionID string) bool {
	body, err := h.apiGet("/v0/session/" + sessionID + "/pending")
	if err != nil {
		return false
	}
	var resp struct {
		Supported bool `json:"supported"`
		Pending   *struct {
			Type string `json:"type"`
		} `json:"pending"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return false
	}
	return resp.Supported && resp.Pending != nil
}

// ---------- Ready handler ----------

// ReadyItem represents a ready work item.
type ReadyItem struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Priority int    `json:"priority"`
	Source   string `json:"source"` // rig name or "city"
	Type     string `json:"type"`   // issue, task, etc.
}

// ReadyResponse is the response for /api/ready.
type ReadyResponse struct {
	Items    []ReadyItem            `json:"items"`
	BySource map[string][]ReadyItem `json:"by_source"`
	Summary  struct {
		Total   int `json:"total"`
		P1Count int `json:"p1_count"`
		P2Count int `json:"p2_count"`
		P3Count int `json:"p3_count"`
	} `json:"summary"`
}

// handleReady returns ready work items across the city.
func (h *APIHandler) handleReady(w http.ResponseWriter, _ *http.Request) {
	h.handleReadyAPI(w)
}

func (h *APIHandler) handleReadyAPI(w http.ResponseWriter) {
	resp := ReadyResponse{
		Items:    make([]ReadyItem, 0),
		BySource: make(map[string][]ReadyItem),
	}

	itemsRaw, err := h.apiGetListRaw("/v0/beads/ready")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	var readyBeads []apiBead
	if err := json.Unmarshal(itemsRaw, &readyBeads); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	for _, b := range readyBeads {
		item := ReadyItem{
			ID:     b.ID,
			Title:  b.Title,
			Source: "city",
			Type:   b.Type,
		}
		resp.Items = append(resp.Items, item)
		resp.BySource["city"] = append(resp.BySource["city"], item)
	}
	resp.Summary.Total = len(resp.Items)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---------- Session preview handler ----------

// SessionPreviewResponse is the response for /api/session/preview.
type SessionPreviewResponse struct {
	Session   string `json:"session"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

// handleSessionPreview returns the last N lines of output for a session.
func (h *APIHandler) handleSessionPreview(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		h.sendError(w, "Missing session parameter", http.StatusBadRequest)
		return
	}

	for _, c := range sessionID {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '/' {
			h.sendError(w, "Invalid session ID: contains invalid characters", http.StatusBadRequest)
			return
		}
	}

	body, err := h.apiGet("/v0/session/" + sessionID + "/transcript?tail=1")
	if err != nil {
		h.sendError(w, "Failed to get session transcript: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var transcriptResp struct {
		Turns []struct {
			Text string `json:"text"`
		} `json:"turns"`
	}
	if json.Unmarshal(body, &transcriptResp) == nil {
		var parts []string
		for _, t := range transcriptResp.Turns {
			if t.Text != "" {
				parts = append(parts, t.Text)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SessionPreviewResponse{
			Session:   sessionID,
			Content:   strings.Join(parts, "\n"),
			Timestamp: time.Now().Format(time.RFC3339),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(SessionPreviewResponse{
		Session:   sessionID,
		Content:   string(body),
		Timestamp: time.Now().Format(time.RFC3339),
	})
}

// ---------- Agent logs handler ----------

// handleAgentOutput proxies the session transcript API.
func (h *APIHandler) handleAgentOutput(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("name")
	if sessionID == "" {
		h.sendError(w, "Missing name parameter", http.StatusBadRequest)
		return
	}

	for _, c := range sessionID {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '/' {
			h.sendError(w, "Invalid session ID", http.StatusBadRequest)
			return
		}
	}

	// Build upstream URL with query params.
	upstream := "/v0/session/" + sessionID + "/transcript"
	sep := "?"
	if v := r.URL.Query().Get("tail"); v != "" {
		upstream += sep + "tail=" + url.QueryEscape(v)
		sep = "&"
	}
	if v := r.URL.Query().Get("before"); v != "" {
		upstream += sep + "before=" + url.QueryEscape(v)
	}

	resp, err := h.apiClient.Get(h.apiURL + scopedPath(upstream, h.cityScope))
	if err != nil {
		h.sendError(w, "Failed to fetch session transcript", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// handleAgentOutputStream proxies the session stream SSE endpoint.
func (h *APIHandler) handleAgentOutputStream(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("name")
	if sessionID == "" {
		h.sendError(w, "Missing name parameter", http.StatusBadRequest)
		return
	}

	for _, c := range sessionID {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '/' {
			h.sendError(w, "Invalid session ID", http.StatusBadRequest)
			return
		}
	}

	upstream := h.apiURL + scopedPath("/v0/session/"+sessionID+"/stream", h.cityScope)
	req, err := http.NewRequestWithContext(r.Context(), "GET", upstream, nil)
	if err != nil {
		h.sendError(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	// Use a client without timeout for the long-lived SSE connection
	// (h.apiClient has a 15s timeout that would kill the stream).
	sseClient := &http.Client{Timeout: 0}
	resp, err := sseClient.Do(req)
	if err != nil {
		h.sendError(w, "Failed to connect to agent output stream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	// On upstream error, proxy the error response as JSON (not SSE).
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		_, _ = w.Write(body)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// Commit SSE headers only after confirming upstream success.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Stream the response body through.
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			flusher.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

// ---------- Command parsing ----------

// parseCommandArgs splits a command string into args, respecting quotes.
func parseCommandArgs(command string) []string {
	return shellquote.Split(command)
}

// ---------- SSE handler ----------

// handleSSE streams Server-Sent Events to the dashboard client.
func (h *APIHandler) handleSSE(w http.ResponseWriter, r *http.Request) {
	h.handleSSEProxy(w, r)
}

// handleSSEProxy proxies the API server's SSE event stream to the browser.
func (h *APIHandler) handleSSEProxy(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Connect to API event stream. Resume from after_seq if provided.
	// The client sends after_seq as a query param (manual EventSource creation
	// doesn't send Last-Event-ID header), so check both sources. The dashboard
	// always connects to per-city streams (cityScope set via meta tag or URL
	// param), so after_seq is always the correct parameter.
	sseURL := h.apiURL + scopedPath("/v0/events/stream", h.cityScope)
	afterSeq := r.URL.Query().Get("after_seq")
	if afterSeq == "" {
		afterSeq = r.Header.Get("Last-Event-ID")
	}
	if afterSeq != "" {
		sseURL += "?after_seq=" + url.QueryEscape(afterSeq)
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, sseURL, nil)
	if err != nil {
		http.Error(w, "SSE request failed", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "text/event-stream")

	// Use a client without timeout for the long-lived SSE connection.
	sseClient := &http.Client{Timeout: 0}
	resp, err := sseClient.Do(req)
	if err != nil {
		http.Error(w, "SSE connect failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close

	// On upstream error, forward the error response as-is so the browser's
	// EventSource sees a non-200 status and backs off properly. Without this
	// check, the proxy writes 200 + "event: connected" unconditionally,
	// which resets the browser's reconnect backoff to 1s and creates a tight
	// 404 retry loop when the city is not yet running.
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send initial connection event.
	fmt.Fprintf(w, "event: connected\ndata: ok\n\n") //nolint:errcheck // best-effort SSE write
	flusher.Flush()

	// Proxy the upstream SSE stream, preserving event types and IDs.
	// Upstream format: "event: <type>\nid: <seq>\ndata: <json>\n\n"
	// We forward: "event: gc-event\nid: <seq>\ndata: <json>\n\n"
	// The browser parses the event type from the JSON data payload and
	// uses Last-Event-ID for automatic reconnection on disconnect.
	//
	// Fields are buffered until the blank-line delimiter, then emitted as a
	// complete event. This eliminates any dependency on field ordering
	// (the SSE spec does not mandate id-before-data).
	scanner := bufio.NewScanner(resp.Body)
	var currentID, currentData string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			currentID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			currentData = strings.TrimPrefix(line, "data:")
		case line == "":
			// Blank line = SSE event delimiter. Emit buffered event.
			if currentData != "" {
				if currentID != "" {
					fmt.Fprintf(w, "event: gc-event\nid: %s\ndata:%s\n\n", currentID, currentData) //nolint:errcheck // best-effort SSE write
				} else {
					fmt.Fprintf(w, "event: gc-event\ndata:%s\n\n", currentData) //nolint:errcheck // best-effort SSE write
				}
				flusher.Flush()
			}
			currentID, currentData = "", ""
		case strings.HasPrefix(line, ":"):
			// Forward keepalive comments to prevent connection timeout.
			fmt.Fprintf(w, ": keepalive\n\n") //nolint:errcheck // best-effort SSE write
			flusher.Flush()
		}
	}
}

// mailPriorityString converts numeric mail priority to display string.
func mailPriorityString(p int) string {
	switch p {
	case 0:
		return "urgent"
	case 1:
		return "high"
	case 2:
		return "normal"
	case 3, 4:
		return "low"
	default:
		return "normal"
	}
}

// prettyJSON formats raw JSON bytes with indentation for display.
func prettyJSON(raw []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") == nil {
		return buf.String()
	}
	return string(raw)
}
