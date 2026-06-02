// Package cloudflare implements [runtime.Provider] using a Cloudflare Worker
// runtime API.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	defaultTimeout      = 30 * time.Second
	defaultStartTimeout = 120 * time.Second
	maxResponseBytes    = 1 << 20
)

// Config holds Cloudflare runtime provider settings.
type Config struct {
	// Endpoint is the base URL for the Cloudflare Worker runtime API.
	Endpoint string
	// Token is an optional bearer token sent to the Worker runtime API.
	Token string
	// Timeout bounds non-start runtime operations.
	Timeout time.Duration
	// StartTimeout bounds session start operations.
	StartTimeout time.Duration
	// Client is the HTTP client used to call the Worker runtime API.
	Client *http.Client
	// ReportActivity enables CanReportActivity in provider capabilities.
	ReportActivity bool
}

// Provider manages sessions through a Cloudflare Worker runtime API.
type Provider struct {
	endpoint       *url.URL
	token          string
	timeout        time.Duration
	startTimeout   time.Duration
	client         *http.Client
	reportActivity bool
}

var _ runtime.Provider = (*Provider)(nil)

// NewProvider creates a Cloudflare runtime provider from environment variables.
//
// Required:
//   - GC_CLOUDFLARE_RUNTIME_URL
//
// Optional:
//   - GC_CLOUDFLARE_RUNTIME_TOKEN
func NewProvider() (*Provider, error) {
	return NewProviderWithConfig(Config{
		Endpoint: os.Getenv("GC_CLOUDFLARE_RUNTIME_URL"),
		Token:    os.Getenv("GC_CLOUDFLARE_RUNTIME_TOKEN"),
	})
}

// NewProviderWithConfig creates a Cloudflare runtime provider.
func NewProviderWithConfig(cfg Config) (*Provider, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("cloudflare runtime endpoint is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing cloudflare runtime endpoint: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("cloudflare runtime endpoint must be an absolute URL")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	startTimeout := cfg.StartTimeout
	if startTimeout <= 0 {
		startTimeout = defaultStartTimeout
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	return &Provider{
		endpoint:       parsed,
		token:          cfg.Token,
		timeout:        timeout,
		startTimeout:   startTimeout,
		client:         client,
		reportActivity: cfg.ReportActivity,
	}, nil
}

// Start creates a new remote session. Maps to POST /session.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	body := startRequest{SessionID: name, Config: startConfigFromRuntime(cfg)}
	return p.do(ctx, p.startTimeout, http.MethodPost, []string{"session"}, body, nil)
}

// Stop destroys the named remote session. Missing sessions are treated as
// already stopped.
func (p *Provider) Stop(name string) error {
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"session", name, "stop"}, nil, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// Interrupt sends a best-effort SIGINT to user-owned processes in the remote
// session. Targets the current user only to avoid signaling system daemons in
// the shared container environment.
func (p *Provider) Interrupt(name string) error {
	err := p.exec(context.Background(), name, `pkill -INT -u "$(id -u)" 2>/dev/null; true`, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// IsRunning reports whether the named remote session is running.
func (p *Provider) IsRunning(name string) bool {
	var out sessionStatusResponse
	if err := p.do(context.Background(), p.timeout, http.MethodGet, []string{"session", name, "status"}, nil, &out); err != nil {
		return false
	}
	return out.Alive
}

// IsAttached always returns false: the Cloudflare runtime has no local TTY and
// CanReportAttachment is false in Capabilities.
func (p *Provider) IsAttached(_ string) bool {
	return false
}

// Attach is not supported because the Cloudflare runtime API has no local TTY.
func (p *Provider) Attach(_ string) error {
	return fmt.Errorf("cloudflare provider does not support attach")
}

// ProcessAlive reports whether any of the named processes are alive in the
// remote session via a pgrep exec. Uses -E (extended regex) so the | alternation
// is interpreted correctly, and -- to guard against process names starting with -.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	pattern := shellQuoteSingle(strings.Join(processNames, "|"))
	var out execResponse
	if err := p.exec(context.Background(), name, "pgrep -Ef -- "+pattern, &out); err != nil {
		return false
	}
	return out.ExitCode == 0
}

// Nudge sends content blocks to the named remote session. Uses FlattenText so
// file_path blocks emit a visible placeholder rather than being silently dropped.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	text := runtime.FlattenText(content)
	if text == "" {
		return nil
	}
	return p.do(context.Background(), p.timeout, http.MethodPost, []string{"session", name, "nudge"}, nudgeRequest{Text: text}, nil)
}

// SetMeta stores session metadata in the remote runtime.
func (p *Provider) SetMeta(name, key, value string) error {
	return p.do(context.Background(), p.timeout, http.MethodPost, []string{"session", name, "meta", key}, metaRequest{Value: value}, nil)
}

// GetMeta retrieves session metadata from the remote runtime.
func (p *Provider) GetMeta(name, key string) (string, error) {
	var out metaResponse
	err := p.do(context.Background(), p.timeout, http.MethodGet, []string{"session", name, "meta", key}, nil, &out)
	if runtime.IsSessionGone(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return out.Value, nil
}

// RemoveMeta removes session metadata from the remote runtime.
func (p *Provider) RemoveMeta(name, key string) error {
	err := p.do(context.Background(), p.timeout, http.MethodDelete, []string{"session", name, "meta", key}, nil, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// Peek captures recent output from the named remote session.
func (p *Provider) Peek(name string, lines int) (string, error) {
	var out peekResponse
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"session", name, "peek"}, peekRequest{Lines: lines}, &out)
	if err != nil {
		return "", err
	}
	return out.Output, nil
}

// ListRunning is not supported: the Cloudflare Worker has no session index endpoint.
func (p *Provider) ListRunning(_ string) ([]string, error) {
	return nil, fmt.Errorf("cloudflare provider does not support ListRunning")
}

// GetLastActivity returns the session creation time from the status record.
// The Cloudflare Worker embeds this in GET /session/:name/status as record.createdAt.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	var out sessionStatusResponse
	if err := p.do(context.Background(), p.timeout, http.MethodGet, []string{"session", name, "status"}, nil, &out); err != nil {
		return time.Time{}, err
	}
	ts := strings.TrimSpace(out.Record.CreatedAt)
	if ts == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing cloudflare last activity for %q: %w", name, err)
	}
	return t, nil
}

// ClearScrollback clears the remote output buffer via exec.
func (p *Provider) ClearScrollback(name string) error {
	err := p.exec(context.Background(), name, "truncate -s0 /workspace/.gc-scrollback 2>/dev/null || true", nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// CopyTo is not supported: the Cloudflare runtime has no host-to-sandbox file
// transfer endpoint in this version of the Worker API.
func (p *Provider) CopyTo(_, _, _ string) error {
	return fmt.Errorf("cloudflare provider does not support CopyTo")
}

// SendKeys sends raw key tokens to the remote session.
func (p *Provider) SendKeys(name string, keys ...string) error {
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"session", name, "keys"}, sendKeysRequest{Keys: keys}, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// RunLive is a best-effort no-op: the Cloudflare Worker has no session-live
// replay endpoint. Returns nil so the reconciler can persist the live-hash and
// stop re-triggering on every tick.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil
}

// Capabilities reports Cloudflare runtime observation support.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportActivity: p.reportActivity,
	}
}

// exec posts a shell command to POST /session/:name/exec and decodes the result into out (may be nil).
func (p *Provider) exec(ctx context.Context, name, cmd string, out any) error {
	return p.do(ctx, p.timeout, http.MethodPost, []string{"session", name, "exec"}, execRequest{Cmd: cmd}, out)
}

func (p *Provider) do(ctx context.Context, timeout time.Duration, method string, parts []string, body any, out any) error {
	return p.doURL(ctx, timeout, method, p.urlFor(parts...).String(), body, out)
}

func (p *Provider) doURL(parent context.Context, timeout time.Duration, method, target string, body any, out any) error {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = p.timeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling cloudflare runtime request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("building cloudflare runtime request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare runtime request: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return fmt.Errorf("reading cloudflare runtime response: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing cloudflare runtime response: %w", closeErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cloudflareStatusError(resp.StatusCode, target, data)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding cloudflare runtime response: %w", err)
	}
	return nil
}

func (p *Provider) urlFor(parts ...string) *url.URL {
	u := *p.endpoint
	base := strings.TrimRight(u.Path, "/")
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, url.PathEscape(part))
	}
	if len(escaped) > 0 {
		u.Path = base + "/" + strings.Join(escaped, "/")
	} else {
		u.Path = base
	}
	return &u
}

func cloudflareStatusError(status int, target string, data []byte) error {
	msg := statusText(status, data)
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: cloudflare runtime %s: %s", runtime.ErrSessionNotFound, target, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: cloudflare runtime %s: %s", runtime.ErrSessionExists, target, msg)
	default:
		return fmt.Errorf("cloudflare runtime %s: status %d: %s", target, status, msg)
	}
}

func statusText(status int, data []byte) string {
	var payload errorResponse
	if err := json.Unmarshal(data, &payload); err == nil {
		switch {
		case payload.Error != "":
			return payload.Error
		case payload.Message != "":
			return payload.Message
		}
	}
	text := strings.TrimSpace(string(data))
	if text != "" {
		return text
	}
	return http.StatusText(status)
}

// shellQuoteSingle wraps s in single quotes, escaping any embedded single quotes.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

type errorResponse struct {
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

type copyEntry struct {
	Src    string `json:"src"`
	RelDst string `json:"rel_dst,omitempty"`
}

type startConfig struct {
	WorkDir                string                    `json:"work_dir,omitempty"`
	Command                string                    `json:"command,omitempty"`
	Env                    map[string]string         `json:"env,omitempty"`
	MCPServers             []runtime.MCPServerConfig `json:"mcp_servers,omitempty"`
	ReadyPromptPrefix      string                    `json:"ready_prompt_prefix,omitempty"`
	ReadyDelayMs           int                       `json:"ready_delay_ms,omitempty"`
	ProcessNames           []string                  `json:"process_names,omitempty"`
	EmitsPermissionWarning bool                      `json:"emits_permission_warning,omitempty"`
	Nudge                  string                    `json:"nudge,omitempty"`
	PreStart               []string                  `json:"pre_start,omitempty"`
	SessionSetup           []string                  `json:"session_setup,omitempty"`
	SessionSetupScript     string                    `json:"session_setup_script,omitempty"`
	SessionLive            []string                  `json:"session_live,omitempty"`
	ProviderName           string                    `json:"provider_name,omitempty"`
	ProviderOverlayName    string                    `json:"provider_overlay_name,omitempty"`
	InstallAgentHooks      []string                  `json:"install_agent_hooks,omitempty"`
	PackOverlayDirs        []string                  `json:"pack_overlay_dirs,omitempty"`
	OverlayDir             string                    `json:"overlay_dir,omitempty"`
	CopyFiles              []copyEntry               `json:"copy_files,omitempty"`
	FingerprintExtra       map[string]string         `json:"fingerprint_extra,omitempty"`
	PromptSuffix           string                    `json:"prompt_suffix,omitempty"`
	PromptFlag             string                    `json:"prompt_flag,omitempty"`
}

// startRequest is the body for POST /session.
type startRequest struct {
	SessionID string      `json:"sessionId"`
	Config    startConfig `json:"config,omitempty"`
}

func startConfigFromRuntime(cfg runtime.Config) startConfig {
	copyFiles := make([]copyEntry, 0, len(cfg.CopyFiles))
	for _, entry := range cfg.CopyFiles {
		copyFiles = append(copyFiles, copyEntry{
			Src:    entry.Src,
			RelDst: entry.RelDst,
		})
	}
	return startConfig{
		WorkDir:                cfg.WorkDir,
		Command:                cfg.Command,
		Env:                    cfg.Env,
		MCPServers:             cfg.MCPServers,
		ReadyPromptPrefix:      cfg.ReadyPromptPrefix,
		ReadyDelayMs:           cfg.ReadyDelayMs,
		ProcessNames:           cfg.ProcessNames,
		EmitsPermissionWarning: cfg.EmitsPermissionWarning,
		Nudge:                  cfg.Nudge,
		PreStart:               cfg.PreStart,
		SessionSetup:           cfg.SessionSetup,
		SessionSetupScript:     cfg.SessionSetupScript,
		SessionLive:            cfg.SessionLive,
		ProviderName:           cfg.ProviderName,
		ProviderOverlayName:    cfg.ProviderOverlayName,
		InstallAgentHooks:      cfg.InstallAgentHooks,
		PackOverlayDirs:        cfg.PackOverlayDirs,
		OverlayDir:             cfg.OverlayDir,
		CopyFiles:              copyFiles,
		FingerprintExtra:       cfg.FingerprintExtra,
		PromptSuffix:           cfg.PromptSuffix,
		PromptFlag:             cfg.PromptFlag,
	}
}

type execRequest struct {
	Cmd string `json:"cmd"`
}

type execResponse struct {
	ExitCode int  `json:"exitCode"`
	Success  bool `json:"success"`
}

// sessionStatusResponse is the shape of GET /session/:name/status.
type sessionStatusResponse struct {
	Alive  bool `json:"alive"`
	Record struct {
		CreatedAt string `json:"createdAt"`
	} `json:"record"`
}

type nudgeRequest struct {
	Text string `json:"text"`
}

type metaRequest struct {
	Value string `json:"value"`
}

type metaResponse struct {
	Value string `json:"value"`
}

type peekRequest struct {
	Lines int `json:"lines,omitempty"`
}

type peekResponse struct {
	Output string `json:"output"`
}

type sendKeysRequest struct {
	Keys []string `json:"keys,omitempty"`
}
