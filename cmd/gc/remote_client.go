package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/clientauth"
	"github.com/gastownhall/gascity/internal/clientgrant"
)

// remoteClientOptions builds the transport options (TLS + bearer) shared by
// every remote client for a resolved target. TLS options come from the target's
// context; the transport bearer comes from the context's credential_command (a
// clientauth.CredentialSource) or, for an ad-hoc --city-url/GC_CITY_URL target,
// from GC_CITY_URL_TOKEN. The resolver guarantees at most one credential
// technique per target. It does NOT wire the city-write grant — that is a
// write-only concern (see buildRemoteWriteClient).
func remoteClientOptions(target *remoteTarget) (api.RemoteOptions, error) {
	opts := api.RemoteOptions{}
	if ctx := target.Ctx; ctx != nil {
		opts.CAFile = ctx.CAFile
		opts.TLSServerName = ctx.TLSServerName
		opts.InsecureSkipVerify = ctx.InsecureSkipVerify
		if ctx.Timeout != "" {
			d, err := time.ParseDuration(ctx.Timeout)
			if err != nil {
				return api.RemoteOptions{}, fmt.Errorf("context %q: invalid timeout %q: %w", ctx.Name, ctx.Timeout, err)
			}
			opts.RESTTimeout = d
		}
		if ctx.CredentialCommand != "" {
			cs, err := clientauth.NewCredentialSource(ctx.CredentialCommand, target.BaseURL, target.CityName, false)
			if err != nil {
				return api.RemoteOptions{}, err
			}
			opts.Token = cs.Token
			// Force-mint on a 401 so a token rejected before its expiry (edge key
			// rotation / early revocation) recovers without a fresh gc invocation.
			opts.RefreshToken = cs.Refresh
		}
	}
	if target.Token != "" {
		tok := target.Token
		opts.Token = func() (string, error) { return tok, nil }
	}
	return opts, nil
}

// buildRemoteClient constructs a no-fallback API client for a resolved remote
// target, for READ commands (no city-write grant is attached).
func buildRemoteClient(target *remoteTarget) (*api.Client, error) {
	opts, err := remoteClientOptions(target)
	if err != nil {
		return nil, err
	}
	return api.NewRemoteCityScopedClient(target.BaseURL, target.CityName, opts)
}

// buildRemoteWriteClient is buildRemoteClient plus the city-write grant: for a
// context that configures a grant_command, it wires a clientgrant.GrantSource so
// every mutating request carries a fresh, request-bound X-GC-City-Write grant
// (gate G18). A context without a grant_command (or an ad-hoc --city-url target,
// Ctx==nil) attaches no grant — correct for a non-hardened direct city, which
// mutates on X-GC-Request alone; a hardened city then answers 401 and the
// operator learns they must configure grant_command.
func buildRemoteWriteClient(target *remoteTarget) (*api.Client, error) {
	opts, err := remoteClientOptions(target)
	if err != nil {
		return nil, err
	}
	if ctx := target.Ctx; ctx != nil && ctx.GrantCommand != "" {
		gs, err := clientgrant.NewGrantSource(ctx.GrantCommand)
		if err != nil {
			return nil, err
		}
		city := target.CityName
		opts.Grant = func(b api.GrantBinding) (string, error) {
			return gs.Mint(clientgrant.GrantInfo{
				Aud:            citywriteauth.AudienceCityWrite,
				City:           city,
				Method:         b.Method,
				Path:           b.Path,
				CanonicalQuery: b.CanonicalQuery,
				BodySHA256:     b.BodySHA256,
				ReqDigest:      b.ReqDigest,
			})
		}
	}
	return api.NewRemoteCityScopedClient(target.BaseURL, target.CityName, opts)
}

// resolveReadTarget resolves a no-argument READ command's target. For a REMOTE
// target (--context/--city-url/env/sticky default) it returns a no-fallback
// remote client with isRemote=true; the caller routes every read through it and,
// because a remote client is non-fallbackable (gate G1), a remote error is
// surfaced rather than fallen back. For a LOCAL target it returns isRemote=false
// and the resolved cityPath, and the caller uses its existing local client seam
// (preserving per-command test injection and the loopback fallback). A remote
// resolution or build failure is returned as err.
func resolveReadTarget() (remoteClient *api.Client, isRemote bool, cityPath string, err error) {
	ctx, err := resolveContextAllowRemote()
	if err != nil {
		return nil, false, "", err
	}
	if ctx.Remote != nil {
		c, berr := buildRemoteClient(ctx.Remote)
		if berr != nil {
			return nil, true, "", berr
		}
		return c, true, "", nil
	}
	return nil, false, ctx.CityPath, nil
}

// resolveWriteTarget resolves a MUTATING command's target. It is the write-side
// sibling of resolveReadTarget: identical context resolution, but a remote
// client is built with buildRemoteWriteClient so it carries the city-write grant
// a hardened city requires (gate G18). Because a remote client is
// non-fallbackable (gate G1), a remote mutation error surfaces rather than
// silently falling back to a local store. For a LOCAL target it returns
// isRemote=false and a nil client; the caller re-resolves the city through its
// existing local seam (unlike the read side, no local cityPath is threaded — the
// write callers already re-run resolveCity on the local branch).
//
// target is the resolved *remoteTarget when isRemote is true (nil for a local
// target), so a caller can echo the target and build a resume recipe naming the
// context/URL without re-running the resolver.
func resolveWriteTarget() (remoteClient *api.Client, isRemote bool, target *remoteTarget, err error) {
	ctx, err := resolveContextAllowRemote()
	if err != nil {
		return nil, false, nil, err
	}
	if ctx.Remote != nil {
		c, berr := buildRemoteWriteClient(ctx.Remote)
		if berr != nil {
			return nil, true, ctx.Remote, berr
		}
		return c, true, ctx.Remote, nil
	}
	return nil, false, nil, nil
}
