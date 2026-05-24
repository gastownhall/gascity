package cloudflare

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

// Start creates a new remote session.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	body := startConfigFromRuntime(cfg)
	if err := p.do(ctx, p.startTimeout, http.MethodPost, []string{"sessions", name, "start"}, body, nil); err != nil {
		return err
	}
	return nil
}

// Stop destroys the named remote session. Missing sessions are treated as
// already stopped.
func (p *Provider) Stop(name string) error {
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "stop"}, nil, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// Interrupt sends a best-effort interrupt to the named remote session.
func (p *Provider) Interrupt(name string) error {
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "interrupt"}, nil, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// IsRunning reports whether the named remote session is running.
func (p *Provider) IsRunning(name string) bool {
	var out statusResponse
	if err := p.do(context.Background(), p.timeout, http.MethodGet, []string{"sessions", name, "running"}, nil, &out); err != nil {
		return false
	}
	return out.Running
}

// IsAttached reports whether an operator is attached to the named remote session.
func (p *Provider) IsAttached(name string) bool {
	var out statusResponse
	if err := p.do(context.Background(), p.timeout, http.MethodGet, []string{"sessions", name, "attached"}, nil, &out); err != nil {
		return false
	}
	return out.Attached
}

// Attach is not supported because the Cloudflare runtime API has no local TTY.
func (p *Provider) Attach(_ string) error {
	return fmt.Errorf("cloudflare provider does not support attach")
}

// ProcessAlive reports whether the expected process is alive in the remote
// session.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	var out statusResponse
	body := processAliveRequest{ProcessNames: processNames}
	if err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "process-alive"}, body, &out); err != nil {
		return false
	}
	return out.Alive
}

// Nudge sends structured content to the named remote session.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	return p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "nudge"}, nudgeRequest{Content: content}, nil)
}

// SetMeta stores session metadata in the remote runtime.
func (p *Provider) SetMeta(name, key, value string) error {
	return p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "meta", key}, metaRequest{Value: value}, nil)
}

// GetMeta retrieves session metadata from the remote runtime.
func (p *Provider) GetMeta(name, key string) (string, error) {
	var out metaResponse
	err := p.do(context.Background(), p.timeout, http.MethodGet, []string{"sessions", name, "meta", key}, nil, &out)
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
	err := p.do(context.Background(), p.timeout, http.MethodDelete, []string{"sessions", name, "meta", key}, nil, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// Peek captures recent output from the named remote session.
func (p *Provider) Peek(name string, lines int) (string, error) {
	var out peekResponse
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "peek"}, peekRequest{Lines: lines}, &out)
	if err != nil {
		return "", err
	}
	return out.Output, nil
}

// ListRunning returns remote session names with the given prefix.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	var out listResponse
	err := p.doList(context.Background(), prefix, &out)
	if err != nil {
		return nil, err
	}
	return out.Names, nil
}

// GetLastActivity returns the last remote activity timestamp.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	var out activityResponse
	err := p.do(context.Background(), p.timeout, http.MethodGet, []string{"sessions", name, "last-activity"}, nil, &out)
	if err != nil {
		return time.Time{}, err
	}
	if strings.TrimSpace(out.LastActivity) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, out.LastActivity)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing cloudflare last activity for %q: %w", name, err)
	}
	return t, nil
}

// ClearScrollback clears the remote output buffer.
func (p *Provider) ClearScrollback(name string) error {
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "clear-scrollback"}, nil, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// CopyTo asks the remote runtime to copy a host-visible path into the session.
func (p *Provider) CopyTo(name, src, relDst string) error {
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat copy source %q: %w", src, err)
	}
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "copy-to"}, copyRequest{Src: src, RelDst: relDst}, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// SendKeys sends raw key tokens to the remote session.
func (p *Provider) SendKeys(name string, keys ...string) error {
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "send-keys"}, sendKeysRequest{Keys: keys}, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// RunLive reapplies live session commands to the remote session.
func (p *Provider) RunLive(name string, cfg runtime.Config) error {
	err := p.do(context.Background(), p.timeout, http.MethodPost, []string{"sessions", name, "run-live"}, runLiveRequest{Config: startConfigFromRuntime(cfg)}, nil)
	if runtime.IsSessionGone(err) {
		return nil
	}
	return err
}

// Capabilities reports Cloudflare runtime observation support.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportActivity: p.reportActivity,
	}
}

func (p *Provider) doList(ctx context.Context, prefix string, out any) error {
	u := p.urlFor("sessions")
	q := u.Query()
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	u.RawQuery = q.Encode()
	return p.doURL(ctx, p.timeout, http.MethodGet, u.String(), nil, out)
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
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("reading cloudflare runtime response: %w", err)
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

type statusResponse struct {
	Running  bool `json:"running,omitempty"`
	Attached bool `json:"attached,omitempty"`
	Alive    bool `json:"alive,omitempty"`
}

type processAliveRequest struct {
	ProcessNames []string `json:"process_names,omitempty"`
}

type nudgeRequest struct {
	Content []runtime.ContentBlock `json:"content,omitempty"`
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

type listResponse struct {
	Names []string `json:"names"`
}

type activityResponse struct {
	LastActivity string `json:"last_activity,omitempty"`
}

type copyRequest struct {
	Src    string `json:"src"`
	RelDst string `json:"rel_dst,omitempty"`
}

type sendKeysRequest struct {
	Keys []string `json:"keys,omitempty"`
}

type runLiveRequest struct {
	Config startConfig `json:"config"`
}

func isSessionGone(err error) bool {
	return err != nil && (runtime.IsSessionGone(err) || errors.Is(err, runtime.ErrSessionNotFound))
}
