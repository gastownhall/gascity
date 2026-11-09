package exec

import "github.com/gastownhall/gascity/internal/runtime"

// serverStopOp is the op a pack declaring server-lifecycle answers to end its
// server.
const serverStopOp = "server-stop"

// ConfigureServer implements [runtime.ServerLifecycleProvider] as a
// documented no-op: it sends no op and returns nil. gc never configures an
// exec pack's server at boot; a pack brings its own server up when its first
// op needs it, so there is nothing for gc to apply, and calling it again
// changes nothing.
func (p *Provider) ConfigureServer() error { return nil }

// TeardownServer implements [runtime.ServerLifecycleProvider]. gc calls it
// once every session of the city is drained (`gc stop`, the controller's
// shutdown). For a pack whose handshake declares server-lifecycle it runs
// `server-stop`, which exits 0 only when the pack's server is gone and no
// session it started for the city remains, and 0 again when no server runs,
// so a second teardown is nil. Its failure, a declared op's exit 2 included,
// is returned with the pack's stderr, which names what may survive. For any
// other pack, a failed handshake included, it sends nothing and returns nil:
// what it did before the token existed.
func (p *Provider) TeardownServer() error {
	if !p.handshakeCapability(runtime.ProtocolCapabilityServerLifecycle) {
		return nil
	}
	_, err := p.run(nil, serverStopOp)
	return err
}
