package credentialprovider

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

var credentialTestNow = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

type recordingCommandRunner struct {
	argv        []string
	stdin       []byte
	environment []string
	deadline    time.Time
	output      commandOutput
	err         error
	calls       int
}

func (runner *recordingCommandRunner) run(ctx context.Context, argv []string, stdin []byte, environment []string) (commandOutput, error) {
	runner.calls++
	runner.argv = append([]string(nil), argv...)
	runner.stdin = append([]byte(nil), stdin...)
	runner.environment = append([]string(nil), environment...)
	runner.deadline, _ = ctx.Deadline()
	return runner.output, runner.err
}

func newRecordingProvider(t *testing.T, output commandOutput, runErr error) (*Provider, *recordingCommandRunner) {
	t.Helper()
	provider, err := New([]string{"gasworks", "credential-provider"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runner := &recordingCommandRunner{output: output, err: runErr}
	provider.run = runner.run
	provider.now = func() time.Time { return credentialTestNow }
	provider.environ = func() []string {
		return []string{
			"PATH=/usr/bin", "HOME=/home/alice", "GASWORKS_CONFIG_DIR=/run/alice/gasworks",
			"XDG_CONFIG_HOME=/home/alice/.config", "GASWORKS_STS_URL=https://sts.example.test",
			"GASWORKS_OIDC_ISSUER=https://id.example.test", "GASWORKS_CLIENT_ID=gasworks-test",
			"GASWORKS_LOOPBACK_PORT=must-not-leak", "HTTPS_PROXY=http://proxy.internal",
			"no_proxy=localhost", "SSL_CERT_FILE=/etc/ssl/custom.pem", "LD_PRELOAD=must-not-leak",
			"APPDATA=C:\\Users\\alice\\AppData\\Roaming", "USERPROFILE=C:\\Users\\alice",
			"SystemRoot=C:\\Windows", "COMSPEC=C:\\Windows\\System32\\cmd.exe",
			"GC_EXEC_INFO=must-not-leak", "AWS_SECRET_ACCESS_KEY=must-not-leak", "UNRELATED=drop",
		}
	}
	return provider, runner
}

func validCredentialOutput() commandOutput {
	return commandOutput{stdout: []byte(`{
		"version":"gascity.dev/credential-provider/v1",
		"kind":"Credential",
		"access_token":"opaque-token",
		"authorization_scheme":"Bearer",
		"expires_at":"2026-07-16T12:05:00Z",
		"audience":"manifold",
		"scopes":["manifold:pool:acme","manifold:proxy"]
	}`)}
}

func validCredentialRequest() Request {
	return Request{
		Audience:       "manifold",
		RequiredScopes: []string{"manifold:proxy", "manifold:pool:acme"},
		Org:            "org-acme",
	}
}

func TestProviderMintRunsBoundedArgvProtocol(t *testing.T) {
	provider, runner := newRecordingProvider(t, validCredentialOutput(), nil)
	deadlineLowerBound := time.Now().Add(helperTimeout)

	credential, err := provider.Mint(context.Background(), validCredentialRequest())
	deadlineUpperBound := time.Now().Add(helperTimeout)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if credential.AccessToken != "opaque-token" || credential.AuthorizationScheme != "Bearer" ||
		credential.Audience != "manifold" || !credential.ExpiresAt.Equal(credentialTestNow.Add(5*time.Minute)) ||
		!slices.Equal(credential.Scopes, []string{"manifold:pool:acme", "manifold:proxy"}) {
		t.Fatalf("credential = %+v", credential)
	}
	if !slices.Equal(runner.argv, []string{"gasworks", "credential-provider"}) {
		t.Fatalf("argv = %q", runner.argv)
	}
	wantStdin := `{"version":"gascity.dev/credential-provider/v1","audience":"manifold","required_scopes":["manifold:pool:acme","manifold:proxy"],"org":"org-acme","force_refresh":false,"interactive":false}`
	if got := string(runner.stdin); got != wantStdin {
		t.Fatalf("stdin = %s, want %s", got, wantStdin)
	}
	if runner.deadline.Before(deadlineLowerBound) || runner.deadline.After(deadlineUpperBound) {
		t.Fatalf("deadline = %v, want a %v whole-process bound", runner.deadline, helperTimeout)
	}

	var request struct {
		Version        string   `json:"version"`
		Audience       string   `json:"audience"`
		RequiredScopes []string `json:"required_scopes"`
		Org            string   `json:"org"`
		ForceRefresh   bool     `json:"force_refresh"`
		Interactive    bool     `json:"interactive"`
	}
	if err := json.Unmarshal(runner.stdin, &request); err != nil {
		t.Fatalf("stdin is not JSON: %v", err)
	}
	if request.Version != ProtocolVersion || request.Audience != "manifold" || request.Org != "org-acme" ||
		request.ForceRefresh || request.Interactive ||
		!slices.Equal(request.RequiredScopes, []string{"manifold:pool:acme", "manifold:proxy"}) {
		t.Fatalf("request = %+v", request)
	}
	wantEnvironment := []string{
		"GASWORKS_CLIENT_ID=gasworks-test", "GASWORKS_CONFIG_DIR=/run/alice/gasworks",
		"GASWORKS_OIDC_ISSUER=https://id.example.test", "GASWORKS_STS_URL=https://sts.example.test",
		"HTTPS_PROXY=http://proxy.internal", "PATH=/usr/bin", "SSL_CERT_FILE=/etc/ssl/custom.pem", "no_proxy=localhost",
	}
	if runtime.GOOS == "windows" {
		wantEnvironment = append(wantEnvironment,
			"APPDATA=C:\\Users\\alice\\AppData\\Roaming", "COMSPEC=C:\\Windows\\System32\\cmd.exe",
			"SystemRoot=C:\\Windows", "USERPROFILE=C:\\Users\\alice",
		)
	} else {
		wantEnvironment = append(wantEnvironment, "HOME=/home/alice", "XDG_CONFIG_HOME=/home/alice/.config")
	}
	gotEnvironment := append([]string(nil), runner.environment...)
	slices.Sort(gotEnvironment)
	slices.Sort(wantEnvironment)
	if !slices.Equal(gotEnvironment, wantEnvironment) {
		t.Fatalf("environment = %q, want %q", runner.environment, wantEnvironment)
	}
}

func TestCredentialProviderWholeResponseDeadlineIsTenSeconds(t *testing.T) {
	if helperTimeout != 10*time.Second {
		t.Fatalf("helperTimeout = %v, want 10s", helperTimeout)
	}
}

func TestNewPreservesEmptyAndWhitespaceArguments(t *testing.T) {
	provider, err := New([]string{"gasworks", "", " "})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider.run = func(_ context.Context, argv []string, _ []byte, _ []string) (commandOutput, error) {
		if !slices.Equal(argv, []string{"gasworks", "", " "}) {
			t.Fatalf("argv = %q", argv)
		}
		return validCredentialOutput(), nil
	}
	provider.now = func() time.Time { return credentialTestNow }
	if _, err := provider.Mint(context.Background(), validCredentialRequest()); err != nil {
		t.Fatalf("Mint: %v", err)
	}
}

func TestProviderMintSerializesForceRefreshWithoutMutatingRequest(t *testing.T) {
	provider, runner := newRecordingProvider(t, validCredentialOutput(), nil)
	request := validCredentialRequest()
	request.ForceRefresh = true
	wantScopes := append([]string(nil), request.RequiredScopes...)

	if _, err := provider.Mint(context.Background(), request); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !slices.Equal(request.RequiredScopes, wantScopes) {
		t.Fatalf("request scopes mutated: got %q, want %q", request.RequiredScopes, wantScopes)
	}
	var payload struct {
		ForceRefresh bool `json:"force_refresh"`
	}
	if err := json.Unmarshal(runner.stdin, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if !payload.ForceRefresh {
		t.Fatal("force_refresh = false, want true")
	}
}

func TestNewRejectsInvalidArgv(t *testing.T) {
	for _, argv := range [][]string{
		nil,
		{},
		{""},
		{"   "},
		{"gasworks\x00forged"},
		{"gasworks", "credential-provider\x00forged"},
	} {
		if _, err := New(argv); err == nil {
			t.Fatalf("New(%q) succeeded", argv)
		}
	}
}

func TestNewDefensivelyCopiesArgv(t *testing.T) {
	argv := []string{"gasworks", "credential-provider"}
	provider, err := New(argv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	argv[0] = "forged"
	provider.run = func(_ context.Context, got []string, _ []byte, _ []string) (commandOutput, error) {
		if got[0] != "gasworks" {
			t.Fatalf("argv mutated through caller slice: %q", got)
		}
		return validCredentialOutput(), nil
	}
	provider.now = func() time.Time { return credentialTestNow }
	if _, err := provider.Mint(context.Background(), validCredentialRequest()); err != nil {
		t.Fatalf("Mint: %v", err)
	}
}

func TestProviderMintRejectsInvalidRequestBeforeExec(t *testing.T) {
	tests := []struct {
		name    string
		request Request
	}{
		{name: "missing audience", request: Request{RequiredScopes: []string{"scope:a"}}},
		{name: "whitespace audience", request: Request{Audience: "mani fold", RequiredScopes: []string{"scope:a"}}},
		{name: "control audience", request: Request{Audience: "manifold\x00", RequiredScopes: []string{"scope:a"}}},
		{name: "whitespace org", request: Request{Audience: "manifold", Org: "ac me", RequiredScopes: []string{"scope:a"}}},
		{name: "missing scopes", request: Request{Audience: "manifold"}},
		{name: "empty scope", request: Request{Audience: "manifold", RequiredScopes: []string{""}}},
		{name: "whitespace scope", request: Request{Audience: "manifold", RequiredScopes: []string{"scope: a"}}},
		{name: "duplicate scope", request: Request{Audience: "manifold", RequiredScopes: []string{"scope:a", "scope:a"}}},
		{name: "too many scopes", request: Request{Audience: "manifold", RequiredScopes: repeatedScopes(maxScopeCount + 1)}},
		{name: "oversized audience", request: Request{Audience: strings.Repeat("a", maxValueBytes+1), RequiredScopes: []string{"scope:a"}}},
		{name: "oversized org", request: Request{Audience: "manifold", Org: strings.Repeat("o", maxValueBytes+1), RequiredScopes: []string{"scope:a"}}},
		{name: "oversized scope", request: Request{Audience: "manifold", RequiredScopes: []string{strings.Repeat("s", maxValueBytes+1)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, runner := newRecordingProvider(t, validCredentialOutput(), nil)
			if _, err := provider.Mint(context.Background(), test.request); err == nil {
				t.Fatal("Mint succeeded")
			}
			if runner.calls != 0 {
				t.Fatalf("invalid request executed provider %d times", runner.calls)
			}
		})
	}
}

func repeatedScopes(count int) []string {
	scopes := make([]string, count)
	for index := range scopes {
		scopes[index] = "scope:" + strings.Repeat("a", index+1)
	}
	return scopes
}

func TestProviderMintRejectsInvalidCredentialResponses(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{
			"version": "gascity.dev/credential-provider/v1", "kind": "Credential",
			"access_token": "opaque-token", "authorization_scheme": "Bearer",
			"expires_at": "2026-07-16T12:05:00Z", "audience": "manifold",
			"scopes": []string{"manifold:pool:acme", "manifold:proxy"},
		}
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong version", mutate: func(response map[string]any) { response["version"] = "v0" }},
		{name: "wrong kind", mutate: func(response map[string]any) { response["kind"] = "Token" }},
		{name: "empty token", mutate: func(response map[string]any) { response["access_token"] = "" }},
		{name: "token whitespace", mutate: func(response map[string]any) { response["access_token"] = "opaque token" }},
		{name: "token carriage return", mutate: func(response map[string]any) { response["access_token"] = "opaque\rtoken" }},
		{name: "token line feed", mutate: func(response map[string]any) { response["access_token"] = "opaque\ntoken" }},
		{name: "token nul", mutate: func(response map[string]any) { response["access_token"] = "opaque\x00token" }},
		{name: "token nonbreaking space", mutate: func(response map[string]any) { response["access_token"] = "opaque\u00a0token" }},
		{name: "token non-ASCII", mutate: func(response map[string]any) { response["access_token"] = "opaque-é" }},
		{name: "token invalid bearer character", mutate: func(response map[string]any) { response["access_token"] = "opaque:token" }},
		{name: "token padding in middle", mutate: func(response map[string]any) { response["access_token"] = "opaque=token" }},
		{name: "wrong scheme", mutate: func(response map[string]any) { response["authorization_scheme"] = "DPoP" }},
		{name: "wrong audience", mutate: func(response map[string]any) { response["audience"] = "crucible" }},
		{name: "malformed expiry", mutate: func(response map[string]any) { response["expires_at"] = "tomorrow" }},
		{name: "expired", mutate: func(response map[string]any) { response["expires_at"] = "2026-07-16T11:59:59Z" }},
		{name: "expiry now", mutate: func(response map[string]any) { response["expires_at"] = "2026-07-16T12:00:00Z" }},
		{name: "missing scope", mutate: func(response map[string]any) { response["scopes"] = []string{"manifold:proxy"} }},
		{name: "extra scope", mutate: func(response map[string]any) {
			response["scopes"] = []string{"manifold:pool:acme", "manifold:proxy", "manifold:admin"}
		}},
		{name: "duplicate scope", mutate: func(response map[string]any) { response["scopes"] = []string{"manifold:proxy", "manifold:proxy"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := valid()
			test.mutate(response)
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			provider, _ := newRecordingProvider(t, commandOutput{stdout: encoded}, nil)
			if _, err := provider.Mint(context.Background(), validCredentialRequest()); err == nil {
				t.Fatal("Mint succeeded")
			}
		})
	}
}

func TestProviderMintCanonicalizesUnsortedCredentialScopes(t *testing.T) {
	output := validCredentialOutput()
	output.stdout = []byte(strings.Replace(
		string(output.stdout),
		`["manifold:pool:acme","manifold:proxy"]`,
		`["manifold:proxy","manifold:pool:acme"]`,
		1,
	))
	provider, _ := newRecordingProvider(t, output, nil)

	credential, err := provider.Mint(context.Background(), validCredentialRequest())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !slices.Equal(credential.Scopes, []string{"manifold:pool:acme", "manifold:proxy"}) {
		t.Fatalf("scopes = %q", credential.Scopes)
	}
}

func TestProviderMintRejectsMalformedResponseShapes(t *testing.T) {
	tests := []string{
		`not-json`,
		`[]`,
		`{"version":"gascity.dev/credential-provider/v1"}`,
		`{"version":"gascity.dev/credential-provider/v1","kind":"Credential","access_token":"opaque-token","authorization_scheme":"Bearer","expires_at":"2026-07-16T12:05:00Z","audience":"manifold","scopes":["manifold:pool:acme","manifold:proxy"],"extra":true}`,
		`{"version":"gascity.dev/credential-provider/v1","kind":"Credential","access_token":"opaque-token","access_token":"forged-token","authorization_scheme":"Bearer","expires_at":"2026-07-16T12:05:00Z","audience":"manifold","scopes":["manifold:pool:acme","manifold:proxy"]}`,
		`{"Version":"gascity.dev/credential-provider/v1","kind":"Credential","access_token":"opaque-token","authorization_scheme":"Bearer","expires_at":"2026-07-16T12:05:00Z","audience":"manifold","scopes":["manifold:pool:acme","manifold:proxy"]}`,
		`{"version":"gascity.dev/credential-provider/v1","kind":"Credential","access_token":"opaque-token","authorization_scheme":"Bearer","expires_at":"2026-07-16T12:05:00Z","audience":"manifold","scopes":["manifold:pool:acme","manifold:proxy"]} {}`,
	}
	for index, response := range tests {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			provider, _ := newRecordingProvider(t, commandOutput{stdout: []byte(response)}, nil)
			if _, err := provider.Mint(context.Background(), validCredentialRequest()); err == nil {
				t.Fatal("Mint succeeded")
			}
		})
	}
}

func TestProviderMintReturnsTypedSecretSafeProviderError(t *testing.T) {
	provider, _ := newRecordingProvider(t, commandOutput{stdout: []byte(`{
		"version":"gascity.dev/credential-provider/v1",
		"kind":"Error",
		"code":"interaction_required",
		"message":"secret-that-must-not-surface"
	}`), stderr: []byte("stderr-secret")}, errors.New("exit status 1: token-secret"))

	_, err := provider.Mint(context.Background(), validCredentialRequest())
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code != "interaction_required" {
		t.Fatalf("error = %T %v", err, err)
	}
	for _, secret := range []string{"secret-that-must-not-surface", "stderr-secret", "token-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposed %q: %v", secret, err)
		}
	}
}

func TestProviderMintRejectsMalformedErrorResponses(t *testing.T) {
	tests := []string{
		`{"version":"gascity.dev/credential-provider/v1","kind":"Error","code":"interaction_required"}`,
		`{"version":"gascity.dev/credential-provider/v1","kind":"Error","code":"interaction_required","message":""}`,
		`{"version":"gascity.dev/credential-provider/v1","kind":"Error","code":"interaction_required","message":"safe","access_token":"secret"}`,
		`{"version":"gascity.dev/credential-provider/v1","kind":"Error","code":"interaction_required","code":"access_denied","message":"safe"}`,
		`{"version":"gascity.dev/credential-provider/v1","kind":"Error","Code":"interaction_required","message":"safe"}`,
	}
	for index, response := range tests {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			provider, _ := newRecordingProvider(t, commandOutput{stdout: []byte(response)}, errors.New("runner-secret"))
			if _, err := provider.Mint(context.Background(), validCredentialRequest()); err == nil {
				t.Fatal("Mint succeeded")
			}
		})
	}
}

func TestProviderMintRejectsCredentialFromFailedProcess(t *testing.T) {
	provider, _ := newRecordingProvider(t, validCredentialOutput(), errors.New("runner-secret"))
	_, err := provider.Mint(context.Background(), validCredentialRequest())
	if err == nil {
		t.Fatal("Mint succeeded")
	}
	if strings.Contains(err.Error(), "runner-secret") {
		t.Fatalf("error exposed runner failure: %v", err)
	}
}

func TestProviderMintDoesNotExposeMalformedOutputOrStderr(t *testing.T) {
	provider, _ := newRecordingProvider(t, commandOutput{
		stdout: []byte(`{"access_token":"stdout-secret"}`),
		stderr: []byte("stderr-secret"),
	}, errors.New("runner-secret"))

	_, err := provider.Mint(context.Background(), validCredentialRequest())
	if err == nil {
		t.Fatal("Mint succeeded")
	}
	for _, secret := range []string{"stdout-secret", "stderr-secret", "runner-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposed %q: %v", secret, err)
		}
	}
}

func TestProviderMintRejectsBoundedOutputOverflow(t *testing.T) {
	for _, output := range []commandOutput{
		{stdout: []byte("prefix"), stdoutOverflow: true},
		{stdout: validCredentialOutput().stdout, stderr: []byte("prefix"), stderrOverflow: true},
	} {
		provider, _ := newRecordingProvider(t, output, nil)
		if _, err := provider.Mint(context.Background(), validCredentialRequest()); err == nil {
			t.Fatal("Mint succeeded with overflowing output")
		}
	}
}

func TestProviderMintPreservesParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider, _ := newRecordingProvider(t, commandOutput{}, context.Canceled)
	_, err := provider.Mint(ctx, validCredentialRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestProviderMintUsesShorterParentDeadline(t *testing.T) {
	provider, runner := newRecordingProvider(t, validCredentialOutput(), nil)
	parentDeadline := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()

	if _, err := provider.Mint(ctx, validCredentialRequest()); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !runner.deadline.Equal(parentDeadline) {
		t.Fatalf("runner deadline = %v, want parent deadline %v", runner.deadline, parentDeadline)
	}
}

func TestProviderMintRejectsUnknownProviderErrorCode(t *testing.T) {
	provider, _ := newRecordingProvider(t, commandOutput{stdout: []byte(`{
		"version":"gascity.dev/credential-provider/v1",
		"kind":"Error",
		"code":"token-secret",
		"message":"message-secret"
	}`)}, errors.New("runner-secret"))

	_, err := provider.Mint(context.Background(), validCredentialRequest())
	if err == nil {
		t.Fatal("Mint succeeded")
	}
	for _, secret := range []string{"token-secret", "message-secret", "runner-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposed %q: %v", secret, err)
		}
	}
}

func TestBoundedBufferRetainsPrefixAndDiscardsOverflow(t *testing.T) {
	buffer := boundedBuffer{limit: 5}

	for _, chunk := range [][]byte{[]byte("abc"), []byte("def"), []byte("ignored")} {
		written, err := buffer.Write(chunk)
		if err != nil || written != len(chunk) {
			t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", chunk, written, err, len(chunk))
		}
	}
	if got := string(buffer.bytes()); got != "abcde" {
		t.Fatalf("bytes = %q, want %q", got, "abcde")
	}
	if !buffer.overflowed() {
		t.Fatal("overflowed = false, want true")
	}
}
