package beads

import (
	"context"
	"net/http"
	"net/url"
	"sync"

	bdhttp "github.com/steveyegge/beads/backend/http"
)

// resolvedAmbientCredential is the beads http backend's own bearer ladder
// (BEADS_HTTP_TOKEN, BEADS_HTTP_TOKEN_COMMAND, the credentials file),
// resolved once for one remote open and then carried explicitly as that
// open's OpenOptions.Credential.
//
// The ladder reads the process environment when it resolves. Every Dolt open
// in this process projects its own environment while holding
// nativeDoltOpenEnvMu, and that projection withholds BEADS_CREDENTIALS_FILE,
// so a ladder resolved at request time could read the projection instead of
// the operator's value. Resolution therefore runs under the same mutex, and
// only resolution does: the resolved token is cached by the inner provider,
// so every request after it authorizes without touching the environment, and
// no network call ever happens while the mutex is held.
type resolvedAmbientCredential struct {
	inner *bdhttp.BearerProvider
	base  *url.URL

	mu       sync.Mutex
	resolved bool
}

// newResolvedAmbientCredential builds the ladder for base and resolves it now.
// A resolution failure (a failing token command) is not cached: the next
// Authorize retries it, under the mutex, and reports the error then.
func newResolvedAmbientCredential(ctx context.Context, base *url.URL) *resolvedAmbientCredential {
	c := &resolvedAmbientCredential{inner: bdhttp.NewBearerProvider(base), base: base}
	_ = c.ensureResolved(ctx)
	return c
}

// ensureResolved resolves the ladder under the env mutex if it has not been
// resolved yet. The throwaway request is never sent: it only gives the
// provider something to authorize, which is what resolves and caches it.
func (c *resolvedAmbientCredential) ensureResolved(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String(), nil)
	if err != nil {
		return err
	}
	nativeDoltOpenEnvMu.Lock()
	err = c.inner.Authorize(ctx, req)
	nativeDoltOpenEnvMu.Unlock()
	if err != nil {
		return err
	}
	c.resolved = true
	return nil
}

// Authorize implements the beads CredentialProvider contract.
func (c *resolvedAmbientCredential) Authorize(ctx context.Context, req *http.Request) error {
	if err := c.ensureResolved(ctx); err != nil {
		return err
	}
	return c.inner.Authorize(ctx, req)
}

// Refresh implements the beads CredentialProvider contract: after a 401 the
// ladder is re-walked (a rotated token), under the env mutex for the same
// reason the first resolution is.
func (c *resolvedAmbientCredential) Refresh(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nativeDoltOpenEnvMu.Lock()
	retry, err := c.inner.Refresh(ctx)
	nativeDoltOpenEnvMu.Unlock()
	if err == nil {
		c.resolved = true
	}
	return retry, err
}
