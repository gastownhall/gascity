package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/credsource"
	"github.com/gastownhall/gascity/internal/config"
)

// Per-city / per-rig credentials for a remote beads backend (DESIGN C3, G7):
// city.toml [beads] credential and rigs.beads_credential. gc reads where the
// bearer lives from config; internal/beads resolves it once per scope at open
// and uses it as that scope's ONLY credential, for the native store, the
// wire_compat handshake and the bd subprocess alike. A scope with none keeps
// bd's ambient ladder (single city per server and process).
//
// An env:NAME source is read from gc's own environment and then moved out of
// it (sequesterCityCredentialEnv), so no child process inherits NAME: gc
// processes an agent runs cannot read it either. When cities share a
// supervisor, or agents run gc against the remote store, prefer file: or
// command:.

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
// a child of another scope. Each child re-resolves its scope, and the token
// comes from the scope's shared provider (refreshed per its source's policy),
// so a rotated token reaches the next child.
type bdScopeCredentialEnv struct {
	cityPath string
}

func newBdScopeCredentialEnv(cityPath string) *bdScopeCredentialEnv {
	return &bdScopeCredentialEnv{cityPath: cityPath}
}

// apply writes scopeRoot's credential env into env when the scope has a
// per-scope credential; otherwise env is left as built (bd's ambient ladder).
func (b *bdScopeCredentialEnv) apply(env map[string]string, scopeRoot string) error {
	if b == nil || env == nil {
		return nil
	}
	_, err := beads.NewRemoteCredentialSubprocessEnv(b.cityPath, scopeRoot).Apply(context.Background(), env)
	return err
}

// cityCredentialEnvNames lists the variables a city's env: credential sources
// name ([beads] credential and every rig's beads_credential).
func cityCredentialEnvNames(creds cityHTTPCredentials) []string {
	var names []string
	add := func(c cityHTTPCredential) {
		if c.ok && c.setting.Source.Kind == credsource.KindEnv {
			names = append(names, c.setting.Source.Env)
		}
	}
	add(creds.city)
	for _, rig := range creds.byRoot {
		add(rig)
	}
	return names
}

// sequesterCityCredentialEnv moves the variables cfg's env: credential sources
// name out of the process environment (beads.SequesterRemoteCredentialEnv), so
// no child gc spawns afterwards (agents, bd subprocesses, hooks) inherits a
// token sourced from the supervisor's environment. The composition root calls
// it as soon as a city's config is loaded, before the city spawns anything.
func sequesterCityCredentialEnv(cityPath string, cfg *config.City) {
	beads.SequesterRemoteCredentialEnv(cityCredentialEnvNames(cityHTTPCredentialsFromConfig(cityPath, cfg))...)
}
