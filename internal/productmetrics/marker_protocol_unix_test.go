//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/unix"
)

const rootTempCrashExitCode = 86

type rootTempProofObservation struct {
	accepted  bool
	unsettled bool
	exhausted bool
	markers   uint64
	sentinel  bool
	usage     spoolWorkUsage
	fixed     bool
}

func TestRootTempJournalMainAndPeerProofModesAreEquivalent(t *testing.T) {
	tests := []struct {
		name          string
		fixture       string
		wantAccepted  bool
		wantExhausted bool
		wantMarkers   uint64
	}{
		{name: "settled intent", fixture: "intent", wantAccepted: true, wantMarkers: 1},
		{name: "settled bound", fixture: "bound", wantAccepted: true, wantMarkers: 1},
		{name: "malformed", fixture: "malformed", wantMarkers: 1},
		{name: "live bound temp", fixture: "live", wantMarkers: 1},
		{name: "exactly 64", fixture: "64", wantAccepted: true, wantMarkers: maximumStorageTempAttempts},
		{name: "65th sentinel", fixture: "65", wantExhausted: true, wantMarkers: maximumStorageTempAttempts},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observations := make(map[string]rootTempProofObservation, 2)
			markerNameBytes := 0
			for _, mode := range []struct {
				name  string
				fixed bool
			}{
				{name: "peer ordinary"},
				{name: "main fixed", fixed: true},
			} {
				t.Run(mode.name, func(t *testing.T) {
					home := newMetricsTestHome(t)
					root := mustOpenMutableRoot(t, home)
					nameBytes := seedRootTempProofFixture(t, home, root, test.fixture)
					if markerNameBytes == 0 {
						markerNameBytes = nameBytes
					} else if nameBytes != markerNameBytes {
						t.Fatalf("proof modes used different marker name widths: %d and %d", markerNameBytes, nameBytes)
					}
					before := filesystemStateFingerprint(t, home.Root())
					meter := newSpoolWorkMeter(defaultSpoolWorkBudget())
					meter.physicalDirectories = true
					proofErr := proveRootTempJournalReadOnlyWithMeter(root, meter, mode.fixed)
					after := filesystemStateFingerprint(t, home.Root())
					if closeErr := root.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					if before != after {
						t.Fatalf("read-only %s proof mutated fixture\nbefore:\n%s\nafter:\n%s", mode.name, before, after)
					}
					observation := rootTempProofObservation{
						accepted: proofErr == nil, unsettled: errors.Is(proofErr, errUnsettledRootTempJournal),
						exhausted: meter.exhausted, markers: meter.rootTempJournalMarkers,
						sentinel: meter.rootTempJournalSentinel, usage: meter.usage, fixed: meter.fixedEnvelopeClaimed,
					}
					if proofErr != nil && !observation.unsettled {
						t.Fatalf("%s proof returned unexpected error: %v", mode.name, proofErr)
					}
					observations[mode.name] = observation
				})
			}

			peer := observations["peer ordinary"]
			main := observations["main fixed"]
			if peer.accepted != main.accepted || peer.unsettled != main.unsettled ||
				peer.exhausted != main.exhausted || peer.markers != main.markers || peer.sentinel != main.sentinel {
				t.Fatalf("main/peer marker proof divergence: peer=%+v main=%+v", peer, main)
			}
			if peer.accepted != test.wantAccepted || peer.unsettled == test.wantAccepted ||
				peer.exhausted != test.wantExhausted || peer.markers != test.wantMarkers || !peer.sentinel {
				t.Fatalf("marker proof result = %+v, want accepted:%v exhausted:%v markers:%d sentinel:true",
					peer, test.wantAccepted, test.wantExhausted, test.wantMarkers)
			}
			if !main.fixed || main.usage.entries != spoolFixedEntryEnvelope ||
				main.usage.nameBytes != spoolFixedNameEnvelope || main.usage.readBytes != spoolFixedReadEnvelope {
				t.Fatalf("main fixed proof accounting = fixed:%v usage:%+v, want envelopes entries:%d names:%d reads:%d",
					main.fixed, main.usage, spoolFixedEntryEnvelope, spoolFixedNameEnvelope, spoolFixedReadEnvelope)
			}
			if test.fixture == "64" || test.fixture == "65" {
				count := uint64(maximumStorageTempAttempts)
				finalJournalRecheck := uint64(1)
				if test.fixture == "65" {
					count++
					finalJournalRecheck = 0
				}
				processed := uint64(maximumStorageTempAttempts)
				journalNameBytes := uint64(len(rootTempJournalDirectoryName))
				nameBytes := uint64(markerNameBytes)
				wantEntries := uint64(2) + count + 4*processed + finalJournalRecheck
				wantNames := journalNameBytes + maximumStorageNameBytes + count*nameBytes +
					processed*(3*nameBytes+journalNameBytes) + finalJournalRecheck*journalNameBytes
				if peer.fixed || peer.usage.entries != wantEntries || peer.usage.nameBytes != wantNames {
					t.Fatalf("ordinary %s sentinel accounting = fixed:%v usage:%+v, want entries:%d names:%d",
						test.fixture, peer.fixed, peer.usage, wantEntries, wantNames)
				}
			}
		})
	}
}

func seedRootTempProofFixture(t *testing.T, home gchome.ProductUsageHome, root *storageRoot, fixture string) int {
	t.Helper()
	backend, ok := root.backend.(*unixStorageDirectory)
	if !ok {
		t.Fatal("root-temp proof fixture requires Unix storage")
	}
	journal, err := backend.openRootTempJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(home.Root(), rootTempJournalDirectoryName)
	var journalStat unix.Stat_t
	if err := unix.Stat(journalPath, &journalStat); err != nil {
		t.Fatal(err)
	}
	device := unixStatDevice(journalStat)
	writeMarker := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(journalPath, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	markerName := func(index uint64) string {
		return fmt.Sprintf(".pm-tmp-%x-%x", os.Getpid(), uint64(0xe00)+index)
	}
	markerNameBytes := len(markerName(0))
	switch fixture {
	case "intent":
		writeMarker(markerName(0), nil)
	case "bound":
		name := markerName(0)
		data, err := encodeBoundRootTempJournalMarker(name, recordIncarnation{dev: device, ino: 1})
		if err != nil {
			t.Fatal(err)
		}
		writeMarker(name, data)
	case "malformed":
		writeMarker(markerName(0), []byte("malformed"))
	case "live":
		name := markerName(0)
		tempPath := filepath.Join(home.Root(), name)
		if err := os.WriteFile(tempPath, []byte("live bound temp"), 0o600); err != nil {
			t.Fatal(err)
		}
		var tempStat unix.Stat_t
		if err := unix.Lstat(tempPath, &tempStat); err != nil {
			t.Fatal(err)
		}
		data, err := encodeBoundRootTempJournalMarker(name, recordIncarnation{
			dev: unixStatDevice(tempStat), ino: unixStatInode(tempStat),
		})
		if err != nil {
			t.Fatal(err)
		}
		writeMarker(name, data)
	case "64", "65":
		count := maximumStorageTempAttempts
		if fixture == "65" {
			count++
		}
		for index := 0; index < count; index++ {
			name := markerName(uint64(index))
			if len(name) != markerNameBytes {
				t.Fatalf("marker name width changed at %d: %q", index, name)
			}
			data, err := encodeBoundRootTempJournalMarker(name, recordIncarnation{dev: device, ino: uint64(index + 1)})
			if err != nil {
				t.Fatal(err)
			}
			writeMarker(name, data)
		}
	default:
		t.Fatalf("unknown root-temp proof fixture %q", fixture)
	}
	return markerNameBytes
}

type rootTempCrashCase struct {
	name          string
	point         string
	markerPresent bool
	markerState   rootTempJournalMarkerState
	tempPresent   bool
	tempData      string
	targetPresent bool
	manual        bool
}

type rootTempCrashArtifacts struct {
	markerPresent bool
	markerState   rootTempJournalMarkerState
	markerName    string
	tempPresent   bool
	tempData      string
	targetPresent bool
	targetData    string
}

func TestRootAtomicWriterCrashReplayAtEveryProtocolOrdinal(t *testing.T) {
	const payload = "sensitive crash payload"
	tests := []rootTempCrashCase{
		{name: "new journal directory sync", point: "step-01"},
		{name: "journal link root sync", point: "step-02"},
		{name: "marker create", point: "step-03"},
		{name: "marker file sync", point: "step-04", markerPresent: true, markerState: rootTempJournalMarkerIntent},
		{name: "marker journal sync", point: "step-05", markerPresent: true, markerState: rootTempJournalMarkerIntent},
		{name: "root temp create", point: "root-temp-create", markerPresent: true, markerState: rootTempJournalMarkerIntent, tempPresent: true, manual: true},
		{name: "root temp root sync", point: "step-06", markerPresent: true, markerState: rootTempJournalMarkerIntent, tempPresent: true, manual: true},
		{name: "bound marker write", point: "step-07", markerPresent: true, markerState: rootTempJournalMarkerIntent, tempPresent: true, manual: true},
		{name: "bound marker file sync", point: "step-08", markerPresent: true, markerState: rootTempJournalMarkerBound, tempPresent: true},
		{name: "bound marker journal sync", point: "step-09", markerPresent: true, markerState: rootTempJournalMarkerBound, tempPresent: true},
		{name: "payload write", point: "step-10", markerPresent: true, markerState: rootTempJournalMarkerBound, tempPresent: true},
		{name: "payload file sync", point: "step-11", markerPresent: true, markerState: rootTempJournalMarkerBound, tempPresent: true, tempData: payload},
		{name: "target rename", point: "step-12", markerPresent: true, markerState: rootTempJournalMarkerBound, tempPresent: true, tempData: payload},
		{name: "target root sync", point: "step-13", markerPresent: true, markerState: rootTempJournalMarkerBound, targetPresent: true},
		{name: "marker delete", point: "step-14", markerPresent: true, markerState: rootTempJournalMarkerBound, targetPresent: true},
		{name: "first durable temp absence sync", point: "step-15", markerPresent: true, markerState: rootTempJournalMarkerBound, targetPresent: true},
		{name: "rechecked durable temp absence sync", point: "step-16", markerPresent: true, markerState: rootTempJournalMarkerBound, targetPresent: true},
		{name: "marker journal sync", point: "step-17", targetPresent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			ensureMetricsRoot(t, home)
			ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRootAtomicWriterCrashHelper$", "--",
				"--productmetrics-root-temp-crash", home.Home().Path(), test.point)
			output, runErr := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("crash helper %s timed out: %v\n%s", test.point, ctx.Err(), output)
			}
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != rootTempCrashExitCode {
				t.Fatalf("crash helper %s = %v, want exit %d\n%s", test.point, runErr, rootTempCrashExitCode, output)
			}

			before := observeRootTempCrashArtifacts(t, home, configFileName)
			assertRootTempCrashArtifacts(t, before, test, payload)
			settled, replayErr := replayRootTempJournalAfterCrash(t, home)
			if test.manual {
				if settled || !errors.Is(replayErr, errUnsettledRootTempJournal) {
					t.Fatalf("%s replay = settled:%v err:%v, want manual pending", test.point, settled, replayErr)
				}
				after := observeRootTempCrashArtifacts(t, home, configFileName)
				assertRootTempCrashArtifacts(t, after, test, payload)
				return
			}
			if replayErr != nil || !settled {
				t.Fatalf("%s replay = settled:%v err:%v", test.point, settled, replayErr)
			}
			after := observeRootTempCrashArtifacts(t, home, configFileName)
			if after.markerPresent || after.tempPresent || after.targetPresent != test.targetPresent ||
				after.targetPresent && after.targetData != payload {
				t.Fatalf("%s settled artifacts = %+v", test.point, after)
			}
		})
	}
}

func TestRootAtomicWriterCrashHelper(t *testing.T) {
	homePath, point, ok := parseRootTempCrashHelperArgs(os.Args)
	if !ok {
		return
	}
	if err := os.Setenv("GC_HOME", homePath); err != nil {
		t.Fatal(err)
	}
	home, err := gchome.InspectProductUsageHome(gchome.ResolveReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	armed := false
	ordinal := 0
	tempPath := ""
	hooks := storageTestHooks{
		beforeTempFileCreate: func(path string) {
			if !armed {
				return
			}
			tempPath = path
		},
		beforeMetadataAttempt: func(path string) error {
			if armed && point == "root-temp-create" && tempPath != "" && path == tempPath {
				os.Exit(rootTempCrashExitCode)
			}
			return nil
		},
		beforeStep: func(storageStep) error {
			if !armed {
				return nil
			}
			ordinal++
			if point == fmt.Sprintf("step-%02d", ordinal) {
				os.Exit(rootTempCrashExitCode)
			}
			return nil
		},
	}
	root, err := openStorageRootMutableWithHooks(home, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	armed = true
	if _, err := root.writeFileAtomicOutcome(configFileName, []byte("sensitive crash payload")); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("root-temp crash point %q was not reached (observed %d steps)", point, ordinal)
}

func parseRootTempCrashHelperArgs(args []string) (string, string, bool) {
	if len(args) < 5 {
		return "", "", false
	}
	suffix := args[len(args)-4:]
	if suffix[0] != "--" || suffix[1] != "--productmetrics-root-temp-crash" ||
		!filepath.IsAbs(suffix[2]) || filepath.Clean(suffix[2]) != suffix[2] || !validRootTempCrashPoint(suffix[3]) {
		return "", "", false
	}
	return suffix[2], suffix[3], true
}

func validRootTempCrashPoint(point string) bool {
	if point == "root-temp-create" {
		return true
	}
	if !strings.HasPrefix(point, "step-") || len(point) != len("step-00") {
		return false
	}
	return point >= "step-01" && point <= "step-17"
}

func observeRootTempCrashArtifacts(t *testing.T, home gchome.ProductUsageHome, targetName string) rootTempCrashArtifacts {
	t.Helper()
	artifacts := rootTempCrashArtifacts{}
	journalPath := filepath.Join(home.Root(), rootTempJournalDirectoryName)
	entries, err := os.ReadDir(journalPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) > 1 {
		t.Fatalf("crash left %d journal markers: %v", len(entries), entries)
	}
	if len(entries) == 1 {
		artifacts.markerPresent = true
		artifacts.markerName = entries[0].Name()
		data, readErr := os.ReadFile(filepath.Join(journalPath, artifacts.markerName))
		if readErr != nil {
			t.Fatal(readErr)
		}
		evidence, decodeErr := decodeRootTempJournalMarker(artifacts.markerName, data)
		if decodeErr != nil {
			t.Fatalf("crash marker %q is malformed: %x: %v", artifacts.markerName, data, decodeErr)
		}
		artifacts.markerState = evidence.state
	}
	rootEntries, err := os.ReadDir(home.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range rootEntries {
		if !canonicalStorageTempName(entry.Name()) {
			continue
		}
		if artifacts.tempPresent {
			t.Fatalf("crash left multiple root temps")
		}
		artifacts.tempPresent = true
		data, readErr := os.ReadFile(filepath.Join(home.Root(), entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		artifacts.tempData = string(data)
		if artifacts.markerPresent && artifacts.markerName != entry.Name() {
			t.Fatalf("crash marker %q maps a different temp %q", artifacts.markerName, entry.Name())
		}
	}
	target, err := os.ReadFile(filepath.Join(home.Root(), targetName))
	if err == nil {
		artifacts.targetPresent = true
		artifacts.targetData = string(target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return artifacts
}

func assertRootTempCrashArtifacts(t *testing.T, got rootTempCrashArtifacts, want rootTempCrashCase, payload string) {
	t.Helper()
	if got.markerPresent != want.markerPresent || got.markerPresent && got.markerState != want.markerState ||
		got.tempPresent != want.tempPresent || got.tempPresent && got.tempData != want.tempData ||
		got.targetPresent != want.targetPresent || got.targetPresent && got.targetData != payload {
		t.Fatalf("%s crash artifacts = %+v, want marker:%v/%d temp:%v/%q target:%v/%q",
			want.point, got, want.markerPresent, want.markerState, want.tempPresent, want.tempData, want.targetPresent, payload)
	}
}

func replayRootTempJournalAfterCrash(t *testing.T, home gchome.ProductUsageHome) (bool, error) {
	t.Helper()
	root := mustOpenMutableRoot(t, home)
	defer func() { _ = root.Close() }()
	for range 4 {
		state := &spoolSweepState{
			root: root, purgeAll: true, meter: newSpoolWorkMeter(defaultSpoolWorkBudget()),
			seen: make(map[string]struct{}), pruneDirs: make(map[string]*storageDir), failClosedArmed: true,
		}
		state.cleanupRootTempJournal()
		if state.operation != nil {
			return false, state.operation
		}
		if state.journalSettled && !state.mutated && !state.meter.exhausted && state.meter.traversalError == nil {
			return true, nil
		}
		if !state.mutated {
			return false, errors.New("productmetrics: root-temp replay made no progress")
		}
	}
	return false, errors.New("productmetrics: root-temp replay did not converge")
}
