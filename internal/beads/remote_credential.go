package beads

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	bdhttp "github.com/steveyegge/beads/backend/http"
)

// resolvedAmbientCredential is the beads http backend's own bearer ladder
// (BEADS_HTTP_TOKEN, BEADS_HTTP_TOKEN_COMMAND, the credentials file),
// resolved for one remote open and then carried explicitly as that open's
// bdhttp.Options.Credential (the single-city posture: a scope with no
// per-city / per-rig credential configured).
//
// The ladder reads the process environment when it walks, and a Dolt open
// may be projecting its own environment at that moment: under
// nativeDoltOpenEnvMu, a hermetic open withholds every BEADS_ variable and
// every open projects BEADS_CREDENTIALS_FILE. The walk does not hold that
// mutex, because its token-command rung is a subprocess (bd allows it 30s)
// and the mutex fences every native open and bd-child env build in the
// process. Each projection bumps nativeDoltOpenEnvEpoch instead, and a walk
// the epoch moved under is walked again, up to ambientCredentialWalks times.
// The last walk stands: every rung is scoped to this server, and a token the
// server refuses is walked again on its 401 (Refresh).
//
// The inner provider caches what a walk resolved, so requests between walks
// authorize without reading the environment. A failed walk is held for
// ambientCredentialFailureBackoff: until then every request fails with it,
// never with the previous token, and a broken token command runs once per
// backoff rather than once per request.
type resolvedAmbientCredential struct {
	inner bdhttp.CredentialProvider

	mu       sync.Mutex
	resolved bool
	failure  error
	failedAt time.Time
}

const (
	// ambientCredentialWalks bounds the walks one resolution makes while env
	// projections keep overlapping it.
	ambientCredentialWalks = 3
	// ambientCredentialFailureBackoff is how long a failed walk answers for
	// the ladder before it is walked again.
	ambientCredentialFailureBackoff = 5 * time.Second
)

// ambientCredentialNow is the failure backoff's clock (tests).
var ambientCredentialNow = time.Now

// newResolvedAmbientCredential builds the ladder for base and walks it now. A
// failure is held for the backoff and reported by the next Authorize.
func newResolvedAmbientCredential(ctx context.Context, base *url.URL) *resolvedAmbientCredential {
	c := &resolvedAmbientCredential{inner: bdhttp.NewBearerProvider(base)}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.walkLocked(ctx)
	return c
}

// walkLocked walks the ladder (the inner provider's Refresh) with c.mu held
// and reports whether a walk changed the token.
func (c *resolvedAmbientCredential) walkLocked(ctx context.Context) (bool, error) {
	if c.failure != nil && ambientCredentialNow().Sub(c.failedAt) < ambientCredentialFailureBackoff {
		return false, c.failure
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var retry bool
	var err error
	for walk := 0; walk < ambientCredentialWalks; walk++ {
		epoch := nativeDoltOpenEnvEpochNow()
		var changed bool
		changed, err = c.inner.Refresh(ctx)
		retry = retry || changed
		if nativeDoltOpenEnvEpochNow() == epoch {
			break
		}
	}
	if err != nil {
		c.resolved, c.failure, c.failedAt = false, err, ambientCredentialNow()
		return false, err
	}
	c.resolved, c.failure = true, nil
	return retry, nil
}

// Authorize implements the beads CredentialProvider contract.
func (c *resolvedAmbientCredential) Authorize(ctx context.Context, req *http.Request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved {
		if _, err := c.walkLocked(ctx); err != nil {
			return err
		}
	}
	return c.inner.Authorize(ctx, req)
}

// Refresh implements the beads CredentialProvider contract: after a 401 the
// ladder is walked again (a rotated token).
func (c *resolvedAmbientCredential) Refresh(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.walkLocked(ctx)
}
