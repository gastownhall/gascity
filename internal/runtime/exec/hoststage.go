package exec

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/setupcmd"
)

// This file is the fs.host half of the exec provider: what gc does on the
// controller's host for a pack whose boxes share its filesystem and user
// (runtime.ProtocolInfo.SharesHostFS). gc then stages the work dir, runs
// pre_start before the box is created and session_setup after the agent is
// up, all on the host, and leaves that work out of the start config, so the
// pack does exactly what the config carries. It does that only into a name
// the pack reports vacant (Start, provisionBox, startFresh). Every reader of
// the token goes through sharesHostFS, so locality and the token are read
// together.

// defaultSetupTimeout is the per-command budget for host pre_start and
// session_setup when the provider was built without one: the [session]
// setup_timeout default.
const defaultSetupTimeout = 10 * time.Second

// Option configures a [Provider] at construction.
type Option func(*Provider)

// WithSetupTimeouts sets the budget of each host-side pre_start and
// session_setup command a pack sharing the host filesystem leaves to gc:
// timeout is [session] setup_timeout (a fixed deadline, or with maxTimeout
// set, the longest silence) and maxTimeout is [session] setup_max_timeout
// (zero: off). A non-positive timeout keeps the default.
func WithSetupTimeouts(timeout, maxTimeout time.Duration) Option {
	return func(p *Provider) {
		if timeout > 0 {
			p.setupTimeout = timeout
		}
		p.setupMaxTimeout = maxTimeout
	}
}

// HostCapabilities is what an exec runtime whose handshake is info
// guarantees about the controller's host ([runtime.HostCapabilities]): both
// capabilities exactly when the handshake shares the host filesystem, since
// gc then stages the work dir and runs pre_start on the host itself, failing
// the start when pre_start fails; none otherwise.
func HostCapabilities(info runtime.ProtocolInfo) runtime.HostCapabilities {
	if !info.SharesHostFS() {
		return runtime.HostCapabilities{}
	}
	return runtime.HostCapabilities{SharesHostFilesystem: true, RunsHostPreStart: true}
}

// sharesHostFS reports whether the pack's handshake declares that its boxes
// share the controller's host filesystem and user. A failed handshake does
// not: gc then does no host work and sends the full start config.
func (p *Provider) sharesHostFS() bool {
	info, err := p.Protocol()
	return err == nil && info.SharesHostFS()
}

// LocalTranscripts implements [runtime.LocalTranscriptProvider]: a pack
// sharing the host filesystem runs the agent on this host as this user, so
// the transcript it writes is readable here. Any other pack's box is taken
// to be elsewhere.
func (p *Provider) LocalTranscripts() bool {
	return p.sharesHostFS()
}

// startConfigFor returns the config a `start` or `provision` op sends for
// name. With hostWork, it first stages cfg's work dir and runs its pre_start
// on the host, a pre_start failure failing it, and leaves that work and the
// session_setup gc runs after the launch out of the config.
func (p *Provider) startConfigFor(ctx context.Context, name string, cfg runtime.Config, hostWork bool) ([]byte, error) {
	marshal := marshalStartConfig
	if hostWork {
		if err := runtime.StageSessionWorkDirWithWarnings(cfg, p.warnings()); err != nil {
			return nil, fmt.Errorf("exec provider: staging the work dir of %q on the host: %w", name, err)
		}
		for i, cmd := range cfg.PreStart {
			if err := p.runSetupCommand(ctx, cmd, cfg.Env); err != nil {
				return nil, fmt.Errorf("exec provider: running pre_start for %q on the host: pre_start[%d]: %w", name, i, err)
			}
		}
		marshal = marshalHostStagedStartConfig
	}
	data, err := marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("exec provider: marshaling start config: %w", err)
	}
	return data, nil
}

// runHostSessionSetup runs cfg's session_setup commands, then its
// session_setup_script, on the host with GC_SESSION set, once the agent is
// up. A failure is a warning: the session still works.
func (p *Provider) runHostSessionSetup(ctx context.Context, name string, cfg runtime.Config) {
	if len(cfg.SessionSetup) == 0 && cfg.SessionSetupScript == "" {
		return
	}
	env := make(map[string]string, len(cfg.Env)+1)
	for k, v := range cfg.Env {
		env[k] = v
	}
	env["GC_SESSION"] = name
	for i, cmd := range cfg.SessionSetup {
		if err := p.runSetupCommand(ctx, cmd, env); err != nil {
			fmt.Fprintf(p.warnings(), "gc: session_setup[%d] warning: %v\n", i, err) //nolint:errcheck // best-effort warning
		}
	}
	if cfg.SessionSetupScript != "" {
		if err := p.runSetupCommand(ctx, cfg.SessionSetupScript, env); err != nil {
			fmt.Fprintf(p.warnings(), "gc: session_setup_script warning: %v\n", err) //nolint:errcheck // best-effort warning
		}
	}
}

// runSetupCommand runs one host setup command under the provider's setup
// budget through [setupcmd.Run], which owns the bounded `sh -c`, its
// cooperative cancellation and the redacted output tail. The city's
// directory is the working directory while GC_DIR, which a pre_start may be
// creating, does not exist yet.
func (p *Provider) runSetupCommand(ctx context.Context, cmd string, env map[string]string) error {
	return setupcmd.Run(ctx, cmd, env, setupcmd.Options{
		Timeout:     p.setupTimeout,
		MaxTimeout:  p.setupMaxTimeout,
		FallbackDir: p.cityPath,
	})
}

// warnings is where non-fatal host-setup diagnostics go.
func (p *Provider) warnings() io.Writer {
	if p.setupWarnings != nil {
		return p.setupWarnings
	}
	return os.Stderr
}

var (
	_ runtime.LocalTranscriptProvider = (*Provider)(nil)
	_ runtime.LocalTranscriptProvider = (*seamBackedProvider)(nil)
)

// LocalTranscripts passes the underlying provider's answer through.
func (s *seamBackedProvider) LocalTranscripts() bool {
	return s.raw.LocalTranscripts()
}
