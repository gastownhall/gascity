package main

import (
	"context"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Per-city / per-rig credentials for a remote beads backend (DESIGN C3, G7):
// city.toml [beads] credential and rigs.beads_credential. gc reads where the
// bearer lives from config; internal/beads resolves it once per scope at open
// and uses it as that scope's ONLY credential, for the native store, the
// wire_compat handshake and the bd subprocess alike. A scope with none keeps
// bd's ambient ladder (single city per server and process).

// cityHTTPCredentials is the credential configuration one city load yields.
type cityHTTPCredentials struct {
	city   cityHTTPCredential
	byRoot map[string]cityHTTPCredential // normalized rig scope root
}

type cityHTTPCredential struct {
	setting config.BeadsCredentialSetting
	ok      bool
	err     error
}

func cityHTTPCredentialsFromConfig(cityPath string, cfg *config.City) cityHTTPCredentials {
	out := cityHTTPCredentials{byRoot: map[string]cityHTTPCredential{}}
	if cfg == nil {
		return out
	}
	setting, ok, err := config.BeadsCredentialFor(cfg, nil)
	out.city = cityHTTPCredential{setting: setting, ok: ok, err: err}
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		if rig.Path == "" {
			continue
		}
		setting, ok, err := config.BeadsCredentialFor(cfg, rig)
		root := normalizePathForCompare(resolveStoreScopeRoot(cityPath, rig.Path))
		out.byRoot[root] = cityHTTPCredential{setting: setting, ok: ok, err: err}
	}
	return out
}

// cityRemoteCredentialLookup is the beads.RemoteCredentialLookup gc installs:
// the scope's rig beads_credential, else the city's [beads] credential, read
// through the stat-validated city config cache (an edit is seen on the next
// open without a restart).
func cityRemoteCredentialLookup(cityPath, scopeRoot string) (beads.RemoteCredentialConfig, bool, error) {
	if cityPath == "" {
		return beads.RemoteCredentialConfig{}, false, nil
	}
	entry, err := cityCredentialProbe(cityPath)
	if err != nil {
		return beads.RemoteCredentialConfig{}, false, err
	}
	if entry == nil {
		return beads.RemoteCredentialConfig{}, false, nil
	}
	chosen := entry.http.city
	if !samePath(scopeRoot, cityPath) {
		if rig, found := entry.http.byRoot[normalizePathForCompare(resolveStoreScopeRoot(cityPath, scopeRoot))]; found {
			chosen = rig
		}
	}
	if chosen.err != nil {
		return beads.RemoteCredentialConfig{}, false, chosen.err
	}
	if !chosen.ok {
		return beads.RemoteCredentialConfig{}, false, nil
	}
	return beads.RemoteCredentialConfig{
		Source:        chosen.setting.Source,
		AllowInsecure: chosen.setting.AllowInsecure,
		Scope:         chosen.setting.Scope,
		FromCity:      chosen.setting.FromCity,
		Dir:           cityPath,
	}, true, nil
}

// bdScopeCredentialEnv projects one bd runner's per-scope remote credential
// into each child's env overrides (beads.RemoteCredentialSubprocessEnv): the
// token travels in that child's environment only, never on argv, and never to
// a child of another scope. One holder per scope directory the runner serves,
// so the token is resolved once per scope, not once per command.
type bdScopeCredentialEnv struct {
	cityPath string

	mu      sync.Mutex
	holders map[string]*beads.RemoteCredentialSubprocessEnv
}

func newBdScopeCredentialEnv(cityPath string) *bdScopeCredentialEnv {
	return &bdScopeCredentialEnv{cityPath: cityPath, holders: map[string]*beads.RemoteCredentialSubprocessEnv{}}
}

// apply writes scopeRoot's credential env into env when the scope has a
// per-scope credential; otherwise env is left as built (bd's ambient ladder).
func (b *bdScopeCredentialEnv) apply(env map[string]string, scopeRoot string) error {
	if b == nil || env == nil {
		return nil
	}
	b.mu.Lock()
	holder, ok := b.holders[scopeRoot]
	if !ok {
		holder = beads.NewRemoteCredentialSubprocessEnv(b.cityPath, scopeRoot)
		b.holders[scopeRoot] = holder
	}
	b.mu.Unlock()
	_, err := holder.Apply(context.Background(), env)
	return err
}
