package exec

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

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
// after it stops the city's sessions (`gc stop`, the controller's
// shutdown). For a pack whose handshake declares server-lifecycle it runs
// `server-stop`, which exits 0 only when the pack's server is gone and no
// session it started for the city remains, and 0 again when no server runs,
// so a second teardown is nil. Its failure, a declared op's exit 2 included,
// is returned with the pack's stderr, which names what may survive. For a
// pack whose handshake does not declare the token it sends nothing and
// returns nil: what it did before the token existed.
//
// A failed handshake does not tell the two apart, so TeardownServer runs one
// fresh handshake past the failure Protocol keeps for handshakeRetryAfter
// (teardown runs once per stop, so the spawn is cheap). When that fails too
// it sends nothing and returns the handshake's error: a server the pack
// declared may survive.
func (p *Provider) TeardownServer() error {
	info, err := p.Protocol()
	if err != nil {
		info, err = p.queryProtocol(context.Background(), p.timeout)
	}
	if err != nil {
		return fmt.Errorf("handshake unread, so a server-lifecycle server may survive: %w", err)
	}
	if !info.Has(runtime.ProtocolCapabilityServerLifecycle) {
		return nil
	}
	_, err = p.run(nil, serverStopOp)
	return err
}
