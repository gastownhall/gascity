// Package commandcensus owns the strict, deterministic source format used to
// generate Gas City's closed product-metrics command domain.
package commandcensus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// SchemaVersion is the current command-census manifest schema version.
const SchemaVersion = 1

// Shape describes whether a command is structural, runnable, or both runnable and a parent.
type Shape string

// ShapeStructural, ShapeRunnable, and ShapeRunnableGroup are the supported command shapes.
const (
	ShapeStructural    Shape = "structural"
	ShapeRunnable      Shape = "runnable"
	ShapeRunnableGroup Shape = "runnable-group"
)

// NoticePolicy controls whether a command invocation may show the product-metrics notice.
type NoticePolicy string

// NoticeEligible and NoticeIneligible are the supported notice policies.
const (
	NoticeEligible   NoticePolicy = "eligible"
	NoticeIneligible NoticePolicy = "ineligible"
)

// RecordingPolicy controls whether a command invocation may be recorded.
type RecordingPolicy string

// RecordingRecordable and RecordingExcluded are the supported recording policies.
const (
	RecordingRecordable RecordingPolicy = "recordable"
	RecordingExcluded   RecordingPolicy = "excluded"
)

// Owner identifies which classifier path owns a command's final classification.
type Owner string

// OwnerStructural, OwnerImmediate, OwnerDeferred, and OwnerExcluded are the supported classifier owners.
const (
	OwnerStructural Owner = "structural"
	OwnerImmediate  Owner = "immediate"
	OwnerDeferred   Owner = "deferred"
	OwnerExcluded   Owner = "excluded"
)

// Mode identifies the static product-metrics behavior assigned to a command.
type Mode string

// Mode constants enumerate the supported static product-metrics behaviors.
const (
	ModeStandard          Mode = "standard"
	ModeCompletion        Mode = "completion"
	ModeVersion           Mode = "version"
	ModeBDPassthrough     Mode = "bd-passthrough"
	ModeEventsStream      Mode = "events-stream"
	ModePerfWrapper       Mode = "perf-wrapper"
	ModeWorkflowCompat    Mode = "workflow-compat"
	ModeSupervisorService Mode = "supervisor-service"
	ModePackCommand       Mode = "pack-command"
	ModeHiddenPrivate     Mode = "hidden-private"
	ModeMetricsControl    Mode = "metrics-control"
	ModeHookProtocol      Mode = "hook-protocol"
	ModeEventEmit         Mode = "event-emit"
	ModeCredentialHelper  Mode = "credential-helper"
	ModePrivateCompletion Mode = "private-completion"
)

// Exclusion identifies why a command invocation must not be recorded.
type Exclusion string

// Exclusion constants enumerate the reviewed reasons an invocation may be excluded.
const (
	ExclusionHiddenPrivate     Exclusion = "hidden-private"
	ExclusionMetricsControl    Exclusion = "metrics-control"
	ExclusionHookProtocol      Exclusion = "hook-protocol"
	ExclusionEventEmit         Exclusion = "event-emit"
	ExclusionCredentialHelper  Exclusion = "credential-helper"
	ExclusionPrivateCompletion Exclusion = "private-completion"
	ExclusionPrimeHook         Exclusion = "prime-hook"
	ExclusionHandoffAutomation Exclusion = "handoff-automation"
	ExclusionMailHookFormat    Exclusion = "mail-hook-format"
	ExclusionManagedContext    Exclusion = "managed-context"
	ExclusionProviderHook      Exclusion = "provider-hook"
)

// ConditionalMode identifies a runtime condition that can change a command's static policy.
type ConditionalMode string

// ConditionalMode constants enumerate the supported runtime policy conditions.
const (
	ConditionalGenericMachineOutput ConditionalMode = "generic-machine-output"
	ConditionalManagedContext       ConditionalMode = "managed-context"
	ConditionalProviderHook         ConditionalMode = "provider-hook"
	ConditionalBeadsMachineOutput   ConditionalMode = "beads-machine-output"
	ConditionalPrimeHook            ConditionalMode = "prime-hook"
	ConditionalHandoffAutomation    ConditionalMode = "handoff-automation"
	ConditionalMailHookFormat       ConditionalMode = "mail-hook-format"
)

// HiddenException identifies a reviewed hidden command that remains recordable.
type HiddenException string

// HiddenExceptionPerfWrapper and HiddenExceptionWorkflowCompat are the reviewed hidden-command exceptions.
const (
	HiddenExceptionPerfWrapper    HiddenException = "perf-wrapper"
	HiddenExceptionWorkflowCompat HiddenException = "workflow-compat"
)

// DeferredDefault identifies the fallback classification for a deferred command resolver.
type DeferredDefault string

// DeferredDefaultHelp and DeferredDefaultUnknown are the supported deferred fallbacks.
const (
	DeferredDefaultHelp    DeferredDefault = "help"
	DeferredDefaultUnknown DeferredDefault = "unknown"
)

// Manifest is the strict source model for the product-metrics command census.
type Manifest struct {
	SchemaVersion          int               `json:"schema_version"`
	NextID                 uint16            `json:"next_id"`
	PermanentIDs           []Identity        `json:"permanent_ids"`
	GlobalConditionalModes []ConditionalMode `json:"global_conditional_modes"`
	Commands               []Command         `json:"commands"`
	Synthetic              []Command         `json:"synthetic"`
	Tombstones             []Tombstone       `json:"tombstones"`
}

// Identity is a stable command ID and wire name allocation.
type Identity struct {
	Name    string `json:"name"`
	ID      uint16 `json:"id"`
	Wire    string `json:"wire"`
	Retired bool   `json:"retired"`
}

// Command describes one live or synthetic command-census row.
type Command struct {
	Path               string            `json:"path"`
	Aliases            []string          `json:"aliases"`
	Hidden             bool              `json:"hidden"`
	EffectiveHidden    bool              `json:"effective_hidden"`
	DisableFlagParsing bool              `json:"disable_flag_parsing"`
	Shape              Shape             `json:"shape"`
	Classification     string            `json:"classification"`
	Mode               Mode              `json:"mode"`
	NoticePolicy       NoticePolicy      `json:"notice_policy"`
	RecordingPolicy    RecordingPolicy   `json:"recording_policy"`
	Owner              Owner             `json:"owner"`
	Resolver           string            `json:"resolver,omitempty"`
	Exclusion          Exclusion         `json:"exclusion,omitempty"`
	CanonicalTarget    string            `json:"canonical_target,omitempty"`
	CanonicalIdentity  bool              `json:"canonical_identity,omitempty"`
	HiddenException    HiddenException   `json:"hidden_exception,omitempty"`
	ConditionalModes   []ConditionalMode `json:"conditional_modes"`
	DeferredDefault    DeferredDefault   `json:"deferred_default,omitempty"`
	ID                 uint16            `json:"id,omitempty"`
}

// Tombstone preserves a retired command identity so its ID cannot be reused.
type Tombstone struct {
	Name string `json:"name"`
	ID   uint16 `json:"id"`
	Wire string `json:"wire"`
}

// DeepCopy returns a manifest whose slice fields can be mutated independently.
func (manifest Manifest) DeepCopy() Manifest {
	copyManifest := manifest
	copyManifest.PermanentIDs = append([]Identity(nil), manifest.PermanentIDs...)
	copyManifest.GlobalConditionalModes = append([]ConditionalMode(nil), manifest.GlobalConditionalModes...)
	copyManifest.Commands = cloneCommands(manifest.Commands)
	copyManifest.Synthetic = cloneCommands(manifest.Synthetic)
	copyManifest.Tombstones = append([]Tombstone(nil), manifest.Tombstones...)
	return copyManifest
}

func cloneCommands(commands []Command) []Command {
	cloned := append([]Command(nil), commands...)
	for index := range cloned {
		cloned[index].Aliases = make([]string, len(commands[index].Aliases))
		copy(cloned[index].Aliases, commands[index].Aliases)
		cloned[index].ConditionalModes = make([]ConditionalMode, len(commands[index].ConditionalModes))
		copy(cloned[index].ConditionalModes, commands[index].ConditionalModes)
	}
	return cloned
}

// DecodeManifest decodes strict manifest JSON, rejecting duplicate, unknown, or missing fields.
func DecodeManifest(data []byte) (Manifest, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Manifest{}, fmt.Errorf("command census: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("command census: decode: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Manifest{}, fmt.Errorf("command census: %w", err)
	}
	if err := validateRequiredJSONFields(data); err != nil {
		return Manifest{}, fmt.Errorf("command census: %w", err)
	}
	return manifest, nil
}

// ValidateManifest verifies all command-census schema and allocation invariants.
func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("command census: schema_version = %d, want %d", manifest.SchemaVersion, SchemaVersion)
	}
	wantPermanent := []Identity{
		{Name: "help", ID: 1, Wire: "help"},
		{Name: "version", ID: 2, Wire: "version"},
		{Name: "unknown", ID: 3, Wire: "unknown"},
		{Name: "pack-command", ID: 4, Wire: "pack-command"},
	}
	if len(manifest.PermanentIDs) != len(wantPermanent) {
		return fmt.Errorf("command census: permanent_ids length = %d, want %d", len(manifest.PermanentIDs), len(wantPermanent))
	}
	if err := validateConditionalModes(manifest.GlobalConditionalModes, true); err != nil {
		return err
	}
	wantGlobalModes := []ConditionalMode{ConditionalGenericMachineOutput, ConditionalManagedContext, ConditionalProviderHook}
	if !reflect.DeepEqual(manifest.GlobalConditionalModes, wantGlobalModes) {
		return fmt.Errorf("command census: global_conditional_modes = %q, want %q", manifest.GlobalConditionalModes, wantGlobalModes)
	}
	if err := validateSortedRows(manifest); err != nil {
		return err
	}
	for index, want := range wantPermanent {
		if got := manifest.PermanentIDs[index]; got != want {
			return fmt.Errorf("command census: permanent_ids[%d] = %+v, want %+v", index, got, want)
		}
	}

	type catalogEntry struct {
		name      string
		wire      string
		permanent bool
		tombstone bool
	}
	byID := make(map[uint16]catalogEntry)
	byWire := make(map[string]uint16)
	byName := make(map[string]uint16)
	maxID := uint16(0)
	addIdentity := func(name string, id uint16, wire string, permanent, tombstone bool) error {
		if id == 0 || !validWire(wire) || !validWire(name) {
			return fmt.Errorf("command census: invalid identity name=%q id=%d wire=%q", name, id, wire)
		}
		if existing, ok := byID[id]; ok {
			if existing.wire != wire {
				return fmt.Errorf("command census: id %d maps to both %q and %q", id, existing.wire, wire)
			}
			if tombstone || existing.tombstone {
				return fmt.Errorf("command census: tombstone id %d is still active or duplicated", id)
			}
		} else {
			byID[id] = catalogEntry{name: name, wire: wire, permanent: permanent, tombstone: tombstone}
		}
		if existingID, ok := byWire[wire]; ok && existingID != id {
			return fmt.Errorf("command census: wire %q maps to both %d and %d", wire, existingID, id)
		}
		byWire[wire] = id
		if existingID, ok := byName[name]; ok && existingID != id {
			return fmt.Errorf("command census: name %q maps to both %d and %d", name, existingID, id)
		}
		byName[name] = id
		if id > maxID {
			maxID = id
		}
		return nil
	}
	for _, identity := range manifest.PermanentIDs {
		if err := addIdentity(identity.Name, identity.ID, identity.Wire, true, false); err != nil {
			return err
		}
	}

	paths := make(map[string]string)
	validateRows := func(kind string, rows []Command) error {
		for index, row := range rows {
			if err := validateCommand(row, kind == "synthetic"); err != nil {
				return fmt.Errorf("command census: %s[%d]: %w", kind, index, err)
			}
			if previous, ok := paths[row.Path]; ok {
				return fmt.Errorf("command census: duplicate path %q in %s and %s", row.Path, previous, kind)
			}
			paths[row.Path] = kind
			if row.RecordingPolicy == RecordingRecordable {
				if err := addIdentity(row.Classification, row.ID, row.Classification, false, false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := validateRows("commands", manifest.Commands); err != nil {
		return err
	}
	if err := validateRows("synthetic", manifest.Synthetic); err != nil {
		return err
	}
	if err := validateCanonicalGroups(manifest.Commands); err != nil {
		return err
	}

	for _, tombstone := range manifest.Tombstones {
		if err := addIdentity(tombstone.Name, tombstone.ID, tombstone.Wire, false, true); err != nil {
			return err
		}
	}
	if manifest.NextID == 0 || manifest.NextID <= maxID {
		return fmt.Errorf("command census: next_id = %d, must be greater than maximum allocated id %d", manifest.NextID, maxID)
	}
	for id := uint16(1); id < manifest.NextID; id++ {
		if _, allocated := byID[id]; !allocated {
			return fmt.Errorf("command census: allocation hole at id %d below next_id %d", id, manifest.NextID)
		}
	}
	if err := validateSyntheticRows(manifest.Synthetic); err != nil {
		return err
	}
	return nil
}

func validateCommand(command Command, synthetic bool) error {
	if err := validateCanonicalPath(command.Path, synthetic); err != nil {
		return err
	}
	switch command.Shape {
	case ShapeStructural, ShapeRunnable, ShapeRunnableGroup:
	default:
		return fmt.Errorf("invalid shape %q", command.Shape)
	}
	if err := validateAliases(command.Path, command.Aliases); err != nil {
		return err
	}
	if err := validateConditionalModes(command.ConditionalModes, false); err != nil {
		return err
	}
	if err := validateHiddenException(command, synthetic); err != nil {
		return err
	}
	decision, ok := staticModeDecision(command.Mode)
	if !ok {
		return fmt.Errorf("invalid mode %q", command.Mode)
	}
	if decision.notice != command.NoticePolicy || decision.recording != command.RecordingPolicy || decision.exclusion != command.Exclusion {
		return fmt.Errorf("mode %q requires notice=%q recording=%q exclusion=%q", command.Mode, decision.notice, decision.recording, decision.exclusion)
	}
	switch command.NoticePolicy {
	case NoticeEligible, NoticeIneligible:
	default:
		return fmt.Errorf("invalid notice policy %q", command.NoticePolicy)
	}
	switch command.RecordingPolicy {
	case RecordingRecordable:
		if command.Exclusion != "" || command.ID == 0 || !validWire(command.Classification) || command.Classification == "excluded" {
			return fmt.Errorf("invalid recordable identity id=%d classification=%q exclusion=%q", command.ID, command.Classification, command.Exclusion)
		}
	case RecordingExcluded:
		if command.Exclusion == "" || command.ID != 0 || command.Classification != "excluded" || command.CanonicalTarget != "" || command.CanonicalIdentity {
			return fmt.Errorf("recording exclusion must have reason, zero id, and excluded classification")
		}
	default:
		return fmt.Errorf("invalid recording policy %q", command.RecordingPolicy)
	}
	switch command.Owner {
	case OwnerStructural:
		if command.Shape != ShapeStructural || command.RecordingPolicy == RecordingExcluded || command.Resolver != "" || command.DeferredDefault != "" {
			return fmt.Errorf("invalid structural owner")
		}
	case OwnerImmediate:
		if command.Shape == ShapeStructural || command.RecordingPolicy == RecordingExcluded || command.Resolver != "" || command.DeferredDefault != "" {
			return fmt.Errorf("invalid immediate owner")
		}
	case OwnerDeferred:
		if (!synthetic && command.Shape != ShapeRunnableGroup) || command.RecordingPolicy == RecordingExcluded || command.Resolver == "" || (!synthetic && command.DeferredDefault != DeferredDefaultHelp && command.DeferredDefault != DeferredDefaultUnknown) {
			return fmt.Errorf("invalid deferred owner")
		}
		if !synthetic {
			wantID, wantClassification, wantTarget := uint16(1), "help", "@help"
			if command.DeferredDefault == DeferredDefaultUnknown {
				wantID, wantClassification, wantTarget = 3, "unknown", "@unknown"
			}
			if command.ID != wantID || command.Classification != wantClassification || command.CanonicalTarget != wantTarget {
				return fmt.Errorf("deferred default %q requires id=%d classification=%q target=%q", command.DeferredDefault, wantID, wantClassification, wantTarget)
			}
		}
	case OwnerExcluded:
		if command.RecordingPolicy != RecordingExcluded || command.Resolver != "" || command.DeferredDefault != "" {
			return fmt.Errorf("invalid excluded owner")
		}
	default:
		return fmt.Errorf("invalid owner %q", command.Owner)
	}
	return nil
}

func validateHiddenException(command Command, synthetic bool) error {
	if synthetic {
		if command.HiddenException != "" {
			return fmt.Errorf("synthetic row has hidden exception")
		}
		return nil
	}
	if !command.EffectiveHidden {
		if command.HiddenException != "" {
			return fmt.Errorf("visible row has hidden exception %q", command.HiddenException)
		}
		return nil
	}
	if command.RecordingPolicy == RecordingRecordable && command.NoticePolicy != NoticeIneligible {
		return fmt.Errorf("effectively hidden recordable row must be notice-ineligible")
	}
	if command.RecordingPolicy == RecordingExcluded {
		if command.HiddenException != "" {
			return fmt.Errorf("excluded hidden row has hidden exception %q", command.HiddenException)
		}
		return nil
	}
	switch command.HiddenException {
	case HiddenExceptionPerfWrapper, HiddenExceptionWorkflowCompat:
	default:
		return fmt.Errorf("effectively hidden recordable row %q lacks a reviewed exception", command.Path)
	}
	return nil
}

func validateCanonicalGroups(commands []Command) error {
	byPath := make(map[string]Command, len(commands))
	byID := make(map[uint16][]Command)
	for _, command := range commands {
		byPath[command.Path] = command
		if command.RecordingPolicy == RecordingRecordable && command.ID > 4 {
			byID[command.ID] = append(byID[command.ID], command)
		}
		if command.RecordingPolicy != RecordingRecordable || command.ID == 0 {
			continue
		}
		switch command.ID {
		case 1, 2, 3:
			wantTarget := "@" + command.Classification
			if command.CanonicalTarget != wantTarget || command.CanonicalIdentity {
				return fmt.Errorf("command census: %q must reference permanent identity %q", command.Path, wantTarget)
			}
			if command.ID == 3 && (command.Owner != OwnerDeferred || command.DeferredDefault != DeferredDefaultUnknown) {
				return fmt.Errorf("command census: unknown identity is allowed only as a deferred default")
			}
		case 4:
			return fmt.Errorf("command census: %q uses a synthetic-only permanent identity", command.Path)
		}
	}
	for id, group := range byID {
		if len(group) == 1 {
			command := group[0]
			if command.CanonicalTarget != "" || command.CanonicalIdentity {
				return fmt.Errorf("command census: %q has unnecessary canonical metadata", command.Path)
			}
			wantWire := strings.ReplaceAll(strings.TrimPrefix(command.Path, "gc "), " ", "-")
			if command.Classification != wantWire {
				return fmt.Errorf("command census: %q classification = %q, want canonical wire %q", command.Path, command.Classification, wantWire)
			}
			continue
		}
		canonicalCount := 0
		canonicalPath := ""
		for _, command := range group {
			if command.CanonicalIdentity {
				canonicalCount++
				canonicalPath = command.Path
				if command.CanonicalTarget != "" {
					return fmt.Errorf("command census: canonical row %q also has a target", command.Path)
				}
			}
		}
		if canonicalCount != 1 {
			return fmt.Errorf("command census: shared id %d has %d canonical rows, want 1", id, canonicalCount)
		}
		for _, command := range group {
			if command.CanonicalIdentity {
				continue
			}
			canonical, ok := byPath[command.CanonicalTarget]
			if !ok || command.CanonicalTarget != canonicalPath || canonical.ID != id || canonical.Classification != command.Classification || !canonical.CanonicalIdentity {
				return fmt.Errorf("command census: %q has invalid canonical target %q", command.Path, command.CanonicalTarget)
			}
		}
	}
	return nil
}

func validateAliases(path string, aliases []string) error {
	if aliases == nil {
		return fmt.Errorf("aliases must be a non-null array")
	}
	seen := make(map[string]struct{}, len(aliases))
	canonical := path[strings.LastIndex(path, " ")+1:]
	previous := ""
	for _, alias := range aliases {
		if strings.TrimSpace(alias) == "" {
			return fmt.Errorf("empty alias")
		}
		if _, exists := seen[alias]; exists {
			return fmt.Errorf("duplicate alias %q", alias)
		}
		if alias == canonical {
			return fmt.Errorf("alias %q equals canonical name", alias)
		}
		if previous != "" && alias < previous {
			return fmt.Errorf("aliases are not sorted")
		}
		seen[alias] = struct{}{}
		previous = alias
	}
	return nil
}

func validateConditionalModes(modes []ConditionalMode, global bool) error {
	if modes == nil {
		return fmt.Errorf("conditional_modes must be a non-null array")
	}
	previous := ConditionalMode("")
	for _, mode := range modes {
		if previous != "" && mode <= previous {
			return fmt.Errorf("conditional_modes are not strictly sorted")
		}
		if global {
			switch mode {
			case ConditionalGenericMachineOutput, ConditionalManagedContext, ConditionalProviderHook:
			default:
				return fmt.Errorf("conditional mode %q is not global", mode)
			}
		} else {
			switch mode {
			case ConditionalBeadsMachineOutput, ConditionalPrimeHook, ConditionalHandoffAutomation, ConditionalMailHookFormat:
			default:
				return fmt.Errorf("conditional mode %q is not command-scoped", mode)
			}
		}
		previous = mode
	}
	return nil
}

func validateCanonicalPath(path string, synthetic bool) error {
	if path == "" || strings.TrimSpace(path) != path || strings.Join(strings.Fields(path), " ") != path {
		return fmt.Errorf("non-canonical path %q", path)
	}
	if synthetic {
		return nil
	}
	if path != "gc" && !strings.HasPrefix(path, "gc ") {
		return fmt.Errorf("live path %q is outside gc", path)
	}
	if strings.ContainsAny(path, "<>\t\r\n") || strings.HasPrefix(path, "gc __") {
		return fmt.Errorf("invalid live path %q", path)
	}
	return nil
}

func validateSortedRows(manifest Manifest) error {
	for index := 1; index < len(manifest.Commands); index++ {
		if manifest.Commands[index-1].Path >= manifest.Commands[index].Path {
			return fmt.Errorf("command census: commands are not strictly sorted by path")
		}
	}
	for index := 1; index < len(manifest.Tombstones); index++ {
		previous, current := manifest.Tombstones[index-1], manifest.Tombstones[index]
		if previous.ID >= current.ID {
			return fmt.Errorf("command census: tombstones are not strictly sorted")
		}
	}
	return nil
}

type modeDecision struct {
	notice    NoticePolicy
	recording RecordingPolicy
	exclusion Exclusion
}

func staticModeDecision(mode Mode) (modeDecision, bool) {
	switch mode {
	case ModeStandard:
		return modeDecision{notice: NoticeEligible, recording: RecordingRecordable}, true
	case ModeCompletion, ModeVersion, ModeBDPassthrough, ModeEventsStream, ModePerfWrapper, ModeWorkflowCompat, ModeSupervisorService, ModePackCommand:
		return modeDecision{notice: NoticeIneligible, recording: RecordingRecordable}, true
	case ModeHiddenPrivate:
		return modeDecision{notice: NoticeIneligible, recording: RecordingExcluded, exclusion: ExclusionHiddenPrivate}, true
	case ModeMetricsControl:
		return modeDecision{notice: NoticeIneligible, recording: RecordingExcluded, exclusion: ExclusionMetricsControl}, true
	case ModeHookProtocol:
		return modeDecision{notice: NoticeIneligible, recording: RecordingExcluded, exclusion: ExclusionHookProtocol}, true
	case ModeEventEmit:
		return modeDecision{notice: NoticeIneligible, recording: RecordingExcluded, exclusion: ExclusionEventEmit}, true
	case ModeCredentialHelper:
		return modeDecision{notice: NoticeIneligible, recording: RecordingExcluded, exclusion: ExclusionCredentialHelper}, true
	case ModePrivateCompletion:
		return modeDecision{notice: NoticeIneligible, recording: RecordingExcluded, exclusion: ExclusionPrivateCompletion}, true
	default:
		return modeDecision{}, false
	}
}

func validateSyntheticRows(rows []Command) error {
	if len(rows) != 3 {
		return fmt.Errorf("command census: synthetic length = %d, want 3", len(rows))
	}
	want := []Command{
		{Path: "gc <unknown>", Aliases: []string{}, ConditionalModes: []ConditionalMode{}, Shape: ShapeRunnable, Classification: "unknown", Mode: ModeStandard, NoticePolicy: NoticeEligible, RecordingPolicy: RecordingRecordable, Owner: OwnerDeferred, Resolver: "root-dispatch", ID: 3},
		{Path: "gc <pack-command>", Aliases: []string{}, ConditionalModes: []ConditionalMode{}, Shape: ShapeRunnable, Classification: "pack-command", Mode: ModePackCommand, NoticePolicy: NoticeIneligible, RecordingPolicy: RecordingRecordable, Owner: OwnerDeferred, Resolver: "pack-dispatch", ID: 4},
		{Path: "gc __complete", Aliases: []string{"__completeNoDesc"}, ConditionalModes: []ConditionalMode{}, Hidden: true, EffectiveHidden: true, DisableFlagParsing: true, Shape: ShapeRunnable, Classification: "excluded", Mode: ModePrivateCompletion, NoticePolicy: NoticeIneligible, RecordingPolicy: RecordingExcluded, Owner: OwnerExcluded, Exclusion: ExclusionPrivateCompletion},
	}
	for index := range want {
		if !commandsEqual(rows[index], want[index]) {
			return fmt.Errorf("command census: synthetic[%d] drifted", index)
		}
	}
	return nil
}

func commandsEqual(left, right Command) bool {
	return reflect.DeepEqual(left, right)
}

func validWire(wire string) bool {
	if wire == "" || len(wire) > 64 || !utf8.ValidString(wire) {
		return false
	}
	return regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`).MatchString(wire)
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
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
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	_, err = decoder.Token()
	return err
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateRequiredJSONFields(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, field := range []string{"schema_version", "next_id", "permanent_ids", "global_conditional_modes", "commands", "synthetic", "tombstones"} {
		value, ok := raw[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("required field %q is missing or null", field)
		}
	}
	for _, collection := range []string{"commands", "synthetic"} {
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(raw[collection], &rows); err != nil {
			return fmt.Errorf("%s: %w", collection, err)
		}
		for index, row := range rows {
			for _, field := range []string{
				"path", "aliases", "hidden", "effective_hidden", "disable_flag_parsing", "shape",
				"classification", "mode", "notice_policy", "recording_policy", "owner", "conditional_modes",
			} {
				value, ok := row[field]
				if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return fmt.Errorf("%s[%d] required field %q is missing or null", collection, index, field)
				}
			}
		}
	}
	return nil
}
