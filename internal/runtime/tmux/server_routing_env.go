package tmux

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/processenv"
)

// withServerRoutingWithheld returns env with every provider routing name
// (processenv.IsProviderRoutingEnv) that the tmux server's global environment
// holds and env does not set pinned empty, so a session's provider endpoint
// and model names come only from its own env. It never modifies env.
//
// A session env is an overlay on the server's global environment (see
// sessionEnvUnsetKeys), which is the environment of whichever process started
// the server. The server outlives that process: a city tmux server started
// when a shell exported ANTHROPIC_BASE_URL still holds it after a later
// supervisor install stopped carrying it, and would hand it to every new
// session beside the credential the session env carries (#9257). A pin here
// is withheld like any other empty value: an `env -u` prefix on the created
// command, and a session-env removal (durableWithholdKeys) that also covers a
// respawned agent. A name env sets, to a value or empty, is env's to decide,
// so routing that config, an [upstreams] entry or GC_SUPERVISOR_ENV supplies
// still reaches the session.
func (t *Tmux) withServerRoutingWithheld(env map[string]string) (map[string]string, error) {
	held, err := t.serverGlobalRoutingNames()
	if err != nil {
		return nil, err
	}
	var out map[string]string
	for _, name := range held {
		if _, set := env[name]; set {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(env)+len(held))
			maps.Copy(out, env)
		}
		out[name] = ""
	}
	if out == nil {
		return env, nil
	}
	return out, nil
}

// serverGlobalRoutingNames returns the provider routing names the tmux
// server's global environment holds (`show-environment -g`), leaving out the
// names it marks removed. A server that is not running holds none: the
// new-session that starts one starts it from this process's environment,
// which the session env is built from. A server that answers without its
// environment ("no current target" wraps ErrNoServer) is an error, since it
// may hold routing.
func (t *Tmux) serverGlobalRoutingNames() ([]string, error) {
	out, err := t.run("show-environment", "-g")
	if err != nil {
		if errors.Is(err, ErrNoServer) && !errors.Is(err, ErrNoCurrentTarget) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the tmux server's global environment: %w", err)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		// A set name reads "NAME=VALUE" and a removed one "-NAME". A later
		// line of a multi-line value can read like anything: one that is not
		// a valid name is skipped, and one that reads like a routing name
		// pins a name the server does not hold, which the session would not
		// get anyway.
		name, _, ok := strings.Cut(line, "=")
		if ok && validEnvNameRe.MatchString(name) && processenv.IsProviderRoutingEnv(name) {
			names = append(names, name)
		}
	}
	return names, nil
}
