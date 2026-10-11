package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/credsource"
	"github.com/gastownhall/gascity/internal/fsys"
)

const inlineSecret = "ghp_inlineS3cr3tTokenValue"

func loadCredentialCity(t *testing.T, files map[string]string) (*City, error) {
	t.Helper()
	fs := fsys.NewFake()
	for path, body := range files {
		fs.Files[path] = []byte(body)
	}
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	return cfg, err
}

// TestBeadsCredentialRefusesInlineSecrets: a token pasted where a source
// belongs fails config load, through Parse and the composed load alike, and
// the error never echoes it.
func TestBeadsCredentialRefusesInlineSecrets(t *testing.T) {
	for name, body := range map[string]string{
		"city": "[workspace]\nname = \"t\"\n[beads]\ncredential = \"" + inlineSecret + "\"\n",
		"rig":  "[workspace]\nname = \"t\"\n[[rigs]]\nname = \"r\"\npath = \"/r\"\nbeads_credential = \"" + inlineSecret + "\"\n",
	} {
		_, err := loadCredentialCity(t, map[string]string{"/city/city.toml": body})
		if err == nil {
			t.Fatalf("%s: inline secret loaded without error", name)
		}
		if strings.Contains(err.Error(), inlineSecret) {
			t.Fatalf("%s: load error %q echoes the secret", name, err)
		}
		if _, err := Parse([]byte(body)); err == nil || strings.Contains(err.Error(), inlineSecret) {
			t.Fatalf("%s: Parse error = %v, want a refusal that does not echo the secret", name, err)
		}
	}
}

// TestBeadsCredentialForPrecedence: a rig's own beads_credential wins (with
// its own plaintext grant); otherwise the city's [beads] credential applies;
// with neither, the scope keeps the ambient ladder.
func TestBeadsCredentialForPrecedence(t *testing.T) {
	cfg, err := loadCredentialCity(t, map[string]string{"/city/city.toml": `
[workspace]
name = "t"

[beads]
credential = "env:CITY_TOKEN"
allow_insecure_credential = true

[[rigs]]
name = "own"
path = "/own"
beads_credential = "file:/run/secrets/own"

[[rigs]]
name = "inherits"
path = "/inherits"
`})
	if err != nil {
		t.Fatal(err)
	}
	city, ok, err := BeadsCredentialFor(cfg, nil)
	if err != nil || !ok || city.Source.Kind != credsource.KindEnv || city.Source.Env != "CITY_TOKEN" || !city.AllowInsecure || !city.FromCity {
		t.Fatalf("city = %+v ok=%v err=%v", city, ok, err)
	}
	own, ok, err := BeadsCredentialFor(cfg, &cfg.Rigs[0])
	if err != nil || !ok || own.Source.Kind != credsource.KindFile || own.AllowInsecure || own.FromCity || own.Scope != `rig "own"` {
		t.Fatalf("own rig = %+v ok=%v err=%v; want its own file source without the city's grant", own, ok, err)
	}
	inherits, ok, err := BeadsCredentialFor(cfg, &cfg.Rigs[1])
	if err != nil || !ok || !inherits.FromCity || inherits.Source.Env != "CITY_TOKEN" {
		t.Fatalf("inheriting rig = %+v ok=%v err=%v; want the city's credential", inherits, ok, err)
	}

	bare, err := loadCredentialCity(t, map[string]string{"/city/city.toml": "[workspace]\nname = \"t\"\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := BeadsCredentialFor(bare, nil); ok || err != nil {
		t.Fatalf("unconfigured city: ok=%v err=%v, want the ambient ladder", ok, err)
	}
}

// TestBeadsCredentialSurvivesAnUnrelatedBeadsFragment: a fragment that sets
// another [beads] key must not drop the city's credential (which would put the
// city back on the ambient ladder silently).
func TestBeadsCredentialSurvivesAnUnrelatedBeadsFragment(t *testing.T) {
	cfg, err := loadCredentialCity(t, map[string]string{
		"/city/city.toml": `
include = ["fragment.toml"]

[workspace]
name = "t"

[beads]
credential = "command:gc-token-helper --city t"
allow_insecure_credential = true
`,
		"/city/fragment.toml": `
[beads]
bd_compatibility = "bd-1.0.5"
`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Beads.Credential != "command:gc-token-helper --city t" || cfg.Beads.AllowInsecureCredential == nil || !*cfg.Beads.AllowInsecureCredential {
		t.Fatalf("credential = %q allow = %v, want both preserved across the fragment", cfg.Beads.Credential, cfg.Beads.AllowInsecureCredential)
	}
}

// TestBeadsCredentialEnvNameMustMarkASecret: gc sets an env: source's variable
// again in the city's own exec orders and agent sessions, and their output,
// errors and events are redacted by variable name (execenv.IsSensitiveKey), so
// a name that does not mark itself as a secret fails config load, through
// Parse and the composed load alike, without echoing the name (a token pasted
// after env: would otherwise be printed). A controller-only variable is
// refused too: gc never passes one to a child.
func TestBeadsCredentialEnvNameMustMarkASecret(t *testing.T) {
	const (
		pasted         = "ghp16C7e42F292c6912E7710c838347Ae178B4a"
		notSecret      = "must mark it as a secret"
		controllerOnly = "controller-only"
	)
	city := func(name string) string {
		return "[workspace]\nname = \"t\"\n[beads]\ncredential = \"env:" + name + "\"\n"
	}
	rig := func(name string) string {
		return "[workspace]\nname = \"t\"\n[[rigs]]\nname = \"r\"\npath = \"/r\"\nbeads_credential = \"env:" + name + "\"\n"
	}
	for name, tc := range map[string]struct{ body, refusal, unechoed string }{
		"city":                 {city("BEADS_BEARER"), notSecret, "BEARER"},
		"rig":                  {rig("RIG_BEARER"), notSecret, "BEARER"},
		"pasted city":          {city(pasted), notSecret, pasted},
		"pasted rig":           {rig(pasted), notSecret, pasted},
		"controller-only city": {city("GC_CONTROLLER_TOKEN"), controllerOnly, "GC_CONTROLLER_TOKEN"},
		"controller-only rig":  {rig("GC_CONTROLLER_TOKEN"), controllerOnly, "GC_CONTROLLER_TOKEN"},
	} {
		_, err := loadCredentialCity(t, map[string]string{"/city/city.toml": tc.body})
		if err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Fatalf("%s: load error = %v, want a refusal containing %q", name, err, tc.refusal)
		}
		if strings.Contains(err.Error(), tc.unechoed) {
			t.Fatalf("%s: load error %q echoes the variable name", name, err)
		}
		if _, err := Parse([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Fatalf("%s: Parse error = %v, want a refusal containing %q", name, err, tc.refusal)
		}
	}
	cfg, err := loadCredentialCity(t, map[string]string{"/city/city.toml": `
[workspace]
name = "t"

[beads]
credential = "env:BEADS_HTTP_TOKEN"

[[rigs]]
name = "lower"
path = "/lower"
beads_credential = "env:rig_api_key"

[[rigs]]
name = "file"
path = "/file"
beads_credential = "file:/run/secrets/bearer"
`})
	if err != nil {
		t.Fatalf("secret-marked names and a file source: %v", err)
	}
	if rig, ok, err := BeadsCredentialFor(cfg, &cfg.Rigs[0]); err != nil || !ok || rig.Source.Env != "rig_api_key" {
		t.Fatalf("lower-case secret-marked rig = %+v ok=%v err=%v", rig, ok, err)
	}
	cfg.Beads.Credential = "env:BEADS_BEARER"
	if _, _, err := BeadsCredentialFor(cfg, nil); err == nil || !strings.Contains(err.Error(), notSecret) {
		t.Fatalf("BeadsCredentialFor on an unvalidated config = %v, want the secret-name refusal", err)
	}
}
