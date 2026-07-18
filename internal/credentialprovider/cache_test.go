package credentialprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type cacheTestClock struct {
	mu    sync.RWMutex
	value time.Time
}

func (clock *cacheTestClock) Now() time.Time {
	clock.mu.RLock()
	defer clock.mu.RUnlock()
	return clock.value
}

func (clock *cacheTestClock) Set(value time.Time) {
	clock.mu.Lock()
	clock.value = value
	clock.mu.Unlock()
}

type cacheTestRunner struct {
	mu           sync.Mutex
	calls        []wireRequest
	environments [][]string
	active       int
	maxActive    int
	respond      func(context.Context, int, wireRequest) (commandOutput, error)
}

func (runner *cacheTestRunner) run(ctx context.Context, _ []string, stdin []byte, environment []string) (commandOutput, error) {
	var request wireRequest
	if err := json.Unmarshal(stdin, &request); err != nil {
		return commandOutput{}, fmt.Errorf("decode test request: %w", err)
	}
	runner.mu.Lock()
	runner.calls = append(runner.calls, request)
	runner.environments = append(runner.environments, slices.Clone(environment))
	runner.active++
	runner.maxActive = max(runner.maxActive, runner.active)
	call := len(runner.calls)
	runner.mu.Unlock()
	defer func() {
		runner.mu.Lock()
		runner.active--
		runner.mu.Unlock()
	}()
	return runner.respond(ctx, call, request)
}

func (runner *cacheTestRunner) Requests() []wireRequest {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return slices.Clone(runner.calls)
}

func (runner *cacheTestRunner) Environments() [][]string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	environments := make([][]string, len(runner.environments))
	for index, environment := range runner.environments {
		environments[index] = slices.Clone(environment)
	}
	return environments
}

func (runner *cacheTestRunner) MaxConcurrentCalls() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.maxActive
}

func newCacheTestProvider(t *testing.T, argv []string, clock *cacheTestClock, runner *cacheTestRunner) *Provider {
	t.Helper()
	provider, err := New(argv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider.run = runner.run
	provider.now = clock.Now
	provider.environ = func() []string { return []string{"PATH=/usr/bin"} }
	return provider
}

func cacheCredentialOutput(t *testing.T, request wireRequest, token string, expiresAt time.Time) commandOutput {
	t.Helper()
	response := struct {
		Version             string   `json:"version"`
		Kind                string   `json:"kind"`
		AccessToken         string   `json:"access_token"`
		AuthorizationScheme string   `json:"authorization_scheme"`
		ExpiresAt           string   `json:"expires_at"`
		Audience            string   `json:"audience"`
		Scopes              []string `json:"scopes"`
	}{
		Version:             ProtocolVersion,
		Kind:                "Credential",
		AccessToken:         token,
		AuthorizationScheme: "Bearer",
		ExpiresAt:           expiresAt.UTC().Format(time.RFC3339Nano),
		Audience:            request.Audience,
		Scopes:              append([]string(nil), request.RequiredScopes...),
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal credential response: %v", err)
	}
	return commandOutput{stdout: encoded}
}

func TestCacheMintCachesLiveCredentialByCanonicalRequest(t *testing.T) {
	provider, runner := newRecordingProvider(t, validCredentialOutput(), nil)
	cache := NewCache()
	request := validCredentialRequest()
	wantRequestScopes := append([]string(nil), request.RequiredScopes...)

	first, err := cache.Mint(context.Background(), provider, request)
	if err != nil {
		t.Fatalf("first Mint: %v", err)
	}
	if !slices.Equal(request.RequiredScopes, wantRequestScopes) {
		t.Fatalf("request scopes mutated: got %q, want %q", request.RequiredScopes, wantRequestScopes)
	}
	first.Scopes[0] = "caller-mutation"

	request.RequiredScopes = []string{"manifold:pool:acme", "manifold:proxy"}
	second, err := cache.Mint(context.Background(), provider, request)
	if err != nil {
		t.Fatalf("second Mint: %v", err)
	}

	if runner.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", runner.calls)
	}
	if !slices.Equal(second.Scopes, []string{"manifold:pool:acme", "manifold:proxy"}) {
		t.Fatalf("cached scopes = %q, want a defensive canonical copy", second.Scopes)
	}
}

func TestCacheMintRefreshesAtSkewBoundary(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		expiresAt := base.Add(2 * time.Minute)
		if call > 1 {
			expiresAt = base.Add(10 * time.Minute)
		}
		return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), expiresAt), nil
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()

	first, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || first.AccessToken != "token-1" {
		t.Fatalf("first Mint = %+v, %v", first, err)
	}
	clock.Set(base.Add(2*time.Minute - credentialRefreshSkew - time.Nanosecond))
	beforeBoundary, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || beforeBoundary.AccessToken != "token-1" {
		t.Fatalf("Mint before skew boundary = %+v, %v", beforeBoundary, err)
	}
	clock.Set(base.Add(2*time.Minute - credentialRefreshSkew))
	atBoundary, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || atBoundary.AccessToken != "token-2" {
		t.Fatalf("Mint at skew boundary = %+v, %v", atBoundary, err)
	}

	requests := runner.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(requests))
	}
	if requests[0].ForceRefresh || !requests[1].ForceRefresh {
		t.Fatalf("force_refresh sequence = [%v %v], want [false true]", requests[0].ForceRefresh, requests[1].ForceRefresh)
	}
}

func TestCacheMintNeverServesAtOrAfterHardExpiry(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		if call == 1 {
			return cacheCredentialOutput(t, request, "token-1", base.Add(time.Minute)), nil
		}
		return commandOutput{}, errors.New("refresh failed")
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()

	if _, err := cache.Mint(context.Background(), provider, validCredentialRequest()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	clock.Set(base.Add(time.Minute))
	credential, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err == nil || credential.AccessToken != "" {
		t.Fatalf("Mint at hard expiry returned credential %+v", credential)
	}
	requests := runner.Requests()
	if len(requests) != 2 || !requests[1].ForceRefresh {
		t.Fatalf("requests = %+v, want forced renewal at hard expiry", requests)
	}
}

func TestCacheMintRechecksHardExpiryBeforeCachedReturn(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		expiresAt := base.Add(time.Hour)
		if call > 1 {
			expiresAt = base.Add(3 * time.Hour)
		}
		return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), expiresAt), nil
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	var nowMu sync.Mutex
	nowCalls := 0
	provider.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		nowCalls++
		if nowCalls >= 6 {
			return base.Add(2 * time.Hour)
		}
		return base
	}
	cache := NewCache()

	if _, err := cache.Mint(context.Background(), provider, validCredentialRequest()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	credential, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || credential.AccessToken != "token-2" {
		t.Fatalf("Mint after cached-hit clock advance = %+v, %v", credential, err)
	}
	requests := runner.Requests()
	if len(requests) != 2 || requests[0].ForceRefresh || !requests[1].ForceRefresh {
		t.Fatalf("force_refresh sequence = %+v, want [false true]", requests)
	}
}

func TestCacheMintDoesNotRetainCredentialInsideRefreshSkew(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), base.Add(10*time.Second)), nil
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()

	for call := 1; call <= 2; call++ {
		credential, err := cache.Mint(context.Background(), provider, validCredentialRequest())
		if err != nil || credential.AccessToken != fmt.Sprintf("token-%d", call) {
			t.Fatalf("Mint %d = %+v, %v", call, credential, err)
		}
	}
	if got := len(runner.Requests()); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
}

func TestCacheMintForceFailureInvalidatesRejectedCredential(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		if call == 2 {
			return commandOutput{}, errors.New("forced refresh failed")
		}
		return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), base.Add(time.Hour)), nil
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()

	first, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || first.AccessToken != "token-1" {
		t.Fatalf("prime cache = %+v, %v", first, err)
	}
	forced := validCredentialRequest()
	forced.ForceRefresh = true
	if credential, err := cache.Mint(context.Background(), provider, forced); err == nil || credential.AccessToken != "" {
		t.Fatalf("forced Mint returned rejected credential %+v", credential)
	}
	afterFailure, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || afterFailure.AccessToken != "token-3" {
		t.Fatalf("Mint after force failure = %+v, %v", afterFailure, err)
	}
	requests := runner.Requests()
	if len(requests) != 3 || !requests[1].ForceRefresh || !requests[2].ForceRefresh {
		t.Fatalf("force_refresh sequence = %+v, want [false true true]", requests)
	}
}

func TestCacheMintExpiryDuringForcedCompletionKeepsForceRequired(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		expiresAt := base.Add(3 * time.Hour)
		if call == 2 {
			expiresAt = base.Add(time.Hour)
		}
		return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), expiresAt), nil
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	var nowMu sync.Mutex
	nowCalls := 0
	provider.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		nowCalls++
		if nowCalls >= 7 {
			return base.Add(2 * time.Hour)
		}
		return base
	}
	cache := NewCache()

	if _, err := cache.Mint(context.Background(), provider, validCredentialRequest()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	forced := validCredentialRequest()
	forced.ForceRefresh = true
	credential, err := cache.Mint(context.Background(), provider, forced)
	if err == nil || credential.AccessToken != "" {
		t.Fatalf("forced Mint after completion-time expiry = %+v, %v", credential, err)
	}
	afterExpiry, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || afterExpiry.AccessToken != "token-3" {
		t.Fatalf("Mint after forced expiry = %+v, %v", afterExpiry, err)
	}
	requests := runner.Requests()
	if len(requests) != 3 || requests[0].ForceRefresh || !requests[1].ForceRefresh || !requests[2].ForceRefresh {
		t.Fatalf("force_refresh sequence = %+v, want [false true true]", requests)
	}
}

func TestCacheMintKeySeparatesEveryCredentialDimension(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), base.Add(time.Hour)), nil
	}
	providerA := newCacheTestProvider(t, []string{"gasworks", "ab", "c"}, clock, runner)
	providerB := newCacheTestProvider(t, []string{"gasworks", "a", "bc"}, clock, runner)
	cache := NewCache()
	baseRequest := Request{Audience: "manifold", Org: "org-a", RequiredScopes: []string{"a", "bc"}}

	requests := []struct {
		provider *Provider
		request  Request
	}{
		{providerA, baseRequest},
		{providerA, Request{Audience: "manifold", Org: "org-a", RequiredScopes: []string{"bc", "a"}}},
		{providerB, baseRequest},
		{providerA, Request{Audience: "manifold", Org: "org-b", RequiredScopes: []string{"a", "bc"}}},
		{providerA, Request{Audience: "crucible", Org: "org-a", RequiredScopes: []string{"a", "bc"}}},
		{providerA, Request{Audience: "manifold", Org: "org-a", RequiredScopes: []string{"ab", "c"}}},
	}
	wantTokens := []string{"token-1", "token-1", "token-2", "token-3", "token-4", "token-5"}
	for index, item := range requests {
		credential, err := cache.Mint(context.Background(), item.provider, item.request)
		if err != nil || credential.AccessToken != wantTokens[index] {
			t.Fatalf("Mint %d = %+v, %v; want %q", index, credential, err, wantTokens[index])
		}
	}
	if got := len(runner.Requests()); got != 5 {
		t.Fatalf("provider calls = %d, want 5", got)
	}
}

func TestCacheMintProviderConfigKeyPreservesArgvEnvironmentBoundary(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), base.Add(time.Hour)), nil
	}
	providerA := newCacheTestProvider(t, []string{"gasworks", "a"}, clock, runner)
	providerA.environ = func() []string { return []string{"PATH=b"} }
	providerB := newCacheTestProvider(t, []string{"gasworks", "a", "PATH=b"}, clock, runner)
	providerB.environ = func() []string { return nil }
	cache := NewCache()

	first, err := cache.Mint(context.Background(), providerA, validCredentialRequest())
	if err != nil || first.AccessToken != "token-1" {
		t.Fatalf("first config Mint = %+v, %v", first, err)
	}
	second, err := cache.Mint(context.Background(), providerB, validCredentialRequest())
	if err != nil || second.AccessToken != "token-2" {
		t.Fatalf("second config Mint = %+v, %v", second, err)
	}
}

func TestCacheMintProviderConfigKeyUsesOneCanonicalEnvironmentSnapshot(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	runner := &cacheTestRunner{}
	runner.respond = func(_ context.Context, call int, request wireRequest) (commandOutput, error) {
		return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), base.Add(time.Hour)), nil
	}
	argv := []string{"gasworks", "credential-provider"}
	first := newCacheTestProvider(t, argv, clock, runner)
	firstEnvironmentCalls := 0
	first.environ = func() []string {
		firstEnvironmentCalls++
		if firstEnvironmentCalls > 1 {
			return []string{"PATH=/usr/bin", "HTTPS_PROXY=http://proxy-b"}
		}
		return []string{"PATH=/usr/bin", "HTTPS_PROXY=http://proxy-a", "UNRELATED=drop"}
	}
	same := newCacheTestProvider(t, argv, clock, runner)
	sameEnvironmentCalls := 0
	same.environ = func() []string {
		sameEnvironmentCalls++
		return []string{"UNRELATED=different", "HTTPS_PROXY=http://proxy-a", "PATH=/usr/bin"}
	}
	changed := newCacheTestProvider(t, argv, clock, runner)
	changedEnvironmentCalls := 0
	changed.environ = func() []string {
		changedEnvironmentCalls++
		return []string{"PATH=/usr/bin", "HTTPS_PROXY=http://proxy-b"}
	}
	cache := NewCache()

	providers := []*Provider{first, same, changed}
	wantTokens := []string{"token-1", "token-1", "token-2"}
	for index, provider := range providers {
		credential, err := cache.Mint(context.Background(), provider, validCredentialRequest())
		if err != nil || credential.AccessToken != wantTokens[index] {
			t.Fatalf("Mint %d = %+v, %v; want %q", index, credential, err, wantTokens[index])
		}
	}
	if firstEnvironmentCalls != 1 || sameEnvironmentCalls != 1 || changedEnvironmentCalls != 1 {
		t.Fatalf("environment calls = [%d %d %d], want [1 1 1]", firstEnvironmentCalls, sameEnvironmentCalls, changedEnvironmentCalls)
	}
	wantEnvironments := [][]string{
		{"HTTPS_PROXY=http://proxy-a", "PATH=/usr/bin"},
		{"HTTPS_PROXY=http://proxy-b", "PATH=/usr/bin"},
	}
	if got := runner.Environments(); !slices.EqualFunc(got, wantEnvironments, slices.Equal[[]string]) {
		t.Fatalf("provider environments = %q, want %q", got, wantEnvironments)
	}
}

type cacheMintResult struct {
	credential Credential
	err        error
}

func waitForCacheFlightWaiters(t *testing.T, cache *Cache, provider *Provider, request Request, want int) {
	t.Helper()
	scopes, err := validateRequest(request)
	if err != nil {
		t.Fatalf("validate request: %v", err)
	}
	key := newCredentialCacheKey(provider.argv, minimalEnvironment(provider.environ()), request, scopes)
	deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer deadline.Stop()
	for {
		cache.mu.Lock()
		entry := cache.entries[key]
		got := 0
		if entry != nil && entry.flight != nil {
			got = entry.flight.waiters
		}
		cache.mu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("flight waiters = %d, want at least %d", got, want)
		default:
			runtime.Gosched()
		}
	}
}

func awaitCacheValue[T any](t *testing.T, values <-chan T, description string) T {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	select {
	case value := <-values:
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}

func startCacheMint(
	ctx context.Context,
	cache *Cache,
	provider *Provider,
	request Request,
	results chan<- cacheMintResult,
) {
	go func() {
		credential, err := cache.Mint(ctx, provider, request)
		results <- cacheMintResult{credential: credential, err: err}
	}()
}

func cacheTestGateRelease(t *testing.T, gate chan struct{}) func() {
	t.Helper()
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

func TestCacheMintColdMissSingleflight(t *testing.T) {
	const callers = 16
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	started := make(chan int, 1)
	release := make(chan struct{})
	releaseProvider := cacheTestGateRelease(t, release)
	runner := &cacheTestRunner{}
	runner.respond = func(ctx context.Context, call int, request wireRequest) (commandOutput, error) {
		started <- call
		select {
		case <-release:
			return cacheCredentialOutput(t, request, "shared-token", base.Add(time.Hour)), nil
		case <-ctx.Done():
			return commandOutput{}, ctx.Err()
		}
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()
	results := make(chan cacheMintResult, callers)

	for range callers {
		startCacheMint(context.Background(), cache, provider, validCredentialRequest(), results)
	}
	if call := awaitCacheValue(t, started, "credential provider start"); call != 1 {
		t.Fatalf("first provider call = %d, want 1", call)
	}
	waitForCacheFlightWaiters(t, cache, provider, validCredentialRequest(), callers)
	releaseProvider()

	for range callers {
		result := awaitCacheValue(t, results, "cached credential result")
		if result.err != nil || result.credential.AccessToken != "shared-token" {
			t.Fatalf("Mint = %+v, %v", result.credential, result.err)
		}
	}
	if got := len(runner.Requests()); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestCacheMintDistinctKeysRunConcurrently(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	manifoldStarted := make(chan int, 1)
	releaseManifold := make(chan struct{})
	releaseBlockedManifold := cacheTestGateRelease(t, releaseManifold)
	runner := &cacheTestRunner{}
	runner.respond = func(ctx context.Context, call int, request wireRequest) (commandOutput, error) {
		if request.Audience == "manifold" {
			manifoldStarted <- call
			select {
			case <-releaseManifold:
			case <-ctx.Done():
				return commandOutput{}, ctx.Err()
			}
		}
		return cacheCredentialOutput(t, request, request.Audience+"-token", base.Add(time.Hour)), nil
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()
	manifoldResults := make(chan cacheMintResult, 1)
	startCacheMint(context.Background(), cache, provider, validCredentialRequest(), manifoldResults)
	awaitCacheValue(t, manifoldStarted, "manifold provider start")
	crucibleRequest := Request{Audience: "crucible", Org: "org-acme", RequiredScopes: []string{"crucible:write"}}
	crucibleResults := make(chan cacheMintResult, 1)
	startCacheMint(context.Background(), cache, provider, crucibleRequest, crucibleResults)
	crucible := awaitCacheValue(t, crucibleResults, "crucible credential result")
	if crucible.err != nil || crucible.credential.AccessToken != "crucible-token" {
		t.Fatalf("crucible Mint = %+v, %v", crucible.credential, crucible.err)
	}
	releaseBlockedManifold()
	manifold := awaitCacheValue(t, manifoldResults, "manifold credential result")
	if manifold.err != nil || manifold.credential.AccessToken != "manifold-token" {
		t.Fatalf("manifold Mint = %+v, %v", manifold.credential, manifold.err)
	}
}

func TestCacheMintCanceledLeaderDoesNotPoisonLiveJoiner(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	started := make(chan int, 1)
	release := make(chan struct{})
	releaseProvider := cacheTestGateRelease(t, release)
	runner := &cacheTestRunner{}
	runner.respond = func(ctx context.Context, call int, request wireRequest) (commandOutput, error) {
		started <- call
		select {
		case <-release:
			return cacheCredentialOutput(t, request, "shared-token", base.Add(time.Hour)), nil
		case <-ctx.Done():
			return commandOutput{}, ctx.Err()
		}
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderResults := make(chan cacheMintResult, 1)
	startCacheMint(leaderCtx, cache, provider, validCredentialRequest(), leaderResults)
	awaitCacheValue(t, started, "credential provider start")

	joinerResults := make(chan cacheMintResult, 1)
	startCacheMint(context.Background(), cache, provider, validCredentialRequest(), joinerResults)
	waitForCacheFlightWaiters(t, cache, provider, validCredentialRequest(), 2)
	cancelLeader()
	leader := awaitCacheValue(t, leaderResults, "canceled leader result")
	if !errors.Is(leader.err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", leader.err)
	}
	releaseProvider()
	joiner := awaitCacheValue(t, joinerResults, "live joiner result")
	if joiner.err != nil || joiner.credential.AccessToken != "shared-token" {
		t.Fatalf("joiner Mint = %+v, %v", joiner.credential, joiner.err)
	}
	if got := len(runner.Requests()); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestCacheMintForceWaitsForWeakFlightThenCoalesces(t *testing.T) {
	const forcedCallers = 8
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	started := make(chan int, 2)
	releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
	releaseWeak := cacheTestGateRelease(t, releases[0])
	releaseForced := cacheTestGateRelease(t, releases[1])
	runner := &cacheTestRunner{}
	runner.respond = func(ctx context.Context, call int, request wireRequest) (commandOutput, error) {
		started <- call
		select {
		case <-releases[call-1]:
			return cacheCredentialOutput(t, request, fmt.Sprintf("token-%d", call), base.Add(time.Hour)), nil
		case <-ctx.Done():
			return commandOutput{}, ctx.Err()
		}
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()
	results := make(chan cacheMintResult, forcedCallers)
	startCacheMint(context.Background(), cache, provider, validCredentialRequest(), results)
	awaitCacheValue(t, started, "weak provider start")

	forced := validCredentialRequest()
	forced.ForceRefresh = true
	canceledCtx, cancelForced := context.WithCancel(context.Background())
	canceledResults := make(chan cacheMintResult, 1)
	startCacheMint(canceledCtx, cache, provider, forced, canceledResults)
	for range forcedCallers - 1 {
		startCacheMint(context.Background(), cache, provider, forced, results)
	}
	waitForCacheFlightWaiters(t, cache, provider, validCredentialRequest(), forcedCallers+1)
	cancelForced()
	canceled := awaitCacheValue(t, canceledResults, "canceled forced credential result")
	if !errors.Is(canceled.err, context.Canceled) {
		t.Fatalf("canceled forced caller error = %v, want context.Canceled", canceled.err)
	}
	releaseWeak()
	if call := awaitCacheValue(t, started, "forced provider start"); call != 2 {
		t.Fatalf("forced provider call = %d, want 2", call)
	}
	waitForCacheFlightWaiters(t, cache, provider, validCredentialRequest(), forcedCallers)
	releaseForced()

	for range forcedCallers {
		result := awaitCacheValue(t, results, "forced credential result")
		if result.err != nil || result.credential.AccessToken != "token-2" {
			t.Fatalf("Mint = %+v, %v; want token-2", result.credential, result.err)
		}
	}
	cached, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || cached.AccessToken != "token-2" {
		t.Fatalf("cached forced credential = %+v, %v", cached, err)
	}
	requests := runner.Requests()
	if len(requests) != 2 || requests[0].ForceRefresh || !requests[1].ForceRefresh {
		t.Fatalf("requests = %+v, want one weak then one forced flight", requests)
	}
	if got := runner.MaxConcurrentCalls(); got != 1 {
		t.Fatalf("concurrent same-key provider calls = %d, want 1", got)
	}
}

func TestCacheMintFailedFlightIsSharedButNotCached(t *testing.T) {
	const callers = 8
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	started := make(chan int, 1)
	release := make(chan struct{})
	releaseProvider := cacheTestGateRelease(t, release)
	runner := &cacheTestRunner{}
	runner.respond = func(ctx context.Context, call int, request wireRequest) (commandOutput, error) {
		if call == 1 {
			started <- call
			select {
			case <-release:
				return commandOutput{}, errors.New("temporary failure")
			case <-ctx.Done():
				return commandOutput{}, ctx.Err()
			}
		}
		return cacheCredentialOutput(t, request, "retry-token", base.Add(time.Hour)), nil
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()
	results := make(chan cacheMintResult, callers)
	for range callers {
		startCacheMint(context.Background(), cache, provider, validCredentialRequest(), results)
	}
	awaitCacheValue(t, started, "failed provider start")
	waitForCacheFlightWaiters(t, cache, provider, validCredentialRequest(), callers)
	releaseProvider()
	for range callers {
		if result := awaitCacheValue(t, results, "failed credential result"); result.err == nil || result.credential.AccessToken != "" {
			t.Fatalf("failed flight returned credential %+v", result.credential)
		}
	}

	retry, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || retry.AccessToken != "retry-token" {
		t.Fatalf("retry Mint = %+v, %v", retry, err)
	}
	if got := len(runner.Requests()); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
}

func TestCacheMintCanceledLastWaiterDoesNotTrapLaterCaller(t *testing.T) {
	base := credentialTestNow
	clock := &cacheTestClock{value: base}
	started := make(chan int, 2)
	forcedCanceled := make(chan struct{})
	releaseForced := make(chan struct{})
	releaseBlockedForced := cacheTestGateRelease(t, releaseForced)
	runner := &cacheTestRunner{}
	runner.respond = func(ctx context.Context, call int, request wireRequest) (commandOutput, error) {
		if call == 1 {
			return cacheCredentialOutput(t, request, "cached-token", base.Add(time.Hour)), nil
		}
		started <- call
		if call == 2 {
			<-ctx.Done()
			close(forcedCanceled)
			<-releaseForced
			return commandOutput{}, ctx.Err()
		}
		return cacheCredentialOutput(t, request, "replacement-token", base.Add(time.Hour)), nil
	}
	provider := newCacheTestProvider(t, []string{"gasworks", "credential-provider"}, clock, runner)
	cache := NewCache()
	prime, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || prime.AccessToken != "cached-token" {
		t.Fatalf("prime cache = %+v, %v", prime, err)
	}

	forced := validCredentialRequest()
	forced.ForceRefresh = true
	ctx, cancel := context.WithCancel(context.Background())
	forcedResults := make(chan cacheMintResult, 1)
	startCacheMint(ctx, cache, provider, forced, forcedResults)
	if call := awaitCacheValue(t, started, "forced provider start"); call != 2 {
		t.Fatalf("forced provider call = %d, want 2", call)
	}
	scopes, err := validateRequest(forced)
	if err != nil {
		t.Fatalf("validate forced request: %v", err)
	}
	key := newCredentialCacheKey(provider.argv, minimalEnvironment(provider.environ()), forced, scopes)
	cache.mu.Lock()
	abandoned := cache.entries[key].flight
	cache.mu.Unlock()
	cancel()
	canceled := awaitCacheValue(t, forcedResults, "canceled forced credential result")
	if !errors.Is(canceled.err, context.Canceled) {
		t.Fatalf("forced caller error = %v, want context.Canceled", canceled.err)
	}
	awaitCacheValue(t, forcedCanceled, "canceled forced provider cleanup")

	replacementResults := make(chan cacheMintResult, 1)
	startCacheMint(context.Background(), cache, provider, validCredentialRequest(), replacementResults)
	if call := awaitCacheValue(t, started, "replacement provider start"); call != 3 {
		t.Fatalf("replacement provider call = %d, want 3", call)
	}
	replacement := awaitCacheValue(t, replacementResults, "replacement credential result")
	if replacement.err != nil || replacement.credential.AccessToken != "replacement-token" {
		t.Fatalf("replacement Mint = %+v, %v", replacement.credential, replacement.err)
	}
	releaseBlockedForced()
	awaitCacheValue(t, abandoned.done, "abandoned forced flight completion")
	cached, err := cache.Mint(context.Background(), provider, validCredentialRequest())
	if err != nil || cached.AccessToken != "replacement-token" {
		t.Fatalf("cached replacement credential = %+v, %v", cached, err)
	}

	requests := runner.Requests()
	if len(requests) != 3 || requests[0].ForceRefresh || !requests[1].ForceRefresh || !requests[2].ForceRefresh {
		t.Fatalf("force_refresh sequence = %+v, want [false true true]", requests)
	}
}
