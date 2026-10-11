package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/credsource"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/processenv"
)

// validateBeadsCredential rejects a [beads] credential that is not one of the
// credential source forms. It is a load error, not a warning: a typo (or a
// token pasted inline) silently meaning "use the ambient credential" would
// send some other credential to this city's server. The error never echoes
// the configured value.
func validateBeadsCredential(b BeadsConfig) error {
	if _, err := parseBeadsCredential(b.Credential); err != nil {
		return fmt.Errorf("beads.credential: %w", err)
	}
	return nil
}

// validateRigBeadsCredentials is validateBeadsCredential for every rig's
// beads_credential.
func validateRigBeadsCredentials(rigs []Rig) error {
	for _, r := range rigs {
		if r.BeadsCredential == nil {
			continue
		}
		if strings.TrimSpace(*r.BeadsCredential) == "" {
			return fmt.Errorf("rig %q beads_credential: empty (remove the key to use the city's credential)", r.Name)
		}
		if _, err := parseBeadsCredential(*r.BeadsCredential); err != nil {
			return fmt.Errorf("rig %q beads_credential: %w", r.Name, err)
		}
	}
	return nil
}

// parseBeadsCredential parses a [beads] credential or a rig beads_credential.
// gc sets an env:NAME source's variable again in the city's own exec orders
// and agent sessions, so a gc they run resolves the same source. NAME must
// therefore mark itself as a secret (execenv.IsSensitiveKey), because an
// order's output, errors and events are redacted by variable name, and must
// not be a controller-only variable, which gc never passes to a child. The
// error never echoes NAME, which may be a token pasted after env:.
func parseBeadsCredential(raw string) (credsource.Source, error) {
	src, err := credsource.Parse(raw)
	if err != nil {
		return credsource.Source{}, err
	}
	if src.Kind != credsource.KindEnv {
		return src, nil
	}
	if !execenv.IsSensitiveKey(src.Env) {
		return credsource.Source{}, errors.New("env: the variable name must mark it as a secret (for example by containing TOKEN, SECRET, PASSWORD, CREDENTIAL or API_KEY), because gc passes it to the city's own orders and agents and redacts an order's output by variable name (the name is not echoed)")
	}
	if processenv.IsControllerOnlyEnv(src.Env) {
		return credsource.Source{}, errors.New("env: names a controller-only variable, which gc never passes to the city's orders and agents (the name is not echoed)")
	}
	return src, nil
}

// BeadsCredentialSetting is the effective remote beads credential for one
// scope.
type BeadsCredentialSetting struct {
	// Source is where the bearer lives.
	Source credsource.Source
	// AllowInsecure permits it over plain http to a non-loopback server.
	AllowInsecure bool
	// Scope names where it was configured, for diagnostics: "[beads]" or
	// `rig "<name>"`.
	Scope string
	// FromCity reports that the scope inherits the city's [beads] credential.
	FromCity bool
}

// BeadsCredentialFor resolves the remote beads credential for a scope: the
// rig's beads_credential when set, else the city's [beads] credential. Pass a
// nil rig for the city scope. ok is false when neither is configured (the
// scope keeps the ambient credential ladder). The config was validated at
// load, so a parse error here means an unvalidated config was handed in.
func BeadsCredentialFor(city *City, rig *Rig) (BeadsCredentialSetting, bool, error) {
	if rig != nil && rig.BeadsCredential != nil {
		src, err := parseBeadsCredential(*rig.BeadsCredential)
		if err != nil {
			return BeadsCredentialSetting{}, false, fmt.Errorf("rig %q beads_credential: %w", rig.Name, err)
		}
		if !src.IsZero() {
			return BeadsCredentialSetting{
				Source:        src,
				AllowInsecure: rig.BeadsAllowInsecureCredential != nil && *rig.BeadsAllowInsecureCredential,
				Scope:         fmt.Sprintf("rig %q", rig.Name),
			}, true, nil
		}
	}
	if city == nil {
		return BeadsCredentialSetting{}, false, nil
	}
	src, err := parseBeadsCredential(city.Beads.Credential)
	if err != nil {
		return BeadsCredentialSetting{}, false, fmt.Errorf("beads.credential: %w", err)
	}
	if src.IsZero() {
		return BeadsCredentialSetting{}, false, nil
	}
	return BeadsCredentialSetting{
		Source:        src,
		AllowInsecure: city.Beads.AllowInsecureCredential != nil && *city.Beads.AllowInsecureCredential,
		Scope:         "[beads]",
		FromCity:      true,
	}, true, nil
}
