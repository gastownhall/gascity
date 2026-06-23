package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/eventfeed"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// muxRebuildInterval is how often the exporter re-enumerates city providers so
// cities that start or stop after launch are picked up.
const muxRebuildInterval = 60 * time.Second

// startEventExport launches the redacted event exporter when [events.export] is
// configured. It is opt-in: with no endpoint the supervisor ships nothing.
//
// The exporter watches the same per-city providers the API serves (via the
// eventfeed adapter), projects each event to an envelope-only shell, and POSTs
// batches to the configured endpoint. It runs in its own goroutine, holds its
// cursor on sink failure, and applies backpressure rather than blocking event
// recording.
func startEventExport(ctx context.Context, ec supervisor.ExportConfig, providers func() map[string]events.Provider, homeDir string, stderr io.Writer) {
	logf := func(format string, args ...any) {
		fmt.Fprintf(stderr, "gc events-export: "+format+"\n", args...) //nolint:errcheck
	}
	tokenProvider, salt := resolveExportCredentials(ec, homeDir, stderr)

	// One-shot startup probe so a fat-fingered token_file surfaces a clear
	// warning instead of only a silent per-POST cursor stall. Non-fatal: the
	// token may legitimately be rotated in after launch.
	if tokenProvider != nil {
		if _, err := tokenProvider(); err != nil {
			logf("WARNING: token unreadable at startup (will retry on each POST): %v", err)
		}
	}

	exp := eventexport.New(eventexport.Config{
		Endpoint:      ec.Endpoint,
		TokenProvider: tokenProvider,
		Salt:          salt,
		ExportRef:     ec.ExportRefEnabled(),
		BatchMax:      ec.BatchMaxEvents,
		BatchInterval: ec.BatchIntervalDuration(),
		Logf:          logf,
	})

	cursorPath := filepath.Join(homeDir, "events-export-cursor.json")
	exp.SetCursors(eventexport.LoadCursors(cursorPath))

	src := eventfeed.NewMuxSource(providers, exp.Cursors, muxRebuildInterval, logf)
	go func() { _ = exp.Run(ctx, src) }()
	go persistExportCursors(ctx, exp, cursorPath)

	logf("enabled -> %s (envelope-only metadata; no payloads leave the box)", ec.Endpoint)
}

// persistExportCursors snapshots the exporter cursor to disk periodically and on
// shutdown so a restart resumes without re-reading the whole history.
func persistExportCursors(ctx context.Context, exp *eventexport.Exporter, path string) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = eventexport.SaveCursors(path, exp.Cursors()) //nolint:errcheck
			return
		case <-t.C:
			_ = eventexport.SaveCursors(path, exp.Cursors()) //nolint:errcheck
		}
	}
}

// resolveExportCredentials builds the bearer-token provider and the actor-hash
// salt. The token is read from token_file (re-read on each POST so it can be
// rotated out of band) when set, otherwise from the inline token; with neither,
// the provider is nil and no Authorization header is sent. The salt is the
// inline actor_salt or, absent that, a random per-install secret persisted
// locally — never the token or endpoint, which the receiver knows and could use
// to reverse the actor hash.
func resolveExportCredentials(ec supervisor.ExportConfig, homeDir string, stderr io.Writer) (func() (string, error), []byte) {
	var tokenProvider func() (string, error)
	switch {
	case strings.TrimSpace(ec.TokenFile) != "":
		tokenFile := strings.TrimSpace(ec.TokenFile)
		tokenProvider = func() (string, error) {
			b, err := os.ReadFile(tokenFile)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(b)), nil
		}
	case ec.Token != "":
		token := ec.Token
		tokenProvider = func() (string, error) { return token, nil }
	}

	salt := ec.ActorSalt
	if salt == "" {
		salt = loadOrCreateSalt(homeDir, stderr)
	}
	return tokenProvider, []byte(salt)
}

// loadOrCreateSalt returns a stable random per-install actor-hash salt, creating
// it on first use. It is a local secret: it is never sent to the endpoint, so
// the receiver cannot reverse the actor hash.
func loadOrCreateSalt(homeDir string, stderr io.Writer) string {
	path := filepath.Join(homeDir, "events-export-salt")
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Extremely unlikely; fall back to a non-empty constant so hashing still
		// works, and warn that the salt is not random.
		fmt.Fprintf(stderr, "gc events-export: WARNING: could not generate a random salt: %v\n", err) //nolint:errcheck
		return "events-export"
	}
	salt := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(salt+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "gc events-export: WARNING: could not persist salt (hashes will change on restart): %v\n", err) //nolint:errcheck
	}
	return salt
}
