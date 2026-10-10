package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	beadslib "github.com/steveyegge/beads"
	"gopkg.in/yaml.v3"
)

// openNativeDoltStorage opens the best-available library storage for beadsDir
// and applies the workspace's events-journal activation to it, as bd's own
// store factories do (beads internal/eventsjournal.ActivateStore). The library
// open leaves the journal off on every store it returns, so without this gc's
// native writes are missing from a journal the workspace asked for while
// every bd CLI write to the same database is in it.
//
// Activation is per store instance and read once, at open: a long-lived
// handle keeps the answer it opened with. A workspace that enables the journal
// on a backend that cannot record it fails the open, closing the store, rather
// than returning a handle whose writes would silently go unrecorded.
func openNativeDoltStorage(ctx context.Context, beadsDir string) (beadslib.Storage, error) {
	storage, err := nativeDoltOpenBestAvailable(ctx, beadsDir)
	if err != nil || storage == nil || !nativeEventsJournalEnabledFor(beadsDir) {
		return storage, err
	}
	configurer, ok := storage.(interface{ SetEventsJournalEnabled(bool) })
	if !ok {
		_ = storage.Close()
		return nil, fmt.Errorf("%s enables the events journal, but its storage backend does not support it", beadsDir)
	}
	configurer.SetEventsJournalEnabled(true)
	return storage, nil
}

// nativeEventsJournalEnabledFor resolves activation with bd's precedence
// (beads internal/eventsjournal.EnabledFor): a parseable BD_EVENTS_JOURNAL,
// then the target workspace's own config.yaml, then off. An unreadable file
// or an unparseable value falls through to the next source, as it does in bd.
func nativeEventsJournalEnabledFor(beadsDir string) bool {
	if enabled, ok := parseNativeJournalBool(os.LookupEnv("BD_EVENTS_JOURNAL")); ok {
		return enabled
	}
	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		return false
	}
	var root map[string]any
	if yaml.Unmarshal(data, &root) != nil || root["events-journal"] == nil {
		return false
	}
	enabled, _ := parseNativeJournalBool(fmt.Sprint(root["events-journal"]), true)
	return enabled
}

// parseNativeJournalBool accepts what bd's parser does: strconv.ParseBool plus
// the YAML 1.1 spellings yes/no/on/off, in any case.
func parseNativeJournalBool(raw string, present bool) (enabled, ok bool) {
	if !present {
		return false, false
	}
	value := strings.TrimSpace(raw)
	if parsed, err := strconv.ParseBool(value); err == nil {
		return parsed, true
	}
	switch strings.ToLower(value) {
	case "yes", "on":
		return true, true
	case "no", "off":
		return false, true
	}
	return false, false
}
