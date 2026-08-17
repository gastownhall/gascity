// Package productmetrics defines Gas City's closed, privacy-bounded product
// metrics wire contract.
package productmetrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/Masterminds/semver/v3"
)

const (
	// SchemaVersionV1 is the only supported product-metrics wire schema.
	SchemaVersionV1 = 1
	// AppGasCity is the literal application namespace on every event.
	AppGasCity = "gascity"
	// MaxBatchEvents bounds one v1 request body.
	MaxBatchEvents = 25
)

// OperatingSystem is the closed v0 operating-system domain.
type OperatingSystem string

const (
	// OSLinux identifies an official Linux build.
	OSLinux OperatingSystem = "linux"
	// OSDarwin identifies an official macOS build.
	OSDarwin OperatingSystem = "darwin"
)

// CommandID is a closed command classification. Its numeric representation is
// intentionally unrelated to its wire representation so arbitrary strings
// cannot be forged by converting them to CommandID.
type CommandID uint16

const (
	// CommandHelp classifies help invocations.
	CommandHelp CommandID = iota + 1
	// CommandVersion classifies version invocations.
	CommandVersion
	// CommandUnknown classifies invocations that cannot be resolved safely.
	CommandUnknown
	// CommandPackCommand classifies all runtime-created pack commands.
	CommandPackCommand
)

type commandIDEntry struct {
	id   CommandID
	wire string
}

type commandIDCatalog func(func(commandIDEntry))

func permanentCommandIDCatalog(yield func(commandIDEntry)) {
	yield(commandIDEntry{id: CommandHelp, wire: "help"})
	yield(commandIDEntry{id: CommandVersion, wire: "version"})
	yield(commandIDEntry{id: CommandUnknown, wire: "unknown"})
	yield(commandIDEntry{id: CommandPackCommand, wire: "pack-command"})
}

func productionCommandIDCatalog(yield func(commandIDEntry)) {
	permanentCommandIDCatalog(yield)
	generatedCommandIDCatalog(yield)
}

type commandIDIndex struct {
	byID   map[CommandID]string
	byWire map[string]CommandID
}

func indexCommandIDCatalog(catalog commandIDCatalog) (commandIDIndex, error) {
	if catalog == nil {
		return commandIDIndex{}, fmt.Errorf("productmetrics: nil command ID catalog")
	}
	index := commandIDIndex{
		byID:   make(map[CommandID]string),
		byWire: make(map[string]CommandID),
	}
	var catalogErr error
	catalog(func(entry commandIDEntry) {
		if catalogErr != nil {
			return
		}
		if entry.id == 0 || entry.wire == "" || len(entry.wire) > 64 || !printableASCII(entry.wire) {
			catalogErr = fmt.Errorf("productmetrics: invalid command ID catalog entry")
			return
		}
		if previous, exists := index.byID[entry.id]; exists {
			catalogErr = fmt.Errorf("productmetrics: command ID %d maps to both %q and %q", entry.id, previous, entry.wire)
			return
		}
		if previous, exists := index.byWire[entry.wire]; exists {
			catalogErr = fmt.Errorf("productmetrics: command wire ID %q maps to both %d and %d", entry.wire, previous, entry.id)
			return
		}
		index.byID[entry.id] = entry.wire
		index.byWire[entry.wire] = entry.id
	})
	if catalogErr != nil {
		return commandIDIndex{}, catalogErr
	}
	return index, nil
}

func commandIDWire(id CommandID, catalog commandIDCatalog) (string, error) {
	index, err := indexCommandIDCatalog(catalog)
	if err != nil {
		return "", err
	}
	wire, ok := index.byID[id]
	if !ok {
		return "", fmt.Errorf("productmetrics: invalid command ID %d", id)
	}
	return wire, nil
}

func commandIDFromWire(wire string, catalog commandIDCatalog) (CommandID, error) {
	index, err := indexCommandIDCatalog(catalog)
	if err != nil {
		return 0, err
	}
	id, ok := index.byWire[wire]
	if !ok {
		return 0, fmt.Errorf("productmetrics: unknown command_id %q", wire)
	}
	return id, nil
}

// String returns id's canonical wire value, or an empty string for a value
// outside the closed domain.
func (id CommandID) String() string {
	wire, err := commandIDWire(id, productionCommandIDCatalog)
	if err != nil {
		return ""
	}
	return wire
}

// MarshalJSON encodes a command ID as its validated canonical wire string.
func (id CommandID) MarshalJSON() ([]byte, error) {
	wire, err := commandIDWire(id, productionCommandIDCatalog)
	if err != nil {
		return nil, err
	}
	return json.Marshal(wire)
}

// UnmarshalJSON accepts only a member of the closed command domain.
func (id *CommandID) UnmarshalJSON(data []byte) error {
	if id == nil {
		return fmt.Errorf("productmetrics: cannot unmarshal command_id into nil receiver")
	}
	var wire string
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("productmetrics: command_id must be a string: %w", err)
	}
	parsed, err := commandIDFromWire(wire, productionCommandIDCatalog)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// Event is the exact closed v0 queue and network event DTO.
type Event struct {
	EventID         string          `json:"event_id"`
	InstallationID  string          `json:"installation_id"`
	App             string          `json:"app"`
	ReleaseVersion  string          `json:"release_version"`
	OS              OperatingSystem `json:"os"`
	OccurredHourUTC string          `json:"occurred_hour_utc"`
	CommandID       CommandID       `json:"command_id"`
}

type eventWire struct {
	EventID         string          `json:"event_id"`
	InstallationID  string          `json:"installation_id"`
	App             string          `json:"app"`
	ReleaseVersion  string          `json:"release_version"`
	OS              OperatingSystem `json:"os"`
	OccurredHourUTC string          `json:"occurred_hour_utc"`
	CommandID       string          `json:"command_id"`
}

// MarshalJSON validates and encodes the exact event shape.
func (event Event) MarshalJSON() ([]byte, error) {
	return encodeEventWithCommandIDCatalog(event, productionCommandIDCatalog)
}

// UnmarshalJSON rejects duplicate and unknown fields before validating the
// exact event shape.
func (event *Event) UnmarshalJSON(data []byte) error {
	if event == nil {
		return fmt.Errorf("productmetrics: cannot unmarshal event into nil receiver")
	}
	decoded, err := decodeEventWithCommandIDCatalog(data, productionCommandIDCatalog)
	if err != nil {
		return err
	}
	*event = decoded
	return nil
}

func encodeEventWithCommandIDCatalog(event Event, catalog commandIDCatalog) ([]byte, error) {
	if err := event.validateWithoutCommandID(); err != nil {
		return nil, err
	}
	commandID, err := commandIDWire(event.CommandID, catalog)
	if err != nil {
		return nil, err
	}
	return json.Marshal(eventWire{
		EventID:         event.EventID,
		InstallationID:  event.InstallationID,
		App:             event.App,
		ReleaseVersion:  event.ReleaseVersion,
		OS:              event.OS,
		OccurredHourUTC: event.OccurredHourUTC,
		CommandID:       commandID,
	})
}

func decodeEventWithCommandIDCatalog(data []byte, catalog commandIDCatalog) (Event, error) {
	var wire eventWire
	if err := strictUnmarshalObject(data, &wire, exactEventField); err != nil {
		return Event{}, fmt.Errorf("productmetrics: decode event: %w", err)
	}
	commandID, err := commandIDFromWire(wire.CommandID, catalog)
	if err != nil {
		return Event{}, err
	}
	decoded := Event{
		EventID:         wire.EventID,
		InstallationID:  wire.InstallationID,
		App:             wire.App,
		ReleaseVersion:  wire.ReleaseVersion,
		OS:              wire.OS,
		OccurredHourUTC: wire.OccurredHourUTC,
		CommandID:       commandID,
	}
	if err := decoded.validateWithoutCommandID(); err != nil {
		return Event{}, err
	}
	return decoded, nil
}

func exactEventField(field string) bool {
	switch field {
	case "event_id", "installation_id", "app", "release_version", "os", "occurred_hour_utc", "command_id":
		return true
	default:
		return false
	}
}

func (event Event) validate() error {
	if err := event.validateWithoutCommandID(); err != nil {
		return err
	}
	if _, err := commandIDWire(event.CommandID, productionCommandIDCatalog); err != nil {
		return fmt.Errorf("productmetrics: command_id is outside the closed domain: %w", err)
	}
	return nil
}

func (event Event) validateWithoutCommandID() error {
	if !validCanonicalUUIDv4(event.EventID) {
		return fmt.Errorf("productmetrics: event_id is not a canonical UUIDv4")
	}
	if !validCanonicalUUIDv4(event.InstallationID) {
		return fmt.Errorf("productmetrics: installation_id is not a canonical UUIDv4")
	}
	if event.App != AppGasCity {
		return fmt.Errorf("productmetrics: app must be %q", AppGasCity)
	}
	version, err := semver.StrictNewVersion(event.ReleaseVersion)
	if err != nil || version.String() != event.ReleaseVersion {
		return fmt.Errorf("productmetrics: release_version is not canonical semver")
	}
	if event.OS != OSLinux && event.OS != OSDarwin {
		return fmt.Errorf("productmetrics: unsupported os %q", event.OS)
	}
	const hourLayout = "2006-01-02T15:04:05Z"
	hour, err := time.Parse(hourLayout, event.OccurredHourUTC)
	if err != nil || hour.Minute() != 0 || hour.Second() != 0 || hour.Nanosecond() != 0 || hour.Format(hourLayout) != event.OccurredHourUTC {
		return fmt.Errorf("productmetrics: occurred_hour_utc must be a canonical UTC hour")
	}
	return nil
}

// Batch is the exact v1 product-metrics request envelope.
type Batch struct {
	SchemaVersion int     `json:"schema_version"`
	Events        []Event `json:"events"`
}

// MarshalJSON validates and encodes the exact batch shape.
func (batch Batch) MarshalJSON() ([]byte, error) {
	if err := batch.validate(); err != nil {
		return nil, err
	}
	type batchWire Batch
	return json.Marshal(batchWire(batch))
}

// UnmarshalJSON rejects duplicate and unknown fields before validating the
// exact batch shape and all contained events.
func (batch *Batch) UnmarshalJSON(data []byte) error {
	if batch == nil {
		return fmt.Errorf("productmetrics: cannot unmarshal batch into nil receiver")
	}
	type batchWire Batch
	var wire batchWire
	if err := strictUnmarshalObject(data, &wire, exactBatchField); err != nil {
		return fmt.Errorf("productmetrics: decode batch: %w", err)
	}
	decoded := Batch(wire)
	if err := decoded.validate(); err != nil {
		return err
	}
	*batch = decoded
	return nil
}

func exactBatchField(field string) bool {
	return field == "schema_version" || field == "events"
}

func (batch Batch) validate() error {
	if batch.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("productmetrics: schema_version must be %d", SchemaVersionV1)
	}
	if len(batch.Events) == 0 || len(batch.Events) > MaxBatchEvents {
		return fmt.Errorf("productmetrics: events count must be between 1 and %d", MaxBatchEvents)
	}
	for i, event := range batch.Events {
		if err := event.validate(); err != nil {
			return fmt.Errorf("productmetrics: events[%d]: %w", i, err)
		}
	}
	return nil
}

// EncodeEvent returns the canonical compact representation of event.
func EncodeEvent(event Event) ([]byte, error) {
	return json.Marshal(event)
}

// DecodeEvent strictly decodes one event and rejects trailing JSON.
func DecodeEvent(data []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return Event{}, err
	}
	return event, nil
}

// EncodeBatch returns the canonical compact representation of batch.
func EncodeBatch(batch Batch) ([]byte, error) {
	return json.Marshal(batch)
}

// DecodeBatch strictly decodes one batch and rejects trailing JSON.
func DecodeBatch(data []byte) (Batch, error) {
	var batch Batch
	if err := json.Unmarshal(data, &batch); err != nil {
		return Batch{}, err
	}
	return batch, nil
}

// ExampleBatch returns the fixed, state-independent public v1 help example.
func ExampleBatch() Batch {
	return Batch{
		SchemaVersion: SchemaVersionV1,
		Events: []Event{{
			EventID:         "8c4f4128-a6e8-4f66-bd1b-1fcf1298b124",
			InstallationID:  "3cf9fd4e-3337-4c29-a0ab-2858cd8a1f21",
			App:             AppGasCity,
			ReleaseVersion:  "0.31.0",
			OS:              OSLinux,
			OccurredHourUTC: "2026-07-11T00:00:00Z",
			CommandID:       CommandHelp,
		}},
	}
}

func validCanonicalUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i := range len(value) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !lowerHex(value[i]) {
			return false
		}
	}
	return value[14] == '4' && (value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b')
}

func lowerHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f'
}

func printableASCII(value string) bool {
	for i := range len(value) {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func strictUnmarshalObject(data []byte, target any, allowedField func(string) bool) error {
	if err := validateExactJSONObject(data, allowedField); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateExactJSONObject(data []byte, allowedField func(string) bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if opening != json.Delim('{') {
		return fmt.Errorf("expected JSON object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate JSON key %q", key)
		}
		seen[key] = struct{}{}
		if allowedField == nil || !allowedField(key) {
			return fmt.Errorf("unknown JSON field %q", key)
		}
		if err := scanJSONValue(decoder); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if closing != json.Delim('}') {
		return fmt.Errorf("malformed JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("malformed JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("malformed JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}
