//go:build !windows

package herdr

import (
	"context"
	"net"
	"os"
	"path/filepath"
)

// herdrConfigDir is the base directory herdr's config directory sits under,
// resolved by herdr's own non-Windows rule (src/config/io.rs config_dir and
// platform_config_dir), which does not vary by OS: $XDG_CONFIG_HOME, else
// $HOME/.config, else the temp dir; socketPath adds "herdr". os.UserConfigDir()
// is not that rule: on macOS it ignores XDG_CONFIG_HOME and returns
// $HOME/Library/Application Support, a directory herdr never uses (#5770).
// herdr reads each variable with std::env::var, so a variable that is set
// counts even when it is empty or relative; startServer refuses the relative
// socket path that results.
func herdrConfigDir() string {
	if dir, ok := os.LookupEnv("XDG_CONFIG_HOME"); ok {
		return dir
	}
	if home, ok := os.LookupEnv("HOME"); ok {
		return filepath.Join(home, ".config")
	}
	return os.TempDir()
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
