// Package credentialprovider executes noninteractive credential-provider
// commands over the Gas City v1 JSON protocol.
package credentialprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// ProtocolVersion identifies the credential-provider JSON contract.
	ProtocolVersion = "gascity.dev/credential-provider/v1"

	helperTimeout = 10 * time.Second
	maxScopeCount = 64
	maxValueBytes = 512
)

type commandOutput struct {
	stdout         []byte
	stderr         []byte
	stdoutOverflow bool
	stderrOverflow bool
}

// Request describes the exact credential a consumer needs.
type Request struct {
	Audience       string
	RequiredScopes []string
	Org            string
	ForceRefresh   bool
}

// Credential is an opaque provider-minted bearer and its validated metadata.
type Credential struct {
	AccessToken         string
	AuthorizationScheme string
	ExpiresAt           time.Time
	Audience            string
	Scopes              []string
}

// ProviderError is a stable, secret-safe error returned by the provider.
type ProviderError struct {
	Code string
}

// Error implements error without exposing the provider's message or output.
func (e *ProviderError) Error() string {
	if e == nil {
		return "credential provider failed"
	}
	return "credential provider failed: " + e.Code
}

// Provider executes one immutable credential-provider argv.
type Provider struct {
	argv    []string
	run     func(context.Context, []string, []byte, []string) (commandOutput, error)
	now     func() time.Time
	environ func() []string
}

// New constructs a Provider that executes argv directly, without a shell.
func New(argv []string) (*Provider, error) {
	if len(argv) == 0 {
		return nil, errors.New("credential provider argv is empty")
	}
	argvCopy := append([]string(nil), argv...)
	if strings.TrimSpace(argvCopy[0]) == "" || strings.ContainsRune(argvCopy[0], '\x00') {
		return nil, errors.New("credential provider argv is invalid")
	}
	for _, argument := range argvCopy[1:] {
		if strings.ContainsRune(argument, '\x00') {
			return nil, errors.New("credential provider argv is invalid")
		}
	}
	return &Provider{
		argv:    argvCopy,
		run:     runCommand,
		now:     time.Now,
		environ: os.Environ,
	}, nil
}

type wireRequest struct {
	Version        string   `json:"version"`
	Audience       string   `json:"audience"`
	RequiredScopes []string `json:"required_scopes"`
	Org            string   `json:"org"`
	ForceRefresh   bool     `json:"force_refresh"`
	Interactive    bool     `json:"interactive"`
}

type wireResponse struct {
	Version             string   `json:"version"`
	Kind                string   `json:"kind"`
	AccessToken         string   `json:"access_token"`
	AuthorizationScheme string   `json:"authorization_scheme"`
	ExpiresAt           string   `json:"expires_at"`
	Audience            string   `json:"audience"`
	Scopes              []string `json:"scopes"`
	Code                string   `json:"code"`
	Message             string   `json:"message"`
}

// Mint invokes the provider once and validates its complete response.
func (p *Provider) Mint(ctx context.Context, request Request) (Credential, error) {
	if ctx == nil {
		return Credential{}, errors.New("credential provider context is nil")
	}
	scopes, err := validateRequest(request)
	if err != nil {
		return Credential{}, err
	}
	payload, err := json.Marshal(wireRequest{
		Version:        ProtocolVersion,
		Audience:       request.Audience,
		RequiredScopes: scopes,
		Org:            request.Org,
		ForceRefresh:   request.ForceRefresh,
		Interactive:    false,
	})
	if err != nil {
		return Credential{}, errors.New("encode credential provider request")
	}

	runCtx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	output, runErr := p.run(runCtx, append([]string(nil), p.argv...), payload, minimalEnvironment(p.environ()))
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if err := runCtx.Err(); err != nil {
		return Credential{}, fmt.Errorf("credential provider deadline: %w", err)
	}
	if output.stdoutOverflow || output.stderrOverflow {
		return Credential{}, errors.New("credential provider output exceeded its limit")
	}

	credential, responseErr := decodeResponse(output.stdout, request.Audience, scopes, p.now())
	if responseErr != nil {
		return Credential{}, responseErr
	}
	if runErr != nil {
		return Credential{}, errors.New("credential provider process failed")
	}
	return credential, nil
}

func validateRequest(request Request) ([]string, error) {
	if !validValue(request.Audience) || (request.Org != "" && !validValue(request.Org)) {
		return nil, errors.New("credential provider request is invalid")
	}
	if len(request.RequiredScopes) == 0 || len(request.RequiredScopes) > maxScopeCount {
		return nil, errors.New("credential provider request is invalid")
	}
	scopes := append([]string(nil), request.RequiredScopes...)
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if !validValue(scope) {
			return nil, errors.New("credential provider request is invalid")
		}
		if _, duplicate := seen[scope]; duplicate {
			return nil, errors.New("credential provider request is invalid")
		}
		seen[scope] = struct{}{}
	}
	sort.Strings(scopes)
	return scopes, nil
}

func decodeResponse(raw []byte, audience string, requestedScopes []string, now time.Time) (Credential, error) {
	fields, err := responseFields(raw)
	if err != nil {
		return Credential{}, errors.New("credential provider response is invalid")
	}
	var response wireResponse
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return Credential{}, errors.New("credential provider response is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Credential{}, errors.New("credential provider response is invalid")
	}
	if response.Version != ProtocolVersion {
		return Credential{}, errors.New("credential provider response is invalid")
	}

	switch response.Kind {
	case "Error":
		if !hasExactFields(fields, "version", "kind", "code", "message") ||
			!validProviderErrorCode(response.Code) || strings.TrimSpace(response.Message) == "" {
			return Credential{}, errors.New("credential provider response is invalid")
		}
		return Credential{}, &ProviderError{Code: response.Code}
	case "Credential":
		if !hasExactFields(fields, "version", "kind", "access_token", "authorization_scheme", "expires_at", "audience", "scopes") {
			return Credential{}, errors.New("credential provider response is invalid")
		}
	default:
		return Credential{}, errors.New("credential provider response is invalid")
	}

	if !validOpaqueToken(response.AccessToken) || response.AuthorizationScheme != "Bearer" || response.Audience != audience {
		return Credential{}, errors.New("credential provider response is invalid")
	}
	expiresAt, err := time.Parse(time.RFC3339, response.ExpiresAt)
	if err != nil || !expiresAt.After(now) {
		return Credential{}, errors.New("credential provider response is invalid")
	}
	responseScopes, err := validateResponseScopes(response.Scopes)
	if err != nil || !slices.Equal(responseScopes, requestedScopes) {
		return Credential{}, errors.New("credential provider response is invalid")
	}
	return Credential{
		AccessToken:         response.AccessToken,
		AuthorizationScheme: response.AuthorizationScheme,
		ExpiresAt:           expiresAt.UTC(),
		Audience:            response.Audience,
		Scopes:              responseScopes,
	}, nil
}

func responseFields(raw []byte) (map[string]struct{}, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("response is not UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("response is not an object")
	}
	fields := make(map[string]struct{})
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		field, ok := fieldToken.(string)
		if !ok || !knownResponseField(field) {
			return nil, errors.New("response field is invalid")
		}
		if _, duplicate := fields[field]; duplicate {
			return nil, errors.New("response field is duplicated")
		}
		fields[field] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("response has trailing data")
	}
	return fields, nil
}

func knownResponseField(field string) bool {
	switch field {
	case "version", "kind", "access_token", "authorization_scheme", "expires_at", "audience", "scopes", "code", "message":
		return true
	default:
		return false
	}
}

func hasExactFields(fields map[string]struct{}, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, present := fields[name]; !present {
			return false
		}
	}
	return true
}

func validProviderErrorCode(code string) bool {
	switch code {
	case "invalid_request", "interaction_required", "access_denied", "temporarily_unavailable":
		return true
	default:
		return false
	}
}

func validateResponseScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 || len(scopes) > maxScopeCount {
		return nil, errors.New("response scopes are invalid")
	}
	validated := append([]string(nil), scopes...)
	seen := make(map[string]struct{}, len(validated))
	for _, scope := range validated {
		if !validValue(scope) {
			return nil, errors.New("response scopes are invalid")
		}
		if _, duplicate := seen[scope]; duplicate {
			return nil, errors.New("response scopes are invalid")
		}
		seen[scope] = struct{}{}
	}
	sort.Strings(validated)
	return validated, nil
}

func validValue(value string) bool {
	if value == "" || len(value) > maxValueBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validOpaqueToken(token string) bool {
	if token == "" {
		return false
	}
	seenValue := false
	seenPadding := false
	for index := 0; index < len(token); index++ {
		character := token[index]
		if character == '=' {
			seenPadding = true
			continue
		}
		if seenPadding {
			return false
		}
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') {
			seenValue = true
			continue
		}
		switch character {
		case '-', '.', '_', '~', '+', '/':
			seenValue = true
			continue
		}
		return false
	}
	return seenValue
}

func minimalEnvironment(source []string) []string {
	selected := make(map[string]string)
	for _, entry := range source {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, '\x00') {
			continue
		}
		lookupKey := environmentLookupKey(key)
		if allowedEnvironmentKey(lookupKey) {
			selected[lookupKey] = entry
		}
	}
	environment := make([]string, 0, len(selected))
	for _, entry := range selected {
		environment = append(environment, entry)
	}
	sort.Strings(environment)
	return environment
}

func allowedEnvironmentKey(key string) bool {
	switch key {
	case "PATH",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
		"GASWORKS_CONFIG_DIR", "GASWORKS_STS_URL", "GASWORKS_OIDC_ISSUER", "GASWORKS_CLIENT_ID":
		return true
	default:
		return allowedPlatformEnvironmentKey(key)
	}
}
