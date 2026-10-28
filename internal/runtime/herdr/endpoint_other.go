//go:build !windows

package herdr

import (
	"context"
	"net"
	"os"
	"path/filepath"
)

// herdrConfigDir returns the directory herdr resolves its configuration, and
// with it every session socket, under: os.UserConfigDir. On Linux and the other
// XDG platforms that is a non-empty XDG_CONFIG_HOME, else $HOME/.config, which
// matches what herdr was measured doing on Linux with XDG_CONFIG_HOME set and
// unset (see socketPath). On darwin os.UserConfigDir is $HOME/Library/Application
// Support, ignoring XDG_CONFIG_HOME, and whether herdr agrees there is
// unverified (client_test.go).
func herdrConfigDir() string {
	configDir, _ := os.UserConfigDir()
	return configDir
}

// herdrJoin is filepath.Join, as socketPath has always joined herdr's paths on
// these platforms.
func herdrJoin(base string, elem ...string) string {
	return filepath.Join(append([]string{base}, elem...)...)
}

// dialUnix is the production connect, installed on every client by newClient and
// reached through the field of the same name so a test can observe the context the
// dial runs under. That context is the one thing no listener-free test can otherwise
// see: a real unix connect completes or fails at once unless the listener backlog is
// saturated, so a missing bound looks exactly like a present one.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// removeStaleSocket unlinks the socket inode when it exists but nothing live is
// listening, so a freshly launched server can bind. Guard with serverAlive
// first — only call once liveness has already returned false.
func (c *client) removeStaleSocket() {
	if fi, err := os.Stat(c.socketPath()); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(c.socketPath())
	}
}
