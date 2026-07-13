// Package t3bridge provides a native Go runtime.Provider that talks directly
// to the T3 WebSocket API for T3 session lifecycle and turn operations, using
// an internal exec shim only for a small remaining helper surface.
package t3bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var (
	authMu                       sync.Mutex
	cachedBridgeWSToken          string
	cachedBridgeWSTokenBaseURL   string
	cachedBridgeWSTokenExpiresAt time.Time
)

var _ runtime.Provider = (*Provider)(nil)

var defaultWSURLCandidates = []string{
	"ws://127.0.0.1:3773/ws",
	"ws://localhost:3773/ws",
}

const (
	bridgeConnectRetryWindow = 8 * time.Second
	bridgeConnectBaseDelay   = 100 * time.Millisecond
	bridgeConnectMaxDelay    = 1 * time.Second
	bridgeHTTPTimeout        = 12 * time.Second
	bridgeWSTimeout          = 3 * time.Second
	snapshotCacheTTL         = 10 * time.Second
)

// Provider wraps an exec.Provider, moving the T3-specific lifecycle and turn
// operations into native Go WebSocket calls while leaving a small helper
// surface on the internal exec shim.
type Provider struct {
	mu           sync.Mutex
	reqSeq       int
	watchers     map[string]context.CancelFunc
	recentStarts map[string]time.Time

	// Cached snapshot for batching multiple IsRunning/ProcessAlive calls.
	snapshotSnapshot map[string]interface{}
	snapshotCache    map[string]string // threadID → session status
	snapshotCacheAt  time.Time
}

type threadBinding struct {
	ProjectID       string          `json:"projectId"`
	ThreadID        string          `json:"threadId"`
	SessionName     string          `json:"sessionName"`
	Agent           string          `json:"agent"`
	WorkDir         string          `json:"workDir"`
	Provider        string          `json:"provider"`
	Model           string          `json:"model"`
	StartupEnvelope json.RawMessage `json:"startupEnvelope"`
}

type execCopyEntry struct {
	Src    string `json:"src"`
	RelDst string `json:"rel_dst,omitempty"`
}

type execStartConfig struct {
	WorkDir            string            `json:"work_dir,omitempty"`
	Command            string            `json:"command,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	StartupEnvelope    json.RawMessage   `json:"startup_envelope,omitempty"`
	ProcessNames       []string          `json:"process_names,omitempty"`
	Nudge              string            `json:"nudge,omitempty"`
	ReadyPromptPrefix  string            `json:"ready_prompt_prefix,omitempty"`
	ReadyDelayMs       int               `json:"ready_delay_ms,omitempty"`
	PreStart           []string          `json:"pre_start,omitempty"`
	SessionSetup       []string          `json:"session_setup,omitempty"`
	SessionSetupScript string            `json:"session_setup_script,omitempty"`
	SessionLive        []string          `json:"session_live,omitempty"`
	PackOverlayDirs    []string          `json:"pack_overlay_dirs,omitempty"`
	OverlayDir         string            `json:"overlay_dir,omitempty"`
	CopyFiles          []execCopyEntry   `json:"copy_files,omitempty"`
}

// resolveWsURLCandidates reads the t3code WebSocket URL from the environment,
// the ws-url file, and finally stable loopback defaults. Called on each
// connection so a t3code restart with a new port/token is picked up without
// restarting gc, while still surviving stale env/file state.
func resolveWsURLCandidates() []string {
	candidates := make([]string, 0, 4)
	seen := make(map[string]struct{})
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		if _, ok := seen[raw]; ok {
			return
		}
		seen[raw] = struct{}{}
		candidates = append(candidates, raw)
	}
	add(os.Getenv("T3_WS_URL"))
	if runtimeURL, err := readRuntimeWSURL(); err == nil {
		add(runtimeURL)
	}
	t3Home := os.Getenv("T3_HOME")
	if t3Home == "" {
		t3Home = filepath.Join(os.Getenv("HOME"), ".t3")
	}
	if urlBytes, err := os.ReadFile(filepath.Join(t3Home, "ws-url")); err == nil {
		add(string(urlBytes))
	}
	for _, candidate := range defaultWSURLCandidates {
		add(candidate)
	}
	return candidates
}

func readRuntimeWSURL() (string, error) {
	type runtimeState struct {
		Origin string `json:"origin"`
	}

	for _, path := range resolveRuntimeStatePaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		var state runtimeState
		if err := json.Unmarshal(data, &state); err != nil {
			return "", fmt.Errorf("decode runtime state %s: %w", path, err)
		}
		if strings.TrimSpace(state.Origin) == "" {
			continue
		}
		wsURL, err := httpOriginToWSURL(state.Origin)
		if err != nil {
			return "", fmt.Errorf("derive ws url from runtime state %s: %w", path, err)
		}
		return wsURL, nil
	}
	return "", nil
}

func resolveRuntimeStatePaths() []string {
	home := os.Getenv("HOME")
	explicitT3Home := strings.TrimSpace(os.Getenv("T3_HOME")) != ""
	baseDir := resolveT3BaseDir()
	paths := make([]string, 0, 4)
	seen := make(map[string]struct{})
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	add(filepath.Join(baseDir, "server-runtime.json"))
	add(filepath.Join(baseDir, "dev", "server-runtime.json"))
	if !explicitT3Home && strings.TrimSpace(home) != "" {
		add(filepath.Join(home, ".t3", "server-runtime.json"))
		add(filepath.Join(home, ".t3", "dev", "server-runtime.json"))
	}
	return paths
}

func httpOriginToWSURL(origin string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported runtime origin scheme: %s", parsed.Scheme)
	}
	parsed.Path = "/ws"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func resolveWsURL() string {
	candidates := resolveWsURLCandidates()
	if len(candidates) == 0 {
		return "ws://127.0.0.1:3773/ws"
	}
	return candidates[0]
}

func resolveT3BaseDir() string {
	if v := os.Getenv("T3_HOME"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v := os.Getenv("T3_BASE_DIR"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v := os.Getenv("T3CODE_HOME"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return filepath.Join(os.Getenv("HOME"), ".t3")
}

func decodeIssuedBearerSessionToken(output []byte) (string, error) {
	var payload struct {
		Token        string `json:"token"`
		SessionToken string `json:"sessionToken"`
	}
	if err := json.Unmarshal(output, &payload); err != nil {
		return "", fmt.Errorf("decode json: %w", err)
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		token = strings.TrimSpace(payload.SessionToken)
	}
	if token == "" {
		return "", fmt.Errorf("empty token")
	}
	return token, nil
}

func resolveBearerSessionToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv("T3_BEARER_TOKEN")); token != "" {
		return token, nil
	}
	return readCachedBearerSessionToken()
}

func issueT3WebSocketToken(wsURL string) (string, error) {
	authMu.Lock()
	defer authMu.Unlock()

	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("parse ws url: %w", err)
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported websocket scheme: %s", parsed.Scheme)
	}
	parsed.Path = "/api/auth/bridge-ws-token"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	baseURL := strings.TrimRight(parsed.String(), "/")

	now := time.Now()
	if cachedBridgeWSToken != "" &&
		cachedBridgeWSTokenBaseURL == baseURL &&
		cachedBridgeWSTokenExpiresAt.Sub(now) > time.Minute {
		return cachedBridgeWSToken, nil
	}

	req, err := http.NewRequest(http.MethodPost, baseURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("build bridge ws-token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: bridgeHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request bridge ws token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("request bridge ws token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode ws token: %w", err)
	}
	if strings.TrimSpace(payload.Token) == "" {
		return "", fmt.Errorf("decode ws token: empty token")
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(payload.ExpiresAt))
	if err == nil {
		cachedBridgeWSToken = strings.TrimSpace(payload.Token)
		cachedBridgeWSTokenBaseURL = baseURL
		cachedBridgeWSTokenExpiresAt = expiresAt
	}
	return payload.Token, nil
}

func isTransientBridgeError(err error) bool {
	if err == nil {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"unexpected eof",
		"eof",
		"no such host",
		"server misbehaving",
		"tls handshake timeout",
		"timeout awaiting response headers",
		"status 502",
		"status 503",
		"status 504",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}

	return false
}

func nextBridgeRetryDelay(attempt int) time.Duration {
	delay := bridgeConnectBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= bridgeConnectMaxDelay {
			return bridgeConnectMaxDelay
		}
	}
	if delay > bridgeConnectMaxDelay {
		return bridgeConnectMaxDelay
	}
	return delay
}

func softenBridgeStartupError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, runtime.ErrSessionInitializing) {
		return err
	}
	if isTransientBridgeError(err) {
		return fmt.Errorf("%w: %w", runtime.ErrSessionInitializing, err)
	}
	return err
}

func isSoftBridgeUnavailable(err error) bool {
	return errors.Is(err, runtime.ErrSessionInitializing) || isTransientBridgeError(err)
}

func isLoopbackWsURL(wsURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(wsURL))
	if err != nil {
		return false
	}
	host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authenticatedWsURL(wsURL string) (string, http.Header, error) {
	if isLoopbackWsURL(wsURL) {
		return wsURL, nil, nil
	}
	if bearerToken, err := resolveBearerSessionToken(); err != nil {
		return "", nil, err
	} else if bearerToken != "" {
		headers := http.Header{}
		headers.Set("Authorization", "Bearer "+bearerToken)
		return wsURL, headers, nil
	}
	wsToken, err := issueT3WebSocketToken(wsURL)
	if err != nil {
		return "", nil, err
	}
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse authenticated ws url: %w", err)
	}
	query := parsed.Query()
	query.Set("wsToken", wsToken)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil, nil
}

// NewProvider creates a t3bridge Provider backed by native WebSocket calls to
// the T3 orchestration API, clearing any legacy exec-shim state dir on startup.
func NewProvider() *Provider {
	cleanupLegacyStateDir()
	return &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
}

func resolveLegacyStateDir() string {
	stateDir := os.Getenv("GC_EXEC_STATE_DIR")
	if stateDir != "" {
		// GC_EXEC_STATE_DIR is trusted — it's set by the GC controller,
		// not by external input. Validate it's an absolute path before use.
		if !filepath.IsAbs(stateDir) {
			return ""
		}
		return stateDir
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".t3", "gc-bridge")
}

func cleanupLegacyStateDir() {
	stateDir := resolveLegacyStateDir()
	if stateDir == "" {
		return
	}
	// Only remove the well-known legacy path, never an env-selected directory.
	if os.Getenv("GC_EXEC_STATE_DIR") != "" {
		return
	}
	_ = os.RemoveAll(stateDir)
}

func resolveNativeStateDir() string {
	if stateDir := os.Getenv("GC_T3BRIDGE_STATE_DIR"); stateDir != "" {
		if !filepath.IsAbs(stateDir) {
			return ""
		}
		return stateDir
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".t3", "gc-bridge-native")
}

func ensureNativeStateDir() error {
	return os.MkdirAll(resolveNativeStateDir(), 0o755)
}

func safeMetaPathComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

func metaFilePath(name, key string) string {
	safeName := safeMetaPathComponent(name)
	safeKey := safeMetaPathComponent(key)
	// safeMetaPathComponent strips path separators and traversal sequences,
	// so the resulting path is always safely contained within the state dir.
	return filepath.Join(resolveNativeStateDir(), fmt.Sprintf("%s.meta.%s", safeName, safeKey))
}

func bearerSessionTokenPath() string {
	return filepath.Join(resolveNativeStateDir(), "auth-bearer-session-token")
}

func readCachedBearerSessionToken() (string, error) {
	data, err := os.ReadFile(bearerSessionTokenPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writeMetaValue(name, key, value string) error {
	if err := ensureNativeStateDir(); err != nil {
		return err
	}
	return os.WriteFile(metaFilePath(name, key), []byte(value), 0o644)
}

func readMetaValue(name, key string) (string, error) {
	data, err := os.ReadFile(metaFilePath(name, key))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

func removeMetaValue(name, key string) error {
	err := os.Remove(metaFilePath(name, key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func resolveContainedPath(baseDir, relPath string) (string, error) {
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return "", fmt.Errorf("empty base dir")
	}
	baseAbs, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		return "", err
	}
	relPath = strings.TrimSpace(relPath)
	if relPath == "" || relPath == "." {
		return baseAbs, nil
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("absolute relative path: %s", relPath)
	}
	cleanRel := filepath.Clean(relPath)
	if cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base dir: %s", relPath)
	}
	target := filepath.Join(baseAbs, cleanRel)
	targetRel, err := filepath.Rel(baseAbs, target)
	if err != nil {
		return "", err
	}
	if targetRel == ".." || strings.HasPrefix(targetRel, ".."+string(filepath.Separator)) || filepath.IsAbs(targetRel) {
		return "", fmt.Errorf("path escapes base dir: %s", relPath)
	}
	return target, nil
}

func copyFileToPath(src, dstRoot, relDst string) error {
	src = filepath.Clean(strings.TrimSpace(src))
	if src == "" || src == "." {
		return fmt.Errorf("empty source path")
	}
	dst, err := resolveContainedPath(dstRoot, relDst)
	if err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if info, err := os.Stat(src); err == nil {
		_ = os.Chmod(dst, info.Mode())
	}
	return nil
}

func copyDirContents(srcDir, dstDir string) error {
	srcDir = filepath.Clean(strings.TrimSpace(srcDir))
	if srcDir == "" || srcDir == "." {
		return fmt.Errorf("empty source dir")
	}
	dstRoot, err := resolveContainedPath(dstDir, "")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return err
	}
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == srcDir {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target, err := resolveContainedPath(dstRoot, rel)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFileToPath(path, dstRoot, rel)
	})
}

func parseMetadataValue(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}

func threadCustomMetadata(thread map[string]interface{}) map[string]string {
	raw, _ := thread["customMetadata"].(map[string]interface{})
	if raw == nil {
		return nil
	}
	meta := make(map[string]string, len(raw))
	for key, value := range raw {
		if str := parseMetadataValue(value); str != "" {
			meta[key] = str
		}
	}
	return meta
}

// ParseSessionEnv decodes a JSON session-env map, returning nil for empty or
// invalid input.
func ParseSessionEnv(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil
	}
	return env
}

// SessionNameFromMetadata derives the GC session name from thread custom
// metadata, falling back to the session env and then the agent name.
func SessionNameFromMetadata(meta map[string]string) string {
	if meta == nil {
		return ""
	}
	if sessionName := strings.TrimSpace(meta["gc.sessionName"]); sessionName != "" {
		return sessionName
	}
	if sessionName := strings.TrimSpace(ParseSessionEnv(meta["gc.sessionEnv"])["GC_SESSION_NAME"]); sessionName != "" {
		return sessionName
	}
	agent := strings.TrimSpace(meta["gc.agent"])
	if agent == "" {
		return ""
	}
	if strings.Contains(agent, "/") {
		return strings.ReplaceAll(agent, "/", "--")
	}
	return agent
}

func snapshotThreadBySessionName(snapshot map[string]interface{}, name string) map[string]interface{} {
	var best map[string]interface{}
	bestUpdatedAt := ""
	for _, thread := range snapshotThreads(snapshot) {
		if deletedAt, ok := thread["deletedAt"]; ok && deletedAt != nil {
			continue
		}
		meta := threadCustomMetadata(thread)
		if SessionNameFromMetadata(meta) != name {
			continue
		}
		updatedAt, _ := thread["updatedAt"].(string)
		createdAt, _ := thread["createdAt"].(string)
		candidate := updatedAt
		if candidate == "" {
			candidate = createdAt
		}
		if best == nil || candidate > bestUpdatedAt {
			best = thread
			bestUpdatedAt = candidate
		}
	}
	return best
}

func snapshotThreadBinding(thread map[string]interface{}) *threadBinding {
	if thread == nil {
		return nil
	}
	threadID, _ := thread["id"].(string)
	projectID, _ := thread["projectId"].(string)
	if threadID == "" || projectID == "" {
		return nil
	}
	meta := threadCustomMetadata(thread)
	if meta == nil {
		return nil
	}
	sessionName := SessionNameFromMetadata(meta)
	if sessionName == "" {
		return nil
	}
	provider := meta["gc.runtimeProvider"]
	if provider == "" {
		provider, _ = thread["provider"].(string)
	}
	model := meta["gc.startupModel"]
	if model == "" {
		model, _ = thread["model"].(string)
	}
	return &threadBinding{
		ProjectID:   projectID,
		ThreadID:    threadID,
		SessionName: sessionName,
		Agent:       meta["gc.agent"],
		WorkDir:     meta["gc.startupWorkDir"],
		Provider:    provider,
		Model:       model,
	}
}

func storedEnvelopeFromThread(thread map[string]interface{}) *StartupEnvelope {
	if thread == nil {
		return nil
	}
	meta := threadCustomMetadata(thread)
	if meta == nil {
		return nil
	}
	agent := meta["gc.agent"]
	template := meta["gc.startupTemplate"]
	workDir := meta["gc.startupWorkDir"]
	provider := meta["gc.runtimeProvider"]
	model := meta["gc.startupModel"]
	if agent == "" || template == "" || workDir == "" || provider == "" || model == "" {
		return nil
	}
	return &StartupEnvelope{
		GC: GCSection{
			Agent:    agent,
			Template: template,
		},
		Runtime: RuntimeSection{
			WorkDir:  workDir,
			Provider: provider,
			Model:    model,
		},
	}
}

func worktreeBaseFromThread(thread map[string]interface{}) string {
	meta := threadCustomMetadata(thread)
	if meta == nil {
		return ""
	}
	return strings.TrimSpace(meta["gc.rigPath"])
}

func threadUpdatedAt(thread map[string]interface{}) time.Time {
	if thread == nil {
		return time.Time{}
	}
	for _, key := range []string{"updatedAt", "createdAt"} {
		value, _ := thread[key].(string)
		if value == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, value); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func (p *Provider) setRecentStart(name string, ts time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recentStarts[name] = ts
}

func (p *Provider) clearRecentStart(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.recentStarts, name)
}

func (p *Provider) withinRecentStart(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	startedAt, ok := p.recentStarts[name]
	if !ok {
		return false
	}
	if time.Since(startedAt) >= 30*time.Second {
		delete(p.recentStarts, name)
		return false
	}
	return true
}

// rpcCall makes a generic WebSocket RPC call and returns the result map.
func (p *Provider) rpcCall(method string, params map[string]interface{}) (map[string]interface{}, error) {
	p.mu.Lock()
	p.reqSeq++
	reqID := p.reqSeq
	p.mu.Unlock()

	deadline := time.Now().Add(bridgeConnectRetryWindow)
	for attempt := 1; ; attempt++ {
		result, err := p.rpcCallOnce(method, params, reqID)
		if err == nil {
			return result, nil
		}
		if !isTransientBridgeError(err) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(nextBridgeRetryDelay(attempt))
	}
}

func (p *Provider) rpcCallOnce(method string, params map[string]interface{}, reqID int) (map[string]interface{}, error) {
	var lastErr error
	var failures []string
	for _, candidate := range resolveWsURLCandidates() {
		wsURL, headers, err := authenticatedWsURL(candidate)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", candidate, err)
			failures = append(failures, lastErr.Error())
			continue
		}
		dialer := *websocket.DefaultDialer
		dialer.HandshakeTimeout = bridgeWSTimeout
		conn, _, err := dialer.Dial(wsURL, headers)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", candidate, err)
			failures = append(failures, lastErr.Error())
			continue
		}
		defer func() { _ = conn.Close() }()

		payload := params
		if payload == nil {
			payload = map[string]interface{}{}
		}
		request := map[string]interface{}{
			"_tag":    "Request",
			"id":      fmt.Sprintf("%d", reqID),
			"tag":     method,
			"payload": payload,
			"headers": []interface{}{},
		}
		if err := conn.WriteJSON(request); err != nil {
			lastErr = fmt.Errorf("%s: %w", candidate, err)
			failures = append(failures, lastErr.Error())
			continue
		}

		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", candidate, err)
				failures = append(failures, lastErr.Error())
				break
			}
			var resp struct {
				Tag       string `json:"_tag"`
				RequestID string `json:"requestId"`
				Exit      *struct {
					Tag   string                 `json:"_tag"`
					Value map[string]interface{} `json:"value"`
					Cause json.RawMessage        `json:"cause"`
				} `json:"exit"`
				Defect string `json:"defect"`
			}
			if err := json.Unmarshal(msg, &resp); err != nil {
				continue
			}
			if resp.Tag == "Defect" {
				return nil, fmt.Errorf("t3bridge rpc defect: %s", resp.Defect)
			}
			if resp.Tag != "Exit" || resp.RequestID != fmt.Sprintf("%d", reqID) {
				continue
			}
			if resp.Exit == nil {
				return nil, fmt.Errorf("t3 rpc %s: nil exit", method)
			}
			if resp.Exit.Tag == "Failure" {
				return nil, fmt.Errorf("t3 rpc %s: %s", method, string(resp.Exit.Cause))
			}
			return resp.Exit.Value, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no T3 WebSocket URL candidates")
	}
	if len(failures) > 1 {
		return nil, fmt.Errorf("all T3 WebSocket candidates failed: %s", strings.Join(failures, "; "))
	}
	return nil, lastErr
}

// threadSessionStatus returns the session status for a thread by checking
// the orchestration snapshot. Results are cached briefly so that multiple
// IsRunning/ProcessAlive calls within the same command share one RPC.
func (p *Provider) threadSessionStatus(threadID string) string {
	p.mu.Lock()
	if time.Since(p.snapshotCacheAt) < snapshotCacheTTL && p.snapshotCache != nil {
		status := p.snapshotCache[threadID]
		p.mu.Unlock()
		return status
	}
	p.mu.Unlock()

	snapshot, err := p.rpcSnapshot()
	if err != nil {
		return ""
	}

	cache := make(map[string]string)
	for _, raw := range snapshotItems(snapshot, "threads") {
		thread, _ := raw.(map[string]interface{})
		if thread == nil {
			continue
		}
		id, _ := thread["id"].(string)
		session, _ := thread["session"].(map[string]interface{})
		if session != nil {
			st, _ := session["status"].(string)
			cache[id] = st
		}
	}

	p.mu.Lock()
	p.snapshotCache = cache
	p.snapshotCacheAt = time.Now()
	p.mu.Unlock()

	return cache[threadID]
}

func (p *Provider) cacheSnapshot(snapshot map[string]interface{}) {
	cache := make(map[string]string)
	for _, raw := range snapshotItems(snapshot, "threads") {
		thread, _ := raw.(map[string]interface{})
		if thread == nil {
			continue
		}
		id, _ := thread["id"].(string)
		session, _ := thread["session"].(map[string]interface{})
		if session != nil {
			st, _ := session["status"].(string)
			cache[id] = st
		}
	}

	p.mu.Lock()
	p.snapshotSnapshot = snapshot
	p.snapshotCache = cache
	p.snapshotCacheAt = time.Now()
	p.mu.Unlock()
}

func (p *Provider) clearSnapshotCache() {
	p.mu.Lock()
	p.snapshotSnapshot = nil
	p.snapshotCache = nil
	p.snapshotCacheAt = time.Time{}
	p.mu.Unlock()
}

func (p *Provider) rpcDispatchCommand(command map[string]interface{}) error {
	_, err := p.rpcCall("orchestration.dispatchCommand", command)
	return err
}

func (p *Provider) rpcSnapshot() (map[string]interface{}, error) {
	p.mu.Lock()
	if time.Since(p.snapshotCacheAt) < snapshotCacheTTL && p.snapshotSnapshot != nil {
		snapshot := p.snapshotSnapshot
		p.mu.Unlock()
		return snapshot, nil
	}
	p.mu.Unlock()

	snapshot, err := p.rpcCall("orchestration.getSnapshot", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	p.cacheSnapshot(snapshot)
	return snapshot, nil
}

func (p *Provider) nextCommandID(prefix string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqSeq++
	return fmt.Sprintf("%s-%d-%s", prefix, p.reqSeq, uuid.NewString())
}

// rpcCreateWorktree calls git.createWorktree via WebSocket. Returns (worktreePath, branch, error).
func (p *Provider) rpcCreateWorktree(cwd, branch, newBranch, path string) (string, string, error) {
	params := map[string]interface{}{
		"cwd":    cwd,
		"branch": branch,
	}
	if newBranch != "" {
		params["newBranch"] = newBranch
	}
	if path != "" {
		params["path"] = path
	} else {
		params["path"] = nil
	}
	result, err := p.rpcCall("git.createWorktree", params)
	if err != nil {
		return "", "", err
	}
	wt, ok := result["worktree"].(map[string]interface{})
	if !ok {
		return "", "", fmt.Errorf("rpcCreateWorktree: unexpected response shape")
	}
	wtPath, _ := wt["path"].(string)
	wtBranch, _ := wt["branch"].(string)
	return wtPath, wtBranch, nil
}

// rpcRemoveWorktree calls git.removeWorktree via WebSocket.
func (p *Provider) rpcRemoveWorktree(cwd, path string) error {
	_, err := p.rpcCall("git.removeWorktree", map[string]interface{}{
		"cwd":  cwd,
		"path": path,
	})
	return err
}

// rpcUpdateThreadMeta dispatches thread.meta.update via orchestration.
func (p *Provider) rpcUpdateThreadMeta(threadID, branch, worktreePath string) error {
	_, err := p.rpcCall("orchestration.dispatchCommand", map[string]interface{}{
		"command": map[string]interface{}{
			"type":         "thread.meta.update",
			"commandId":    p.nextCommandID("gc-worktree"),
			"threadId":     threadID,
			"branch":       branch,
			"worktreePath": worktreePath,
		},
	})
	return err
}

func (p *Provider) dispatchProjectCreate(projectID, title, workspaceRoot, provider, model string) error {
	command := map[string]interface{}{
		"type":          "project.create",
		"commandId":     p.nextCommandID("t3bridge-project"),
		"projectId":     projectID,
		"title":         title,
		"workspaceRoot": workspaceRoot,
		"createdAt":     time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if provider != "" || model != "" {
		command["defaultModelSelection"] = map[string]interface{}{
			"provider": provider,
			"model":    model,
		}
	}
	return p.rpcDispatchCommand(command)
}

func (p *Provider) dispatchThreadCreate(
	threadID,
	projectID,
	title,
	provider,
	model,
	branch,
	worktreePath string,
	customMetadata map[string]interface{},
) error {
	command := map[string]interface{}{
		"type":            "thread.create",
		"commandId":       p.nextCommandID("t3bridge-thread"),
		"threadId":        threadID,
		"projectId":       projectID,
		"title":           title,
		"modelSelection":  map[string]interface{}{"provider": provider, "model": model},
		"runtimeMode":     "full-access",
		"interactionMode": "default",
		"branch":          nil,
		"worktreePath":    nil,
		"createdAt":       time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if branch != "" {
		command["branch"] = branch
	}
	if worktreePath != "" {
		command["worktreePath"] = worktreePath
	}
	if customMetadata != nil {
		command["customMetadata"] = customMetadata
	}
	return p.rpcDispatchCommand(command)
}

func (p *Provider) dispatchThreadArchive(threadID string) error {
	return p.rpcDispatchCommand(map[string]interface{}{
		"type":      "thread.archive",
		"commandId": p.nextCommandID("t3bridge-archive"),
		"threadId":  threadID,
	})
}

func (p *Provider) dispatchThreadSessionStop(threadID string) error {
	return p.rpcDispatchCommand(map[string]interface{}{
		"type":      "thread.session.stop",
		"commandId": p.nextCommandID("t3bridge-session-stop"),
		"threadId":  threadID,
		"createdAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
}

func (p *Provider) dispatchThreadMeta(threadID string, customMetadata map[string]interface{}) error {
	command := map[string]interface{}{
		"type":      "thread.meta.update",
		"commandId": p.nextCommandID("t3bridge-meta"),
		"threadId":  threadID,
	}
	if customMetadata != nil {
		command["customMetadata"] = customMetadata
	}
	return p.rpcDispatchCommand(command)
}

func (p *Provider) dispatchThreadModelSelection(threadID, provider, model string) error {
	return p.rpcDispatchCommand(map[string]interface{}{
		"type":      "thread.meta.update",
		"commandId": p.nextCommandID("t3bridge-model"),
		"threadId":  threadID,
		"modelSelection": map[string]interface{}{
			"provider": provider,
			"model":    model,
		},
	})
}

func (p *Provider) dispatchActivity(threadID, kind, summary string, payload map[string]interface{}) error {
	tone := "info"
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	return p.rpcDispatchCommand(map[string]interface{}{
		"type":      "thread.activity.append",
		"commandId": p.nextCommandID("t3bridge-activity"),
		"threadId":  threadID,
		"activity": map[string]interface{}{
			"id":        uuid.NewString(),
			"turnId":    nil,
			"kind":      kind,
			"summary":   summary,
			"tone":      tone,
			"payload":   payload,
			"createdAt": now,
		},
		"createdAt": now,
	})
}

func (p *Provider) dispatchTurnStart(threadID, text, provider, model string) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	return p.rpcDispatchCommand(map[string]interface{}{
		"type":      "thread.turn.start",
		"commandId": p.nextCommandID("t3bridge-turn"),
		"threadId":  threadID,
		"message": map[string]interface{}{
			"messageId":   uuid.NewString(),
			"role":        "user",
			"text":        text,
			"attachments": []interface{}{},
		},
		"modelSelection": map[string]interface{}{
			"provider": provider,
			"model":    model,
		},
		"runtimeMode":     "full-access",
		"interactionMode": "default",
		"createdAt":       now,
	})
}

func (p *Provider) dispatchTurnInterrupt(threadID string) error {
	return p.rpcDispatchCommand(map[string]interface{}{
		"type":      "thread.turn.interrupt",
		"commandId": p.nextCommandID("t3bridge-interrupt"),
		"threadId":  threadID,
		"createdAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
}

func snapshotProjects(snapshot map[string]interface{}) []map[string]interface{} {
	raw := snapshotItems(snapshot, "projects")
	projects := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		project, _ := item.(map[string]interface{})
		if project != nil {
			projects = append(projects, project)
		}
	}
	return projects
}

func snapshotThreads(snapshot map[string]interface{}) []map[string]interface{} {
	raw := snapshotItems(snapshot, "threads")
	threads := make([]map[string]interface{}, 0, len(raw))
	for _, item := range raw {
		thread, _ := item.(map[string]interface{})
		if thread != nil {
			threads = append(threads, thread)
		}
	}
	return threads
}

func snapshotItems(snapshot map[string]interface{}, key string) []interface{} {
	if snapshot == nil {
		return nil
	}
	if raw, ok := snapshot[key].([]interface{}); ok {
		return raw
	}
	result, _ := snapshot["result"].(map[string]interface{})
	raw, _ := result[key].([]interface{})
	return raw
}

func resolveActiveProjectID(snapshot map[string]interface{}, workspaceRoot string) string {
	for _, project := range snapshotProjects(snapshot) {
		if deletedAt, ok := project["deletedAt"]; ok && deletedAt != nil {
			continue
		}
		if root, _ := project["workspaceRoot"].(string); root == workspaceRoot {
			if id, _ := project["id"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

func projectIsActive(snapshot map[string]interface{}, projectID string) bool {
	for _, project := range snapshotProjects(snapshot) {
		id, _ := project["id"].(string)
		if id != projectID {
			continue
		}
		if deletedAt, ok := project["deletedAt"]; ok && deletedAt != nil {
			return false
		}
		return true
	}
	return false
}

func threadIsActive(snapshot map[string]interface{}, threadID string) bool {
	for _, thread := range snapshotThreads(snapshot) {
		id, _ := thread["id"].(string)
		if id != threadID {
			continue
		}
		if deletedAt, ok := thread["deletedAt"]; ok && deletedAt != nil {
			return false
		}
		return true
	}
	return false
}

func threadHasRequiredGCMetadata(snapshot map[string]interface{}, threadID string) bool {
	for _, thread := range snapshotThreads(snapshot) {
		id, _ := thread["id"].(string)
		if id != threadID {
			continue
		}
		meta := threadCustomMetadata(thread)
		if meta == nil {
			return false
		}
		if strings.TrimSpace(meta["gc.agent"]) == "" {
			return false
		}
		if strings.TrimSpace(meta["gc.sessionName"]) == "" {
			return false
		}
		return true
	}
	return false
}

func (p *Provider) waitForThreadGCMetadata(threadID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		p.clearSnapshotCache()
		snapshot, err := p.rpcSnapshot()
		if err == nil && threadHasRequiredGCMetadata(snapshot, threadID) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("wait for thread metadata: %w", err)
			}
			return fmt.Errorf("wait for thread metadata: timed out for thread %s", threadID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func decodeEnvelope(data json.RawMessage) *StartupEnvelope {
	if len(data) == 0 {
		return nil
	}
	var envelope StartupEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil
	}
	return &envelope
}

func buildThreadEnv(env map[string]string) map[string]string {
	threadEnv := make(map[string]string)
	for key, value := range env {
		if value == "" {
			continue
		}
		if key == "GC_STARTUP_ENVELOPE" {
			continue
		}
		if strings.HasPrefix(key, "GC_") {
			threadEnv[key] = value
		}
	}
	if strings.EqualFold(threadEnv["GC_BEADS_BACKEND"], "doltlite") || strings.EqualFold(env["BEADS_BACKEND"], "doltlite") {
		for _, key := range []string{
			"GC_DOLT_HOST",
			"GC_DOLT_PORT",
			"GC_DOLT_SERVER_PORT",
			"BEADS_DOLT_PORT",
			"BEADS_DOLT_SERVER_HOST",
			"BEADS_DOLT_SERVER_MODE",
			"BEADS_DOLT_SERVER_PORT",
			"BEADS_DOLT_SHARED_SERVER",
		} {
			delete(threadEnv, key)
		}
		return threadEnv
	}
	if host := strings.TrimSpace(threadEnv["GC_DOLT_HOST"]); host != "" {
		threadEnv["BEADS_DOLT_SERVER_HOST"] = host
	}
	if port := strings.TrimSpace(threadEnv["GC_DOLT_PORT"]); port != "" {
		threadEnv["BEADS_DOLT_PORT"] = port
		threadEnv["BEADS_DOLT_SERVER_PORT"] = port
		threadEnv["BEADS_DOLT_SERVER_MODE"] = "1"
	}
	delete(threadEnv, "BEADS_DOLT_SHARED_SERVER")
	return threadEnv
}

func buildGCMetadata(envelope StartupEnvelope, runtimeProvider string, sessionEnv map[string]string) map[string]interface{} {
	state := "active"
	groupKind := "workspace"
	groupID := envelope.GC.CityName
	groupLabel := strings.ToUpper(strings.TrimSpace(envelope.GC.CityName))
	if strings.TrimSpace(envelope.GC.RigName) != "" {
		groupKind = "rig"
		groupID = envelope.GC.RigName
		groupLabel = envelope.GC.RigName
	}
	agentQualified := envelope.GC.Agent
	agentLabel := agentQualified
	if slash := strings.LastIndex(agentQualified, "/"); slash >= 0 && slash+1 < len(agentQualified) {
		agentLabel = agentQualified[slash+1:]
	}
	if dot := strings.LastIndex(agentLabel, "."); dot >= 0 && dot+1 < len(agentLabel) {
		agentLabel = agentLabel[dot+1:]
	}
	meta := map[string]interface{}{
		"gc.agent":             envelope.GC.Agent,
		"gc.sessionName":       envelope.GC.SessionName,
		"gc.rig":               envelope.GC.RigName,
		"gc.rigPath":           envelope.GC.RigPath,
		"gc.city":              envelope.GC.CityName,
		"gc.bead":              envelope.Assignment.BeadID,
		"gc.beadTitle":         envelope.Assignment.BeadTitle,
		"gc.convoy":            envelope.Assignment.ConvoyID,
		"gc.convoyTitle":       envelope.Assignment.ConvoyTitle,
		"gc.convoyStatus":      envelope.Assignment.ConvoyStatus,
		"gc.convoyClosedCount": envelope.Assignment.ConvoyClosedCount,
		"gc.convoyTotalCount":  envelope.Assignment.ConvoyTotalCount,
		"gc.provider":          "t3bridge",
		"gc.runtimeProvider":   runtimeProvider,
		"gc.state":             state,
		"gc.startupVersion":    fmt.Sprintf("%d", envelope.Version),
		"gc.startupTemplate":   envelope.GC.Template,
		"gc.startupModel":      envelope.Runtime.Model,
		"gc.startupWorkDir":    envelope.Runtime.WorkDir,
		"gc.molecule":          envelope.Assignment.MoleculeID,
		"gc.formula":           envelope.Assignment.Formula,
		"gc.groupKind":         groupKind,
		"gc.groupId":           groupID,
		"gc.groupLabel":        groupLabel,
		"gc.agentQualified":    agentQualified,
		"gc.agentLabel":        agentLabel,
	}
	if len(sessionEnv) > 0 {
		if encodedEnv, err := json.Marshal(sessionEnv); err == nil {
			meta["gc.sessionEnv"] = string(encodedEnv)
		}
		if port := sessionEnv["GC_DOLT_PORT"]; port != "" && !strings.EqualFold(sessionEnv["GC_BEADS_BACKEND"], "doltlite") && !strings.EqualFold(sessionEnv["BEADS_BACKEND"], "doltlite") {
			meta["gc.doltPort"] = port
		}
	}
	for key, value := range meta {
		str := strings.TrimSpace(fmt.Sprint(value))
		if str == "" {
			delete(meta, key)
			continue
		}
		meta[key] = str
	}
	return meta
}

func stateChangePayload(state string, extra map[string]interface{}) map[string]interface{} {
	payload := map[string]interface{}{"state": state}
	for k, v := range extra {
		payload[k] = v
	}
	return payload
}

func assignmentFromThread(thread map[string]interface{}) string {
	meta := threadCustomMetadata(thread)
	if meta == nil {
		return ""
	}
	return strings.TrimSpace(meta["gc.bead"])
}

func (p *Provider) isPersistentAgent(thread map[string]interface{}) bool {
	return assignmentFromThread(thread) == ""
}

func (p *Provider) recordStateChange(threadID, state, summary string, extra map[string]interface{}) {
	_ = p.dispatchThreadMeta(threadID, map[string]interface{}{"gc.state": state})
	_ = p.dispatchActivity(threadID, "gc.state.changed", summary, stateChangePayload(state, extra))
}

func (p *Provider) removeWorktreeForThread(thread map[string]interface{}) {
	if thread == nil {
		return
	}
	worktreePath, _ := thread["worktreePath"].(string)
	if worktreePath == "" {
		return
	}
	base := worktreeBaseFromThread(thread)
	if base == "" {
		return
	}
	_ = p.rpcRemoveWorktree(base, worktreePath)
}

func (p *Provider) clearBridgeMeta(name string) {
	_ = removeMetaValue(name, "GC_DRAIN")
	_ = removeMetaValue(name, "GC_DRAIN_ACK")
	_ = removeMetaValue(name, "drained")
}

func poolKickoffText(envelope StartupEnvelope) string {
	templateName := envelope.GC.Template
	if templateName == "" {
		return ""
	}
	if !strings.Contains(templateName, "pool") &&
		!strings.Contains(templateName, "codex") &&
		!strings.Contains(templateName, "claude") &&
		templateName != "t3-codex-pool" {
		return ""
	}
	if envelope.Assignment.BeadID != "" || envelope.Assignment.BeadTitle != "" {
		label := envelope.Assignment.BeadTitle
		if label == "" {
			label = envelope.Assignment.BeadID
		}
		suffix := ""
		if envelope.Assignment.BeadID != "" && envelope.Assignment.BeadTitle != "" {
			suffix = " (" + envelope.Assignment.BeadID + ")"
		}
		return "[gc] Begin work now. Your current work is " + label + suffix + ". Check for assigned in-progress work first, then check the pool queue, claim the next item, execute it, close it, and drain when complete."
	}
	return "[gc] Begin work now. Check for assigned in-progress work first, then check the pool queue, claim the next item, execute it, close it, and drain when complete."
}

func deriveThreadTitle(name string, envelope StartupEnvelope) string {
	if envelope.Assignment.BeadTitle != "" {
		return envelope.Assignment.BeadTitle
	}
	if envelope.Assignment.BeadID != "" {
		return envelope.Assignment.BeadID
	}
	shortAgent := filepath.Base(envelope.GC.Agent)
	if shortAgent == "" {
		shortAgent = name
	}
	return name + " · " + shortAgent
}

func deriveProjectWorkspaceRoot(workDir string, envelope StartupEnvelope) string {
	if root := strings.TrimSpace(envelope.GC.RigPath); root != "" {
		return root
	}
	if root := strings.TrimSpace(envelope.GC.CityPath); root != "" {
		return root
	}
	return strings.TrimSpace(workDir)
}

func deriveProjectTitle(name, workspaceRoot string, envelope StartupEnvelope) string {
	if workspaceRoot != "" {
		return filepath.Base(workspaceRoot)
	}
	if envelope.GC.Agent != "" {
		return envelope.GC.Agent
	}
	return name
}

const defaultCodexModel = "gpt-5.4"

func resolveProviderModel(cfg runtime.Config, envelope StartupEnvelope) (string, string) {
	provider := normalizeT3Provider(cfg.Env["GC_PROVIDER"])
	model := strings.TrimSpace(cfg.Env["GC_MODEL"])
	if model == "" {
		model = strings.TrimSpace(envelope.Runtime.Model)
	}
	if provider == "" {
		switch {
		case strings.Contains(cfg.Command, "codex"):
			provider = "codex"
		case strings.Contains(cfg.Command, "claude"):
			provider = "claudeAgent"
		case envelope.Runtime.Provider != "":
			provider = normalizeT3Provider(envelope.Runtime.Provider)
		default:
			provider = inferProviderFromModel(model)
		}
	}
	if model == "" {
		if provider == "codex" {
			model = defaultCodexModel
		} else {
			model = "claude-sonnet-4-6"
		}
	}
	return provider, model
}

func normalizeT3Provider(provider string) string {
	switch strings.TrimSpace(provider) {
	case "claude", "claudeAgent":
		return "claudeAgent"
	case "codex":
		return "codex"
	default:
		return strings.TrimSpace(provider)
	}
}

func inferProviderFromModel(model string) string {
	trimmed := strings.TrimSpace(strings.ToLower(model))
	switch {
	case trimmed == "":
		return "codex"
	case strings.HasPrefix(trimmed, "claude"):
		return "claudeAgent"
	case strings.HasPrefix(trimmed, "gpt"):
		return "codex"
	default:
		return "codex"
	}
}

func resolveBindingProviderModel(binding threadBinding, env map[string]string) (string, string) {
	provider := normalizeT3Provider(binding.Provider)
	model := strings.TrimSpace(binding.Model)
	if model == "" && env != nil {
		model = strings.TrimSpace(env["GC_MODEL"])
	}
	if provider == "" && env != nil {
		provider = normalizeT3Provider(env["GC_PROVIDER"])
	}
	if provider == "" {
		provider = inferProviderFromModel(model)
	}
	if model == "" {
		if provider == "codex" {
			model = defaultCodexModel
		} else {
			model = "claude-sonnet-4-6"
		}
	}
	return provider, model
}

func resolveConfigProviderModel(cfg *execStartConfig) (string, string, bool) {
	if cfg == nil {
		return "", "", false
	}
	envelope := decodeEnvelope(cfg.StartupEnvelope)
	if envelope == nil {
		return "", "", false
	}
	provider := normalizeT3Provider(envelope.Runtime.Provider)
	model := strings.TrimSpace(envelope.Runtime.Model)
	if provider != "" && model != "" {
		return provider, model, true
	}
	provider, model = resolveProviderModel(
		runtime.Config{
			Command: cfg.Command,
			Env:     cfg.Env,
			WorkDir: cfg.WorkDir,
		},
		*envelope,
	)
	return provider, model, true
}

func beadStoreForWatcher(workDir string, env map[string]string) *beads.CachingStore {
	bd := beads.NewBdStore(workDir, beads.ExecCommandRunnerWithEnv(env))
	return beads.NewCachingStore(bd, nil)
}

func beadEventRelevant(ev events.Event, bead beads.Bead, agentName, currentBead string) bool {
	if ev.Actor == agentName {
		return true
	}
	if bead.Assignee == agentName {
		return true
	}
	if currentBead == "" {
		return false
	}
	if ev.Subject == currentBead {
		return true
	}
	return strings.HasPrefix(ev.Subject, currentBead+".")
}

func activityFromBeadEvent(ev events.Event, bead beads.Bead) (string, string, map[string]interface{}) {
	kind := "gc.bead.claimed"
	summaryLabel := bead.Title
	if summaryLabel == "" {
		summaryLabel = bead.ID
	}
	switch {
	case ev.Type == events.BeadClosed || bead.Status == "closed":
		kind = "gc.bead.closed"
		return kind, "Bead closed: " + summaryLabel, map[string]interface{}{
			"beadId":     bead.ID,
			"beadTitle":  bead.Title,
			"beadStatus": bead.Status,
			"assignee":   bead.Assignee,
			"formula":    bead.Ref,
			"moleculeId": bead.Metadata[beadmeta.MoleculeIDMetadataKey],
			"eventType":  ev.Type,
		}
	case ev.Type == events.BeadUpdated:
		statusPrefix := ""
		if bead.Status != "" {
			statusPrefix = "[" + bead.Status + "] "
		}
		return kind, "Bead " + statusPrefix + "updated: " + summaryLabel, map[string]interface{}{
			"beadId":     bead.ID,
			"beadTitle":  bead.Title,
			"beadStatus": bead.Status,
			"assignee":   bead.Assignee,
			"formula":    bead.Ref,
			"moleculeId": bead.Metadata[beadmeta.MoleculeIDMetadataKey],
			"eventType":  ev.Type,
		}
	default:
		return kind, "Bead updated: " + summaryLabel, map[string]interface{}{
			"beadId":     bead.ID,
			"beadTitle":  bead.Title,
			"beadStatus": bead.Status,
			"assignee":   bead.Assignee,
			"formula":    bead.Ref,
			"moleculeId": bead.Metadata[beadmeta.MoleculeIDMetadataKey],
			"eventType":  ev.Type,
		}
	}
}

func (p *Provider) refreshAssignmentProjection(threadID string, envelope StartupEnvelope, providerName string, bead beads.Bead, cache *beads.CachingStore) {
	convoyID := ""
	convoyTitle := ""
	convoyStatus := ""
	convoyClosedCount := envelope.Assignment.ConvoyClosedCount
	convoyTotalCount := envelope.Assignment.ConvoyTotalCount
	if bead.ParentID != "" {
		if parent, err := cache.Get(bead.ParentID); err == nil && parent.Type == "convoy" {
			convoyID = parent.ID
			convoyTitle = parent.Title
			convoyStatus = parent.Status
			if children, err := cache.Children(parent.ID); err == nil {
				total := len(children)
				closed := 0
				for _, child := range children {
					if child.Status == "closed" {
						closed++
					}
				}
				convoyTotalCount = strconv.Itoa(total)
				convoyClosedCount = strconv.Itoa(closed)
			}
		}
	}

	next := envelope
	next.Assignment.BeadID = bead.ID
	next.Assignment.BeadTitle = bead.Title
	next.Assignment.ConvoyID = convoyID
	next.Assignment.ConvoyTitle = convoyTitle
	next.Assignment.ConvoyStatus = convoyStatus
	next.Assignment.ConvoyClosedCount = convoyClosedCount
	next.Assignment.ConvoyTotalCount = convoyTotalCount
	next.Assignment.Formula = bead.Ref
	if next.Assignment.MoleculeID == "" {
		next.Assignment.MoleculeID = bead.Metadata[beadmeta.MoleculeIDMetadataKey]
	}
	_ = p.dispatchThreadMeta(threadID, buildGCMetadata(next, providerName, nil))
}

func (p *Provider) runEventWatcher(ctx context.Context, _ string, cfg runtime.Config, binding threadBinding, envelope StartupEnvelope, providerName string) {
	cityPath := cfg.Env["GC_CITY_PATH"]
	if cityPath == "" {
		cityPath = cfg.Env["GC_CITY"]
	}
	if cityPath == "" || cfg.WorkDir == "" {
		return
	}

	eventPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	recorder, err := events.NewFileRecorder(eventPath, io.Discard)
	if err != nil {
		return
	}
	defer func() { _ = recorder.Close() }()

	cache := beadStoreForWatcher(cfg.WorkDir, cfg.Env)
	_ = cache.Prime(ctx)

	afterSeq, err := recorder.LatestSeq()
	if err != nil {
		afterSeq = 0
	}
	watcher, err := recorder.Watch(ctx, afterSeq)
	if err != nil {
		return
	}
	defer func() { _ = watcher.Close() }()

	agentName := cfg.Env["GC_AGENT"]
	currentBead := cfg.Env["GC_BEAD"]

	for {
		ev, err := watcher.Next()
		if err != nil {
			return
		}
		if ev.Type != events.BeadUpdated && ev.Type != events.BeadClosed && ev.Type != events.BeadCreated {
			continue
		}
		cache.ApplyEvent(ev.Type, ev.Payload)
		bead, err := cache.Get(ev.Subject)
		if err != nil {
			continue
		}
		if !beadEventRelevant(ev, bead, agentName, currentBead) {
			continue
		}
		p.refreshAssignmentProjection(binding.ThreadID, envelope, providerName, bead, cache)
		kind, summary, payload := activityFromBeadEvent(ev, bead)
		if kind == "gc.bead.claimed" && bead.Title != "" {
			_ = p.rpcDispatchCommand(map[string]interface{}{
				"type":      "thread.meta.update",
				"commandId": p.nextCommandID("t3bridge-title"),
				"threadId":  binding.ThreadID,
				"title":     bead.Title,
			})
		}
		_ = p.dispatchActivity(binding.ThreadID, kind, summary, payload)
	}
}

func (p *Provider) ensureEventWatcher(name string, cfg runtime.Config, binding threadBinding, envelope StartupEnvelope, providerName string) {
	p.mu.Lock()
	if cancel, ok := p.watchers[name]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.watchers[name] = cancel
	p.mu.Unlock()

	go p.runEventWatcher(ctx, name, cfg, binding, envelope, providerName)
}

func (p *Provider) stopEventWatcher(name string) {
	p.mu.Lock()
	cancel := p.watchers[name]
	delete(p.watchers, name)
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func t3bridgeDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_T3BRIDGE_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func t3bridgeDebugf(format string, args ...interface{}) {
	if t3bridgeDebugEnabled() {
		fmt.Fprintf(os.Stderr, format, args...) //nolint:errcheck // best-effort debug logging
	}
}

// IsRunning checks T3 for session liveness via the orchestration snapshot.
// A recently-started session is treated as running for a short grace period
// even before T3 reports a provider session, avoiding duplicate starts while
// the first turn is still materializing.
func (p *Provider) IsRunning(name string) bool {
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if p.withinRecentStart(name) {
			fmt.Fprintf(os.Stderr, "t3bridge: IsRunning(%s) — snapshot soft-unavailable during startup grace → true (%v)\n", name, err)
			return true
		}
		t3bridgeDebugf("t3bridge: IsRunning(%s) — snapshot error: %v\n", name, err)
		return false
	}
	thread := snapshotThreadBySessionName(snapshot, name)
	binding := snapshotThreadBinding(thread)
	if binding == nil {
		t3bridgeDebugf("t3bridge: IsRunning(%s) — no snapshot binding\n", name)
		return false
	}
	status := p.threadSessionStatus(binding.ThreadID)
	if (status == "none" || status == "gone") && p.withinRecentStart(name) {
		fmt.Fprintf(os.Stderr, "t3bridge: IsRunning(%s) threadID=%s — startup grace period → true\n", name, binding.ThreadID)
		return true
	}
	result := status == "running" || status == "ready"
	t3bridgeDebugf("t3bridge: IsRunning(%s) threadID=%s status=%q → %v\n", name, binding.ThreadID, status, result)
	return result
}

// ListRunning enumerates live GC-managed session names from the T3 snapshot.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if isSoftBridgeUnavailable(err) {
			fmt.Fprintf(os.Stderr, "t3bridge: ListRunning(%s) — soft-unavailable: %v\n", prefix, err)
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0)
	seen := make(map[string]struct{})
	for _, thread := range snapshotThreads(snapshot) {
		if deletedAt, ok := thread["deletedAt"]; ok && deletedAt != nil {
			continue
		}
		meta := threadCustomMetadata(thread)
		name := SessionNameFromMetadata(meta)
		if name == "" {
			continue
		}
		if meta["gc.state"] == "archived" {
			continue
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

// Start creates or reuses a T3 thread for the named session and dispatches the
// startup prompt and any nudge.
func (p *Provider) Start(_ context.Context, name string, cfg runtime.Config) error {
	fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) called, wsURL=%s\n", name, resolveWsURL())

	var envelope StartupEnvelope
	hasWorktree := false
	var cwd, branch, newBranch, desiredPath string

	// Try reading a pre-built envelope from env first.
	if envJSON := cfg.Env["GC_STARTUP_ENVELOPE"]; envJSON != "" {
		if err := json.Unmarshal([]byte(envJSON), &envelope); err != nil {
			return fmt.Errorf("t3bridge: decode startup envelope: %w", err)
		}
	} else {
		// Build envelope from cfg fields and env vars set by the reconciler.
		cityPath := cfg.Env["GC_CITY_PATH"]
		cityName := filepath.Base(cityPath)
		groupingAgent := cfg.Env["GC_TEMPLATE"]
		if strings.TrimSpace(groupingAgent) == "" {
			groupingAgent = cfg.Env["GC_ALIAS"]
		}
		if strings.TrimSpace(groupingAgent) == "" {
			groupingAgent = cfg.Env["GC_AGENT"]
		}
		envelope.GC = GCSection{
			CityName:    cityName,
			CityPath:    cityPath,
			RigName:     cfg.Env["GC_RIG"],
			RigPath:     cfg.Env["GC_RIG_ROOT"],
			Agent:       groupingAgent,
			Template:    cfg.Env["GC_TEMPLATE"],
			SessionName: name,
		}
		envelope.Runtime = RuntimeSection{
			WorkDir:     cfg.WorkDir,
			RuntimeMode: "full-access",
			Command:     cfg.Command,
		}
		envelope.Startup = StartupSection{
			StartupPrompt: cfg.PromptSuffix,
		}
		if cfg.Nudge != "" {
			envelope.Startup.InitialNudge = cfg.Nudge
		}
	}
	if envelope.Runtime.Branch != "" {
		hasWorktree = true
		cwd = envelope.GC.RigPath
		branch = envelope.Runtime.Branch
		newBranch = envelope.Runtime.NewBranch
		desiredPath = cfg.WorkDir
	}

	providerName, modelName := resolveProviderModel(cfg, envelope)
	fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) resolved provider=%s model=%s workdir=%s projectRoot=%s agent=%s template=%s\n", //nolint:errcheck
		name, providerName, modelName, cfg.WorkDir, deriveProjectWorkspaceRoot(cfg.WorkDir, envelope), envelope.GC.Agent, envelope.GC.Template)
	if envelope.Runtime.Provider == "" {
		envelope.Runtime.Provider = providerName
	}
	if envelope.Runtime.Model == "" {
		envelope.Runtime.Model = modelName
	}
	if envelope.Resume.Policy == "" && !envelope.Resume.AllowThreadReuse {
		legacyNamed := strings.TrimSpace(cfg.Env["GC_BEAD"]) == "" && envelope.GC.SessionName != ""
		envelope.Resume.Policy = "fallback"
		envelope.Resume.RequiredThreadProvider = envelope.Runtime.Provider
		envelope.Resume.RequiredThreadModel = envelope.Runtime.Model
		if legacyNamed {
			envelope.Resume.AllowThreadReuse = true
		}
	}

	var worktreePath, worktreeBranch string
	fail := func(err error) error {
		if worktreePath != "" {
			_ = p.rpcRemoveWorktree(cwd, worktreePath)
		}
		return softenBridgeStartupError(err)
	}
	if hasWorktree && cwd != "" {
		var err error
		worktreePath, worktreeBranch, err = p.rpcCreateWorktree(cwd, branch, newBranch, desiredPath)
		if err != nil {
			return softenBridgeStartupError(fmt.Errorf("t3bridge: create worktree: %w", err))
		}
		cfg.WorkDir = worktreePath
		envelope.Worktree = &WorktreeSection{
			Cwd:          cwd,
			WorktreePath: worktreePath,
			Branch:       worktreeBranch,
		}
		updated, err := json.Marshal(envelope)
		if err == nil {
			if cfg.Env == nil {
				cfg.Env = make(map[string]string)
			}
			cfg.Env["GC_STARTUP_ENVELOPE"] = string(updated)
		}
	}

	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		if worktreePath != "" {
			_ = p.rpcRemoveWorktree(cwd, worktreePath)
		}
		return fmt.Errorf("t3bridge: encode startup envelope: %w", err)
	}
	if cfg.Env == nil {
		cfg.Env = make(map[string]string)
	}
	cfg.Env["GC_STARTUP_ENVELOPE"] = string(envelopeJSON)

	snapshot, err := p.rpcSnapshot()
	if err != nil {
		return fail(fmt.Errorf("t3bridge: load snapshot: %w", err))
	}

	existingThread := snapshotThreadBySessionName(snapshot, name)
	existingBinding := snapshotThreadBinding(existingThread)

	threadTitle := deriveThreadTitle(name, envelope)
	projectWorkspaceRoot := deriveProjectWorkspaceRoot(cfg.WorkDir, envelope)
	projectTitle := deriveProjectTitle(name, projectWorkspaceRoot, envelope)

	projectID := ""
	threadID := ""

	if existingBinding != nil {
		fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) found existing binding thread=%s project=%s\n", name, existingBinding.ThreadID, existingBinding.ProjectID) //nolint:errcheck
		reuse := DecideThreadReuse(ReuseCheck{
			Desired:       envelope,
			Stored:        storedEnvelopeFromThread(existingThread),
			ThreadActive:  threadIsActive(snapshot, existingBinding.ThreadID),
			ProjectActive: projectIsActive(snapshot, existingBinding.ProjectID),
		})

		switch reuse.Decision {
		case ReuseDecisionReuse, ReuseDecisionRebind:
			fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) reuse decision=%s thread=%s\n", name, reuse.Decision, existingBinding.ThreadID) //nolint:errcheck
			projectID = existingBinding.ProjectID
			threadID = existingBinding.ThreadID
			if reuse.Decision == ReuseDecisionRebind {
				_ = p.dispatchThreadModelSelection(threadID, providerName, modelName)
			}
			if p.threadSessionStatus(threadID) != "running" {
				_ = p.dispatchThreadSessionStop(threadID)
			}
			binding := threadBinding{
				ProjectID:   projectID,
				ThreadID:    threadID,
				SessionName: name,
				Agent:       envelope.GC.Agent,
				WorkDir:     cfg.WorkDir,
				Provider:    providerName,
				Model:       modelName,
			}
			p.setRecentStart(name, time.Now())
			_ = p.dispatchThreadMeta(threadID, buildGCMetadata(envelope, providerName, buildThreadEnv(cfg.Env)))
			if worktreePath != "" {
				_ = p.rpcUpdateThreadMeta(threadID, worktreeBranch, worktreePath)
			}
			_ = p.dispatchActivity(threadID, "gc.session.reused", "GC session reused", map[string]interface{}{
				"agent":       envelope.GC.Agent,
				"template":    envelope.GC.Template,
				"convoyId":    envelope.Assignment.ConvoyID,
				"convoyTitle": envelope.Assignment.ConvoyTitle,
				"provider":    providerName,
				"model":       modelName,
				"workDir":     cfg.WorkDir,
				"sessionName": name,
				"decision":    string(reuse.Decision),
			})
			p.ensureEventWatcher(name, cfg, binding, envelope, providerName)
			return nil
		default:
			fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) discard existing thread=%s decision=%s\n", name, existingBinding.ThreadID, reuse.Decision) //nolint:errcheck
			if existingBinding.ThreadID != "" {
				_ = p.dispatchThreadMeta(existingBinding.ThreadID, map[string]interface{}{"gc.state": "archived"})
				_ = p.dispatchThreadSessionStop(existingBinding.ThreadID)
				_ = p.dispatchThreadArchive(existingBinding.ThreadID)
			}
		}
	}

	projectID = resolveActiveProjectID(snapshot, projectWorkspaceRoot)
	if projectID == "" {
		projectID = uuid.NewString()
		fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) creating project id=%s title=%s root=%s\n", name, projectID, projectTitle, projectWorkspaceRoot) //nolint:errcheck
		if err := p.dispatchProjectCreate(projectID, projectTitle, projectWorkspaceRoot, providerName, modelName); err != nil {
			return fail(err)
		}
	}

	threadID = uuid.NewString()
	createBranch := ""
	createWorktreePath := ""
	if worktreePath != "" {
		createBranch = worktreeBranch
		createWorktreePath = worktreePath
	}
	fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) creating thread id=%s project=%s title=%s branch=%s worktree=%s\n", //nolint:errcheck
		name, threadID, projectID, threadTitle, createBranch, createWorktreePath)
	initialGCMetadata := buildGCMetadata(envelope, providerName, buildThreadEnv(cfg.Env))
	if err := p.dispatchThreadCreate(
		threadID,
		projectID,
		threadTitle,
		providerName,
		modelName,
		createBranch,
		createWorktreePath,
		initialGCMetadata,
	); err != nil {
		return fail(err)
	}

	binding := threadBinding{
		ProjectID:   projectID,
		ThreadID:    threadID,
		SessionName: name,
		Agent:       envelope.GC.Agent,
		WorkDir:     cfg.WorkDir,
		Provider:    providerName,
		Model:       modelName,
	}
	p.setRecentStart(name, time.Now())
	fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) writing gc metadata thread=%s\n", name, threadID) //nolint:errcheck
	_ = p.dispatchThreadMeta(threadID, initialGCMetadata)
	_ = p.dispatchActivity(threadID, "gc.session.started", "GC session started", map[string]interface{}{
		"agent":       envelope.GC.Agent,
		"rig":         envelope.GC.RigName,
		"city":        envelope.GC.CityName,
		"template":    envelope.GC.Template,
		"beadId":      envelope.Assignment.BeadID,
		"beadTitle":   envelope.Assignment.BeadTitle,
		"convoyId":    envelope.Assignment.ConvoyID,
		"convoyTitle": envelope.Assignment.ConvoyTitle,
		"provider":    providerName,
		"model":       modelName,
		"workDir":     cfg.WorkDir,
		"sessionName": name,
	})
	p.ensureEventWatcher(name, cfg, binding, envelope, providerName)

	if prompt := strings.TrimSpace(envelope.Startup.StartupPrompt); prompt != "" {
		fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) sending startup prompt thread=%s len=%d\n", name, threadID, len(prompt)) //nolint:errcheck
		if err := p.dispatchTurnStart(threadID, prompt, providerName, modelName); err != nil {
			return fail(err)
		}
		_ = p.dispatchActivity(threadID, "gc.prompt.sent", "GC startup prompt sent", map[string]interface{}{
			"textLength": len(prompt),
		})
	}
	nudgeText := strings.TrimSpace(cfg.Nudge)
	if nudgeText == "" {
		nudgeText = strings.TrimSpace(poolKickoffText(envelope))
	}
	if nudgeText != "" {
		fmt.Fprintf(os.Stderr, "t3bridge: Start(%s) sending nudge thread=%s len=%d\n", name, threadID, len(nudgeText)) //nolint:errcheck
		if err := p.dispatchTurnStart(threadID, nudgeText, providerName, modelName); err != nil {
			return fail(err)
		}
		_ = p.dispatchActivity(threadID, "gc.nudge.sent", "GC nudge sent", map[string]interface{}{
			"source": func() string {
				if cfg.Nudge != "" {
					return "startup"
				}
				return "pool-kickoff"
			}(),
			"textLength": len(nudgeText),
		})
	}
	p.clearSnapshotCache()
	return nil
}

// Stop tears down the session's event watcher and stops or archives its T3
// thread; persistent agents are left running.
func (p *Provider) Stop(name string) error {
	p.stopEventWatcher(name)
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if isSoftBridgeUnavailable(err) {
			p.clearRecentStart(name)
			p.clearBridgeMeta(name)
			return nil
		}
		return err
	}
	thread := snapshotThreadBySessionName(snapshot, name)
	binding := snapshotThreadBinding(thread)
	if binding == nil {
		p.clearRecentStart(name)
		p.clearBridgeMeta(name)
		return nil
	}

	if p.isPersistentAgent(thread) {
		p.recordStateChange(binding.ThreadID, "persistent-alive", "GC stop skipped (persistent agent)", map[string]interface{}{
			"reason": "persistent agent not killed",
		})
		p.clearBridgeMeta(name)
		return nil
	}

	drained, _ := p.GetMeta(name, "drained")
	p.recordStateChange(binding.ThreadID, "stopped", "GC session stopped", nil)
	_ = p.dispatchThreadSessionStop(binding.ThreadID)

	if drained == "1" {
		_ = p.dispatchThreadMeta(binding.ThreadID, map[string]interface{}{"gc.state": "archived"})
		p.removeWorktreeForThread(thread)
	}
	p.clearRecentStart(name)
	p.clearBridgeMeta(name)
	p.clearSnapshotCache()
	return nil
}

// Interrupt cancels the in-flight turn for the named session, if any.
func (p *Provider) Interrupt(name string) error {
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if isSoftBridgeUnavailable(err) {
			return nil
		}
		return err
	}
	binding := snapshotThreadBinding(snapshotThreadBySessionName(snapshot, name))
	if binding == nil {
		return nil
	}
	if err := p.dispatchTurnInterrupt(binding.ThreadID); err != nil {
		return err
	}
	p.clearSnapshotCache()
	return nil
}

// IsAttached reports whether a local terminal is attached. T3 sessions are
// headless, so this is always false.
func (p *Provider) IsAttached(_ string) bool {
	return false
}

// Attach reports where the session can be viewed in the T3 Code UI; it never
// attaches a local terminal.
func (p *Provider) Attach(name string) error {
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		return err
	}
	binding := snapshotThreadBinding(snapshotThreadBySessionName(snapshot, name))
	if binding == nil {
		return fmt.Errorf("no session registered for %s", name)
	}
	return fmt.Errorf("session %q is visible in T3 Code UI at http://localhost:5173 (thread %s)", name, binding.ThreadID)
}

// ProcessAlive checks if the session still has a live T3 runtime binding.
// T3 reports active-but-idle threads as "ready", which should count as alive
// for durable named sessions so the reconciler does not keep re-waking them.
func (p *Provider) ProcessAlive(name string, _ []string) bool {
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		return p.withinRecentStart(name)
	}
	binding := snapshotThreadBinding(snapshotThreadBySessionName(snapshot, name))
	if binding == nil {
		return false
	}
	status := p.threadSessionStatus(binding.ThreadID)
	return status == "running" || status == "ready"
}

// Nudge delivers content to the session as a new user turn.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if isSoftBridgeUnavailable(err) {
			return nil
		}
		return err
	}
	thread := snapshotThreadBySessionName(snapshot, name)
	binding := snapshotThreadBinding(thread)
	if binding == nil {
		return nil
	}
	text := runtime.FlattenText(content)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	meta := threadCustomMetadata(thread)
	provider := meta["gc.runtimeProvider"]
	if provider == "" {
		provider = binding.Provider
	}
	model := meta["gc.startupModel"]
	if model == "" {
		model = binding.Model
	}
	return p.dispatchTurnStart(binding.ThreadID, text, provider, model)
}

// SetMeta persists a session metadata value and reflects drain transitions onto
// the T3 thread.
func (p *Provider) SetMeta(name, key, value string) error {
	if err := writeMetaValue(name, key, value); err != nil {
		return err
	}
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if isSoftBridgeUnavailable(err) {
			return nil
		}
		return err
	}
	binding := snapshotThreadBinding(snapshotThreadBySessionName(snapshot, name))
	if binding == nil {
		return nil
	}
	switch {
	case key == "GC_DRAIN":
		p.recordStateChange(binding.ThreadID, "draining", "GC session draining", nil)
	case key == "GC_DRAIN_ACK" && value == "1":
		_ = writeMetaValue(name, "drained", "1")
		p.recordStateChange(binding.ThreadID, "drained", "GC session drained", nil)
	}
	return nil
}

// GetMeta reads a previously stored session metadata value.
func (p *Provider) GetMeta(name, key string) (string, error) {
	return readMetaValue(name, key)
}

// RemoveMeta deletes a session metadata key and reflects drain-clearing onto the
// T3 thread.
func (p *Provider) RemoveMeta(name, key string) error {
	err := removeMetaValue(name, key)
	if err != nil {
		return err
	}
	snapshot, loadErr := p.rpcSnapshot()
	if loadErr != nil {
		if isSoftBridgeUnavailable(loadErr) {
			return nil
		}
		return loadErr
	}
	binding := snapshotThreadBinding(snapshotThreadBySessionName(snapshot, name))
	if binding == nil {
		return nil
	}
	switch key {
	case "GC_DRAIN":
		p.recordStateChange(binding.ThreadID, "active", "GC drain cleared", map[string]interface{}{"reason": "drain cleared"})
	case "GC_DRAIN_ACK":
		_ = removeMetaValue(name, "drained")
		p.recordStateChange(binding.ThreadID, "active", "GC drain acknowledgment cleared", map[string]interface{}{"reason": "drain acknowledgment cleared"})
	}
	return nil
}

// Peek returns a short human-readable summary of the session thread's latest
// activity.
func (p *Provider) Peek(name string, lines int) (string, error) {
	resultMap, err := p.rpcCall("gc.peekThreadMessages", map[string]interface{}{
		"sessionName": name,
		"limit":       lines,
	})
	if err == nil {
		if rawResults, ok := resultMap["results"].([]interface{}); ok && len(rawResults) > 0 {
			builder := &strings.Builder{}
			for i, raw := range rawResults {
				row, _ := raw.(map[string]interface{})
				if row == nil {
					continue
				}
				snippet, _ := row["snippet"].(string)
				if strings.TrimSpace(snippet) == "" {
					continue
				}
				if i > 0 {
					_, _ = io.WriteString(builder, "\n\n")
				}
				_, _ = io.WriteString(builder, snippet)
			}
			if builder.Len() > 0 {
				return builder.String(), nil
			}
		}
	}

	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if isSoftBridgeUnavailable(err) {
			return "t3bridge temporarily unavailable", nil
		}
		return "", err
	}
	binding := snapshotThreadBinding(snapshotThreadBySessionName(snapshot, name))
	if binding == nil {
		return "no session registered for " + name, nil
	}
	result, _ := snapshot["result"].(map[string]interface{})
	threads, _ := result["threads"].([]interface{})
	for _, raw := range threads {
		thread, _ := raw.(map[string]interface{})
		if thread == nil {
			continue
		}
		id, _ := thread["id"].(string)
		if id != binding.ThreadID {
			continue
		}
		title, _ := thread["title"].(string)
		model, _ := thread["model"].(string)
		latestTurn, _ := thread["latestTurn"].(map[string]interface{})
		turnState, _ := latestTurn["state"].(string)
		messages, _ := thread["messages"].([]interface{})
		builder := &strings.Builder{}
		if title == "" {
			title = binding.ThreadID
		}
		_, _ = io.WriteString(builder, "Thread: "+title+"\n")
		if model == "" {
			model = "?"
		}
		_, _ = io.WriteString(builder, "Model: "+model+"\n")
		if turnState == "" {
			turnState = "none"
		}
		_, _ = io.WriteString(builder, "Turn: "+turnState+"\n")
		_, _ = io.WriteString(builder, fmt.Sprintf("Messages: %d", len(messages)))
		return builder.String(), nil
	}
	return "Thread " + binding.ThreadID, nil
}

// GetLastActivity returns the timestamp of the session thread's most recent
// update.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if p.withinRecentStart(name) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.recentStarts[name], nil
		}
		if isSoftBridgeUnavailable(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	thread := snapshotThreadBySessionName(snapshot, name)
	if thread == nil {
		if p.withinRecentStart(name) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.recentStarts[name], nil
		}
		return time.Time{}, nil
	}
	return threadUpdatedAt(thread), nil
}

// ClearScrollback is a no-op; T3 sessions keep no local scrollback.
func (p *Provider) ClearScrollback(_ string) error {
	return nil
}

// CopyTo copies a file or directory tree into the session's working directory.
func (p *Provider) CopyTo(name, src, relDst string) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(src) == "" {
		return nil
	}
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		if isSoftBridgeUnavailable(err) {
			return nil
		}
		return nil
	}
	thread := snapshotThreadBySessionName(snapshot, name)
	binding := snapshotThreadBinding(thread)
	workDir := ""
	if binding != nil {
		workDir = strings.TrimSpace(binding.WorkDir)
	}
	if workDir == "" {
		meta := threadCustomMetadata(thread)
		workDir = strings.TrimSpace(meta["gc.startupWorkDir"])
	}
	if workDir == "" {
		return nil
	}

	info, err := os.Stat(src)
	if err != nil {
		return nil
	}
	if info.IsDir() {
		dstRoot := workDir
		if strings.TrimSpace(relDst) != "" {
			var err error
			dstRoot, err = resolveContainedPath(workDir, relDst)
			if err != nil {
				return nil
			}
		}
		if err := copyDirContents(src, dstRoot); err != nil {
			return nil
		}
		return nil
	}

	fileRelDst := strings.TrimSpace(relDst)
	if strings.TrimSpace(relDst) != "" {
		if _, err := resolveContainedPath(workDir, fileRelDst); err != nil {
			return nil
		}
	} else {
		fileRelDst = filepath.Base(src)
	}
	if err := copyFileToPath(src, workDir, fileRelDst); err != nil {
		return nil
	}
	return nil
}

// SendKeys is a no-op; T3 sessions do not accept raw key input.
func (p *Provider) SendKeys(_ string, _ ...string) error {
	return nil
}

// RunLive is a no-op; t3bridge does not support live interactive runs.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil
}

// Capabilities reports the optional provider features t3bridge supports.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportActivity: true,
	}
}

// SleepCapability reports how the named session may be put to sleep.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}
