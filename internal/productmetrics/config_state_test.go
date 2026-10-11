package productmetrics

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

const (
	testInstallationID  = "11111111-1111-4111-8111-111111111111"
	testSpoolGeneration = "22222222-2222-4222-8222-222222222222"
)

func TestPersistedStateCanonicalRoundTrip(t *testing.T) {
	want := persistedState{
		StateSchema:               currentStateSchema,
		CounterNamespace:          initialCounterNamespace,
		StateGeneration:           7,
		Preference:                preferenceEnabled,
		RequiredNoticeVersion:     3,
		AcceptedNoticeVersion:     3,
		InstallationID:            testInstallationID,
		SpoolGeneration:           testSpoolGeneration,
		CleanupKind:               cleanupNone,
		CleanupEpoch:              2,
		PausedThroughMetricsEpoch: 0,
	}

	encoded, err := encodePersistedState(want)
	if err != nil {
		t.Fatalf("encodePersistedState() error = %v", err)
	}
	const canonical = "state_schema = 1\n" +
		"counter_namespace = 1\n" +
		"state_generation = 7\n" +
		"preference = \"enabled\"\n" +
		"required_notice_version = 3\n" +
		"accepted_notice_version = 3\n" +
		"installation_id = \"11111111-1111-4111-8111-111111111111\"\n" +
		"spool_generation = \"22222222-2222-4222-8222-222222222222\"\n" +
		"cleanup_kind = \"none\"\n" +
		"cleanup_epoch = 2\n" +
		"paused_through_metrics_epoch = 0\n"
	if string(encoded) != canonical {
		t.Fatalf("encoded state =\n%s\nwant\n%s", encoded, canonical)
	}

	got, err := decodePersistedState(encoded)
	if err != nil {
		t.Fatalf("decodePersistedState() error = %v", err)
	}
	if got != want {
		t.Fatalf("decoded state = %#v, want %#v", got, want)
	}
}

func TestPersistedStateOmitsOnlyOptionalIdentityFields(t *testing.T) {
	state := pendingState(4)
	encoded, err := encodePersistedState(state)
	if err != nil {
		t.Fatalf("encodePersistedState() error = %v", err)
	}
	if strings.Contains(string(encoded), "installation_id") || strings.Contains(string(encoded), "spool_generation") {
		t.Fatalf("pending encoding leaked optional identity fields:\n%s", encoded)
	}
	if _, err := decodePersistedState(encoded); err != nil {
		t.Fatalf("decode pending state: %v", err)
	}
}

func TestPersistedStateStrictSchemaRejectsMalformedUnknownAndIncompleteInput(t *testing.T) {
	valid, err := encodePersistedState(enabledState(1, 1, testInstallationID, testSpoolGeneration))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":                 nil,
		"malformed":             []byte("state_schema = [\n"),
		"unknown scalar":        append(append([]byte(nil), valid...), []byte("surprise = true\n")...),
		"unknown table":         append(append([]byte(nil), valid...), []byte("[future]\nvalue = 1\n")...),
		"case folded alias":     []byte(strings.Replace(string(valid), "state_schema", "STATE_SCHEMA", 1)),
		"duplicate":             append(append([]byte(nil), valid...), []byte("preference = \"enabled\"\n")...),
		"trailing document":     append(append([]byte(nil), valid...), 0),
		"missing generation":    []byte(strings.Replace(string(valid), "state_generation = 1\n", "", 1)),
		"missing namespace":     []byte(strings.Replace(string(valid), "counter_namespace = 1\n", "", 1)),
		"missing cleanup kind":  []byte(strings.Replace(string(valid), "cleanup_kind = \"none\"\n", "", 1)),
		"integer overflow":      []byte(strings.Replace(string(valid), "state_generation = 1", "state_generation = 18446744073709551616", 1)),
		"negative unsigned":     []byte(strings.Replace(string(valid), "cleanup_epoch = 0", "cleanup_epoch = -1", 1)),
		"bounded file exceeded": []byte(strings.Repeat("#", maximumConfigBytes+1)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodePersistedState(data); err == nil {
				t.Fatal("decodePersistedState() error = nil, want strict rejection")
			}
		})
	}
}

func TestPersistedStateRejectsUnknownEnumsVersionsAndExhaustedCounters(t *testing.T) {
	valid := enabledState(1, 1, testInstallationID, testSpoolGeneration)
	cases := map[string]persistedState{
		"zero schema":                  withState(valid, func(s *persistedState) { s.StateSchema = 0 }),
		"newer schema":                 withState(valid, func(s *persistedState) { s.StateSchema = currentStateSchema + 1 }),
		"zero generation":              withState(valid, func(s *persistedState) { s.StateGeneration = 0 }),
		"zero counter namespace":       withState(valid, func(s *persistedState) { s.CounterNamespace = 0 }),
		"overflowed counter namespace": withState(valid, func(s *persistedState) { s.CounterNamespace = terminalCounterNamespace + 1 }),
		"exhausted generation":         withState(valid, func(s *persistedState) { s.StateGeneration = uint64(math.MaxInt64) }),
		"unknown preference":           withState(valid, func(s *persistedState) { s.Preference = preference("maybe") }),
		"unknown cleanup":              withState(valid, func(s *persistedState) { s.CleanupKind = cleanupKind("later") }),
		"exhausted cleanup epoch":      withState(valid, func(s *persistedState) { s.CleanupEpoch = uint64(math.MaxInt64) }),
		"accepted beyond required":     withState(valid, func(s *persistedState) { s.AcceptedNoticeVersion = 2 }),
		"enabled zero notice floor":    withState(valid, func(s *persistedState) { s.RequiredNoticeVersion = 0; s.AcceptedNoticeVersion = 0 }),
		"enabled without ID":           withState(valid, func(s *persistedState) { s.InstallationID = "" }),
		"invalid installation UUID":    withState(valid, func(s *persistedState) { s.InstallationID = "not-a-uuid" }),
		"non-v4 installation UUID":     withState(valid, func(s *persistedState) { s.InstallationID = "11111111-1111-1111-8111-111111111111" }),
		"uppercase installation UUID":  withState(valid, func(s *persistedState) { s.InstallationID = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA" }),
		"invalid spool UUID":           withState(valid, func(s *persistedState) { s.SpoolGeneration = "bad" }),
		"unset with identity":          withState(valid, func(s *persistedState) { s.Preference = preferenceUnset }),
		"disabled with identity":       withState(valid, func(s *persistedState) { s.Preference = preferenceDisabled }),
		"disable cleanup enabled":      withState(valid, func(s *persistedState) { s.CleanupKind = cleanupDisable }),
		"pause cleanup disabled":       withState(disabledState(2, 1, cleanupNone), func(s *persistedState) { s.CleanupKind = cleanupPause }),
		"pause cleanup no epoch":       withState(valid, func(s *persistedState) { s.CleanupKind = cleanupPause }),
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			data := encodeUncheckedState(t, state)
			if _, err := decodePersistedState(data); err == nil {
				t.Fatalf("decodePersistedState(%s) error = nil, want semantic rejection", data)
			}
		})
	}
}

func TestPersistedStateCounterTerminalBoundaryIsExclusive(t *testing.T) {
	tests := map[string]struct {
		mutate func(*persistedState, uint64)
	}{
		"state generation": {mutate: func(state *persistedState, value uint64) { state.StateGeneration = value }},
		"cleanup epoch":    {mutate: func(state *persistedState, value uint64) { state.CleanupEpoch = value }},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for _, boundary := range []struct {
				name  string
				value uint64
				valid bool
			}{
				{name: "lower neighbor", value: maximumStateCounter - 1, valid: true},
				{name: "terminal", value: maximumStateCounter, valid: false},
				{name: "upper neighbor", value: maximumStateCounter + 1, valid: false},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					state := enabledState(7, 1, testInstallationID, testSpoolGeneration)
					test.mutate(&state, boundary.value)
					data := encodeUncheckedState(t, state)
					_, err := decodePersistedState(data)
					if (err == nil) != boundary.valid {
						t.Fatalf("decode counter %d error = %v, want valid=%v", boundary.value, err, boundary.valid)
					}
				})
			}
		})
	}
}

func TestCounterIncrementReservesTerminalValue(t *testing.T) {
	state := enabledState(maximumStateCounter-2, 1, testInstallationID, testSpoolGeneration)
	if err := incrementStateGeneration(&state); err != nil || state.StateGeneration != maximumStateCounter-1 {
		t.Fatalf("increment lower mutable neighbor = (%d, %v), want (%d, nil)", state.StateGeneration, err, maximumStateCounter-1)
	}
	if err := incrementStateGeneration(&state); err == nil {
		t.Fatal("incrementStateGeneration entered the reserved terminal value")
	}

	state = enabledState(7, 1, testInstallationID, testSpoolGeneration)
	state.CleanupEpoch = maximumStateCounter - 2
	if err := incrementCleanupEpoch(&state); err != nil || state.CleanupEpoch != maximumStateCounter-1 {
		t.Fatalf("increment cleanup lower mutable neighbor = (%d, %v), want (%d, nil)", state.CleanupEpoch, err, maximumStateCounter-1)
	}
	if err := incrementCleanupEpoch(&state); err == nil {
		t.Fatal("incrementCleanupEpoch entered the reserved terminal value")
	}
}

func TestCounterNamespaceRecoveryIsMonotonicAndTerminatesWithoutWrapping(t *testing.T) {
	valid := enabledState(7, 1, testInstallationID, testSpoolGeneration)
	valid.CounterNamespace = 41
	data := append(encodeUncheckedState(t, valid), []byte("unknown = true\n")...)
	if got := recoveryCounterNamespace(data); got != 42 {
		t.Fatalf("recover decodable current namespace = %d, want 42", got)
	}

	terminalAdjacent := valid
	terminalAdjacent.CounterNamespace = terminalCounterNamespace - 1
	data = append(encodeUncheckedState(t, terminalAdjacent), []byte("unknown = true\n")...)
	if got := recoveryCounterNamespace(data); got != terminalCounterNamespace {
		t.Fatalf("recover terminal-adjacent namespace = %d, want terminal %d", got, terminalCounterNamespace)
	}

	for name, data := range map[string][]byte{
		"malformed":    []byte("state_schema = [\n"),
		"missing":      []byte("state_schema = 1\n"),
		"newer schema": []byte("state_schema = 2\ncounter_namespace = 8\n"),
		"terminal":     []byte(fmt.Sprintf("state_schema = 1\ncounter_namespace = %d\n", terminalCounterNamespace)),
	} {
		t.Run(name, func(t *testing.T) {
			if got := recoveryCounterNamespace(data); got != terminalCounterNamespace {
				t.Fatalf("recovery namespace = %d, want terminal %d", got, terminalCounterNamespace)
			}
		})
	}
}

func TestTerminalCounterNamespaceAllowsOnlyInactiveDurableFallback(t *testing.T) {
	inactive := enabledState(1, 2, testInstallationID, "")
	inactive.CounterNamespace = terminalCounterNamespace
	inactive.AcceptedNoticeVersion = 1
	if _, err := decodePersistedState(encodeUncheckedState(t, inactive)); err != nil {
		t.Fatalf("decode inactive terminal fallback: %v", err)
	}

	active := enabledState(1, 2, testInstallationID, testSpoolGeneration)
	active.CounterNamespace = terminalCounterNamespace
	if _, err := decodePersistedState(encodeUncheckedState(t, active)); err == nil {
		t.Fatal("active spool decoded in terminal counter namespace")
	}

	if err := advanceCounterNamespace(&inactive); err == nil || inactive.CounterNamespace != terminalCounterNamespace {
		t.Fatalf("terminal namespace advanced or wrapped: state=%#v err=%v", inactive, err)
	}
}

func TestStatusDTOHasOnlyApprovedRedactedFields(t *testing.T) {
	want := []string{
		"State",
		"Reason",
		"HomeStable",
		"HomeReason",
		"ConfigPath",
		"ConfigPresent",
		"StateSchema",
		"RequiredNoticeVersion",
		"AcceptedNoticeVersion",
		"InstallationIDPresent",
		"SpoolGenerationPresent",
		"CleanupPending",
		"QueueEvents",
		"QueueBytes",
		"QueueDiagnosticsAvailable",
		"OldestQueuedEventAge",
		"OldestQueuedEventPresent",
		"DroppedEvents",
		"LastUploadAttemptHourUTC",
		"LastUploadSuccessHourUTC",
		"LastErrorClass",
		"StatusDiagnosticsAvailable",
		"SpawnThrottleAge",
		"SpawnThrottlePresent",
	}
	statusType := reflect.TypeOf(Status{})
	got := make([]string, 0, statusType.NumField())
	for index := range statusType.NumField() {
		field := statusType.Field(index)
		if field.PkgPath != "" {
			t.Fatalf("Status field %q is unexported; DTO fields must be deliberately projected", field.Name)
		}
		got = append(got, field.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Status fields = %v, want only approved redacted fields %v", got, want)
	}
}

func TestPersistedStateAcceptsEachClosedStateShape(t *testing.T) {
	states := []persistedState{
		pendingState(1),
		enabledState(2, 1, "10101010-1010-4010-8010-101010101010", testSpoolGeneration),
		disabledState(3, 0, cleanupNone),
		withState(disabledState(3, 0, cleanupNone), func(s *persistedState) { s.RequiredNoticeVersion = 0 }),
		disabledState(4, 1, cleanupDisable),
		{
			StateSchema: currentStateSchema, CounterNamespace: initialCounterNamespace, StateGeneration: 5,
			Preference: preferenceEnabled, RequiredNoticeVersion: 1, AcceptedNoticeVersion: 1,
			InstallationID: testInstallationID, CleanupKind: cleanupPause, CleanupEpoch: 2,
			PausedThroughMetricsEpoch: 3,
		},
		{
			StateSchema: currentStateSchema, CounterNamespace: initialCounterNamespace, StateGeneration: 6,
			Preference: preferenceEnabled, RequiredNoticeVersion: 2, AcceptedNoticeVersion: 1,
			InstallationID: testInstallationID, CleanupKind: cleanupNone, CleanupEpoch: 2,
		},
	}
	for index, state := range states {
		t.Run(fmt.Sprintf("shape-%d", index), func(t *testing.T) {
			encoded, err := encodePersistedState(state)
			if err != nil {
				t.Fatalf("encode state: %v", err)
			}
			if _, err := decodePersistedState(encoded); err != nil {
				t.Fatalf("decode state: %v\n%s", err, encoded)
			}
		})
	}
}

func withState(state persistedState, mutate func(*persistedState)) persistedState {
	mutate(&state)
	return state
}

func encodeUncheckedState(t *testing.T, state persistedState) []byte {
	t.Helper()
	data, err := encodeStateWire(state)
	if err != nil {
		t.Fatalf("encodeStateWire: %v", err)
	}
	return data
}

func enabledState(generation, noticeVersion uint64, installationID, spoolGeneration string) persistedState {
	return persistedState{
		StateSchema: currentStateSchema, CounterNamespace: initialCounterNamespace, StateGeneration: generation,
		Preference: preferenceEnabled, RequiredNoticeVersion: noticeVersion, AcceptedNoticeVersion: noticeVersion,
		InstallationID: installationID, SpoolGeneration: spoolGeneration,
		CleanupKind: cleanupNone,
	}
}

func pendingState(generation uint64) persistedState {
	return persistedState{
		StateSchema: currentStateSchema, CounterNamespace: initialCounterNamespace, StateGeneration: generation,
		Preference: preferenceUnset, RequiredNoticeVersion: 1,
		CleanupKind: cleanupNone,
	}
}

func disabledState(generation, cleanupEpoch uint64, cleanup cleanupKind) persistedState {
	return persistedState{
		StateSchema: currentStateSchema, CounterNamespace: initialCounterNamespace, StateGeneration: generation,
		Preference: preferenceDisabled, RequiredNoticeVersion: 1,
		CleanupKind: cleanup, CleanupEpoch: cleanupEpoch,
	}
}

func testStateVersion(generation uint64) stateVersion {
	return stateVersion{counterNamespace: initialCounterNamespace, stateGeneration: generation}
}
