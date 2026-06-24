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
	"github.com/gastownhall/gascity/internal/transcriptmeta"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// muxRebuildInterval is how often the exporter re-enumerates city providers so
// cities that start or stop after launch are picked up.
const muxRebuildInterval = 60 * time.Second

// minActorSaltLen mirrors the projection's fail-closed salt floor: a shorter
// salt makes the actor hash brute-forceable, so eventexport.ProjectEvent drops
// every event. Used only to warn loudly at startup.
const minActorSaltLen = 16

// startEventExport launches the redacted event exporter when [events.export] is
// configured. It is opt-in: with no endpoint the supervisor ships nothing.
//
// Enabling export also arms the transcript-session sidecars
// (internal/transcriptmeta): the same opt-in that ships correlated events writes
// a session-id sidecar next to each keyed transcript, since the sidecar is only
// useful when the correlated event stream is being consumed. Arming is deferred
// until the exporter clears its fail-closed startup (durable cursor loaded), so a
// refused start — e.g. a corrupt cursor — leaves sidecars off rather than writing
// correlation files with no event stream to join. The gate is per-process, so a
// one-shot CLI that delivers a turn without a supervisor writes no sidecar until
// the supervisor next touches the transcript.
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
	// The projection fails closed on a salt shorter than 16 bytes (a short salt
	// makes the actor hash brute-forceable), which silently drops every event. A
	// loud startup warning turns that into an operator-visible misconfiguration
	// instead of a dark exporter. loadOrCreateSalt always yields a 32-hex salt, so
	// this only fires on a too-short inline actor_salt.
	if len(salt) < minActorSaltLen {
		logf("WARNING: actor salt is %d bytes (< %d); the exporter will DROP ALL events — set a longer [events.export] actor_salt", len(salt), minActorSaltLen)
	}

	exp := eventexport.New(eventexport.Config{
		Endpoint:      ec.Endpoint,
		TokenProvider: tokenProvider,
		Salt:          salt,
		ExportRef:     ec.ExportRefEnabled(),
		// Events now carry typed run_id/session_id stamped at the record site, so
		// emit the opaque correlation ids. They are safeRef-gated and remain
		// within the v1 wire schema (the envelope already defines both as optional
		// omitempty fields), so this does not bump SchemaVersion.
		EmitCorrelation: true,
		BatchMax:        ec.BatchMaxEvents,
		BatchInterval:   ec.BatchIntervalDuration(),
		Logf:            logf,
	})

	cursorPath := filepath.Join(homeDir, "events-export-cursor.json")
	cursors, err := eventexport.LoadCursors(cursorPath)
	if err != nil {
		// Fail closed: a corrupt or unreadable cursor file would otherwise floor
		// each city at its current head and silently skip every event accumulated
		// since the last durable cursor. Refuse to start and surface it so an
		// operator can repair the file (or remove it to deliberately start fresh)
		// rather than lose exports.
		logf("ERROR: cannot read durable export cursor %s (refusing to start; remove it to start fresh): %v", cursorPath, err)
		return
	}
	exp.SetCursors(cursors)

	// Arm transcript-session sidecars only now that the exporter has cleared its
	// fail-closed startup: the sidecar and the event stream are one correlated
	// opt-in, so a start that refuses (e.g. the corrupt-cursor return above) must
	// not leave sidecars writing .gcmeta files that imply an event stream exists.
	transcriptmeta.SetEnabled(true)

	src := eventfeed.NewMuxSource(providers, exp.Cursors, muxRebuildInterval, logf)
	go func() { _ = exp.Run(ctx, src) }()
	go persistExportCursors(ctx, exp, cursorPath, logf)

	logf("enabled -> %s (envelope-only metadata; no payloads leave the box)", ec.Endpoint)
}

// persistExportCursors snapshots the exporter cursor to disk periodically and on
// shutdown so a restart resumes without re-reading the whole history. A save
// failure is logged rather than swallowed: a full disk or bad permissions means
// a restart would resume from a stale cursor (re-exporting or skipping events),
// which an operator needs to see.
func persistExportCursors(ctx context.Context, exp *eventexport.Exporter, path string, logf func(string, ...any)) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	save := func() {
		if err := eventexport.SaveCursors(path, exp.Cursors()); err != nil {
			logf("WARNING: failed to persist export cursor %s (a restart may re-export or skip events): %v", path, err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			save()
			return
		case <-t.C:
			save()
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
			tok := strings.TrimSpace(string(b))
			if tok == "" {
				// A configured token source that resolves empty is a config/auth
				// error, not the deliberately-anonymous opt-out (which is the nil
				// provider). Fail closed so the cursor holds and the empty
				// credential surfaces, rather than silently downgrading to an
				// unauthenticated POST.
				return "", fmt.Errorf("token_file %s resolved to an empty token", tokenFile)
			}
			return tok, nil
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
		// works, and warn that the salt is not random. Must be >= minActorSaltLen
		// bytes or the projection would fail closed and drop every event.
		fmt.Fprintf(stderr, "gc events-export: WARNING: could not generate a random salt: %v\n", err) //nolint:errcheck
		return "events-export-fallback-salt"
	}
	salt := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(salt+"\n"), 0o600); err != nil {
		fmt.Fprintf(stderr, "gc events-export: WARNING: could not persist salt (hashes will change on restart): %v\n", err) //nolint:errcheck
	}
	return salt
}
