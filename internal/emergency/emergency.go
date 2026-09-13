// Package emergency implements the dolt-independent emergency spool writer.
package emergency

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
)

const (
	// SeverityInfo is an informational emergency level.
	SeverityInfo = "info"
	// SeverityWarn is a degradation emergency level.
	SeverityWarn = "warn"
	// SeverityError is the default emergency level.
	SeverityError = "error"
	// SeverityCritical is the substrate-failure emergency level.
	SeverityCritical = "critical"

	// MaxMessageBytes caps emergency message bodies at 4 KiB.
	MaxMessageBytes = 4 * 1024
)

const (
	recordIDTimestampLayout = "20060102T150405Z"
	notifyDedupeDirName     = ".notify-dedupe"
)

var recordIDPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{8}$`)

// Record is the JSON spool record persisted under .gc/emergency/.
type Record struct {
	ID         string            `json:"id"`
	Severity   string            `json:"severity"`
	Actor      string            `json:"actor"`
	Message    string            `json:"message"`
	RefBead    string            `json:"ref_bead,omitempty"`
	SourcePath string            `json:"source_path,omitempty"`
	SourcePID  int               `json:"source_pid,omitempty"`
	Hostname   string            `json:"hostname,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// IsEventPayload marks Record as an events.Payload-compatible shape.
func (Record) IsEventPayload() {}

// RecordOptions supplies fields for NewRecord.
type RecordOptions struct {
	Severity   string
	Actor      string
	Message    string
	RefBead    string
	SourcePath string
	SourcePID  int
	Hostname   string
	Metadata   map[string]string
	Now        func() time.Time
	Random     io.Reader
}

// DedupeResult describes whether an OS notification should be fired.
type DedupeResult struct {
	Fire      bool
	KeyPrefix string
	Age       time.Duration
}

// NewRecord validates options and builds a spool record with a stable ID.
func NewRecord(opts RecordOptions) (Record, error) {
	severity := strings.TrimSpace(opts.Severity)
	if severity == "" {
		severity = SeverityError
	}
	if !ValidSeverity(severity) {
		return Record{}, fmt.Errorf("severity must be one of info, warn, error, critical")
	}
	message := strings.TrimSpace(opts.Message)
	if message == "" {
		return Record{}, fmt.Errorf("message is required")
	}
	if len([]byte(message)) > MaxMessageBytes {
		return Record{}, fmt.Errorf("message exceeds 4 KiB cap (got %d bytes)", len([]byte(message)))
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = "human"
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	random := opts.Random
	if random == nil {
		random = rand.Reader
	}
	createdAt := now().UTC()
	suffix := make([]byte, 4)
	if _, err := io.ReadFull(random, suffix); err != nil {
		return Record{}, fmt.Errorf("generating emergency id: %w", err)
	}
	return Record{
		ID:         createdAt.Format(recordIDTimestampLayout) + "-" + hex.EncodeToString(suffix),
		Severity:   severity,
		Actor:      actor,
		Message:    message,
		RefBead:    strings.TrimSpace(opts.RefBead),
		SourcePath: strings.TrimSpace(opts.SourcePath),
		SourcePID:  opts.SourcePID,
		Hostname:   strings.TrimSpace(opts.Hostname),
		CreatedAt:  createdAt,
		Metadata:   cloneMetadata(opts.Metadata),
	}, nil
}

// ValidSeverity reports whether severity is one of the supported levels.
func ValidSeverity(severity string) bool {
	switch strings.TrimSpace(severity) {
	case SeverityInfo, SeverityWarn, SeverityError, SeverityCritical:
		return true
	default:
		return false
	}
}

// ValidRecordID reports whether id has the emergency spool ID format.
func ValidRecordID(id string) bool {
	return recordIDPattern.MatchString(strings.TrimSpace(id))
}

// SpoolDir returns the city-local emergency spool directory.
func SpoolDir(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "emergency")
}

// WriteSpool atomically writes rec to the city-local emergency spool.
func WriteSpool(cityPath string, rec Record) (string, error) {
	if strings.TrimSpace(cityPath) == "" {
		return "", fmt.Errorf("city path is required")
	}
	if !ValidRecordID(rec.ID) {
		return "", fmt.Errorf("invalid emergency record id %q", rec.ID)
	}
	dir := SpoolDir(cityPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating emergency spool dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("setting emergency spool dir permissions: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding emergency record: %w", err)
	}
	data = append(data, '\n')
	finalPath := filepath.Join(dir, rec.ID+".json")
	if err := writeFileAtomic(finalPath, data, 0o600); err != nil {
		return "", err
	}
	return finalPath, nil
}

// RecordSignaled records an emergency.signaled event for rec.
func RecordSignaled(recorder events.Recorder, rec Record) error {
	return recordEmergencyEvent(recorder, events.EmergencySignaled, rec)
}

// RecordAcked records an emergency.acked event for rec.
func RecordAcked(recorder events.Recorder, rec Record) error {
	return recordEmergencyEvent(recorder, events.EmergencyAcked, rec)
}

// RecordSignaledToCityLog mirrors rec to the city-local events.jsonl file.
func RecordSignaledToCityLog(cityPath string, rec Record, stderr io.Writer) error {
	eventPath := citylayout.RuntimePath(cityPath, "events.jsonl")
	provider, err := events.NewFileRecorder(eventPath, stderr)
	if err != nil {
		return fmt.Errorf("opening events.jsonl: %w", err)
	}
	defer provider.Close() //nolint:errcheck // best-effort close
	return RecordSignaled(provider, rec)
}

// NotifyDedupeKey returns a filesystem-safe key for OS-notification dedupe.
func NotifyDedupeKey(severity, message string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(severity) + "\x00" + strings.TrimSpace(message)))
	return strings.TrimSpace(severity) + "-" + hex.EncodeToString(sum[:])
}

// MarkNotifyDedupe marks a notification key and reports whether to fire.
func MarkNotifyDedupe(cityPath, key string, now time.Time, ttl time.Duration) (DedupeResult, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return DedupeResult{}, fmt.Errorf("dedupe key is required")
	}
	if key == "." || key == ".." || strings.ContainsAny(key, "/\\") {
		return DedupeResult{}, fmt.Errorf("invalid dedupe key: must not contain path separators or dot elements")
	}
	dir := filepath.Join(SpoolDir(cityPath), notifyDedupeDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return DedupeResult{}, fmt.Errorf("creating notify dedupe dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return DedupeResult{}, fmt.Errorf("setting notify dedupe dir permissions: %w", err)
	}
	path := filepath.Join(dir, key)
	prefix := key
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_ = f.Close()
		_ = os.Chtimes(path, now, now)
		return DedupeResult{Fire: true, KeyPrefix: prefix}, nil
	}
	if !os.IsExist(err) {
		return DedupeResult{}, fmt.Errorf("creating notify dedupe marker: %w", err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return DedupeResult{}, fmt.Errorf("reading notify dedupe marker: %w", statErr)
	}
	age := now.Sub(info.ModTime())
	if ttl > 0 && age >= 0 && age < ttl {
		return DedupeResult{Fire: false, KeyPrefix: prefix, Age: age}, nil
	}
	if err := os.Chtimes(path, now, now); err != nil {
		return DedupeResult{}, fmt.Errorf("updating notify dedupe marker: %w", err)
	}
	return DedupeResult{Fire: true, KeyPrefix: prefix, Age: age}, nil
}

func recordEmergencyEvent(recorder events.Recorder, eventType string, rec Record) error {
	if recorder == nil {
		return fmt.Errorf("events recorder is nil")
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding %s payload: %w", eventType, err)
	}
	recorder.Record(events.Event{
		Type:    eventType,
		Actor:   rec.Actor,
		Subject: rec.ID,
		Message: rec.Message,
		Payload: payload,
	})
	return nil
}

func writeFileAtomic(finalPath string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(finalPath)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		tmpPath := fmt.Sprintf("%s.%d.tmp", finalPath, attempt)
		f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			if os.IsExist(err) {
				lastErr = err
				continue
			}
			return fmt.Errorf("creating temp spool file: %w", err)
		}
		writeErr := writeAndClose(f, data)
		if writeErr != nil {
			_ = os.Remove(tmpPath)
			return writeErr
		}
		if err := linkTempFile(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("committing emergency spool file: %w", err)
		}
		_ = os.Remove(tmpPath)
		return nil
	}
	return fmt.Errorf("creating temp spool file after retries in %s: %w", dir, lastErr)
}

func linkTempFile(tmpPath, finalPath string) error {
	if err := os.Link(tmpPath, finalPath); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("record already exists: %w", err)
		}
		return err
	}
	return nil
}

func writeAndClose(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing emergency spool file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing emergency spool file: %w", err)
	}
	return nil
}

func init() {
	events.RegisterPayload(events.EmergencySignaled, Record{})
	events.RegisterPayload(events.EmergencyAcked, Record{})
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
