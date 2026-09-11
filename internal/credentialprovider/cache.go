package credentialprovider

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"strings"
	"sync"
	"time"
)

const credentialRefreshSkew = 30 * time.Second

// Cache owns an expiry-aware, process-local credential cache.
type Cache struct {
	mu      sync.Mutex
	entries map[credentialCacheKey]*credentialCacheEntry
}

type credentialCacheEntry struct {
	credential    Credential
	forceRequired bool
	flight        *credentialFlight
}

type credentialFlight struct {
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	waiters       int
	forceProvider bool
	retryForce    bool
	completed     bool
	credential    Credential
	err           error
}

type credentialCacheLookup struct {
	credential Credential
	flight     *credentialFlight
	start      bool
}

type credentialCacheKey struct {
	providerConfig [sha256.Size]byte
	org            string
	audience       string
	scopes         string
}

// NewCache constructs an empty in-memory credential cache.
func NewCache() *Cache {
	return &Cache{entries: make(map[credentialCacheKey]*credentialCacheEntry)}
}

// Mint returns a fresh cached credential or invokes provider to mint one.
func (c *Cache) Mint(ctx context.Context, provider *Provider, request Request) (Credential, error) {
	if ctx == nil {
		return Credential{}, errors.New("credential provider context is nil")
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if c == nil {
		return Credential{}, errors.New("credential cache is nil")
	}
	if provider == nil {
		return Credential{}, errors.New("credential provider is nil")
	}

	scopes, err := validateRequest(request)
	if err != nil {
		return Credential{}, err
	}
	environment := minimalEnvironment(provider.environ())
	key := newCredentialCacheKey(provider.argv, environment, request, scopes)
	for {
		lookup := c.acquire(key, request.ForceRefresh, provider.now())
		if lookup.credential.AccessToken != "" {
			if err := ctx.Err(); err != nil {
				return Credential{}, err
			}
			if !lookup.credential.ExpiresAt.After(provider.now()) {
				continue
			}
			return cloneCredential(lookup.credential), nil
		}
		if lookup.start {
			flightRequest := request
			flightRequest.ForceRefresh = lookup.flight.forceProvider
			flightRequest.RequiredScopes = scopes
			go c.runFlight(provider, key, lookup.flight, flightRequest, scopes, environment)
		}

		credential, retry, err := c.waitForFlight(ctx, lookup.flight)
		if err != nil {
			return Credential{}, err
		}
		if retry {
			continue
		}
		if !credential.ExpiresAt.After(provider.now()) {
			return Credential{}, errors.New("credential provider returned an expired credential")
		}
		return cloneCredential(credential), nil
	}
}

func (c *Cache) acquire(key credentialCacheKey, explicitForce bool, now time.Time) credentialCacheLookup {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[credentialCacheKey]*credentialCacheEntry)
	}
	entry := c.entries[key]
	if entry == nil {
		entry = &credentialCacheEntry{}
		c.entries[key] = entry
	}

	hadCredential := entry.credential.AccessToken != ""
	if hadCredential && !entry.credential.ExpiresAt.After(now) {
		entry.credential = Credential{}
	}
	if explicitForce {
		entry.credential = Credential{}
		entry.forceRequired = true
	} else if !entry.forceRequired && entry.credential.AccessToken != "" &&
		now.Add(credentialRefreshSkew).Before(entry.credential.ExpiresAt) {
		return credentialCacheLookup{credential: cloneCredential(entry.credential)}
	}

	if entry.flight != nil && entry.flight.waiters == 0 && !entry.flight.completed {
		entry.flight = nil
	}

	if entry.flight != nil {
		if entry.forceRequired && !entry.flight.forceProvider {
			entry.flight.retryForce = true
		}
		entry.flight.waiters++
		return credentialCacheLookup{flight: entry.flight}
	}

	flightCtx, cancel := context.WithCancel(context.Background())
	flight := &credentialFlight{
		ctx:           flightCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
		waiters:       1,
		forceProvider: explicitForce || entry.forceRequired || hadCredential,
	}
	entry.flight = flight
	return credentialCacheLookup{flight: flight, start: true}
}

func (c *Cache) runFlight(
	provider *Provider,
	key credentialCacheKey,
	flight *credentialFlight,
	request Request,
	scopes []string,
	environment []string,
) {
	credential, err := provider.mintValidated(flight.ctx, request, scopes, environment)
	c.completeFlight(key, flight, credential, err, provider.now())
}

func (c *Cache) completeFlight(
	key credentialCacheKey,
	flight *credentialFlight,
	credential Credential,
	err error,
	now time.Time,
) {
	if err == nil && !credential.ExpiresAt.After(now) {
		credential = Credential{}
		err = errors.New("credential provider returned an expired credential")
	}
	c.mu.Lock()
	flight.credential = cloneCredential(credential)
	flight.err = err
	flight.completed = true
	entry := c.entries[key]
	if entry != nil && entry.flight == flight {
		entry.flight = nil
		if err == nil {
			if flight.forceProvider {
				entry.forceRequired = false
			}
			if !entry.forceRequired && now.Add(credentialRefreshSkew).Before(credential.ExpiresAt) {
				entry.credential = cloneCredential(credential)
			} else if !entry.forceRequired {
				entry.credential = Credential{}
			}
		}
		if entry.credential.AccessToken == "" && !entry.forceRequired {
			delete(c.entries, key)
		}
	}
	flight.cancel()
	close(flight.done)
	c.mu.Unlock()
}

func (c *Cache) waitForFlight(ctx context.Context, flight *credentialFlight) (Credential, bool, error) {
	select {
	case <-ctx.Done():
		c.releaseWaiter(flight)
		return Credential{}, false, ctx.Err()
	case <-flight.done:
		ctxErr := ctx.Err()
		c.releaseWaiter(flight)
		if ctxErr != nil {
			return Credential{}, false, ctxErr
		}
		if flight.retryForce {
			return Credential{}, true, nil
		}
		if flight.err != nil {
			return Credential{}, false, flight.err
		}
		return cloneCredential(flight.credential), false, nil
	}
}

func (c *Cache) releaseWaiter(flight *credentialFlight) {
	c.mu.Lock()
	if flight.waiters > 0 {
		flight.waiters--
	}
	if flight.waiters == 0 && !flight.completed {
		flight.cancel()
	}
	c.mu.Unlock()
}

func newCredentialCacheKey(argv, environment []string, request Request, scopes []string) credentialCacheKey {
	digest := sha256.New()
	writeCacheKeyStrings(digest, argv)
	writeCacheKeyStrings(digest, environment)
	var providerConfig [sha256.Size]byte
	copy(providerConfig[:], digest.Sum(nil))
	return credentialCacheKey{
		providerConfig: providerConfig,
		org:            request.Org,
		audience:       request.Audience,
		scopes:         strings.Join(scopes, "\x00"),
	}
}

func writeCacheKeyStrings(destination hash.Hash, values []string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(values)))
	_, _ = destination.Write(length[:])
	for _, value := range values {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = destination.Write(length[:])
		_, _ = destination.Write([]byte(value))
	}
}

func cloneCredential(credential Credential) Credential {
	credential.Scopes = append([]string(nil), credential.Scopes...)
	return credential
}
