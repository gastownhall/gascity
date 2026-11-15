package resourcecensus

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
)

const (
	// CountsPath is the repository-relative path of the generated per-file
	// counts file: the only place the source census counts are stored.
	CountsPath = "test/test-resource-counts.toml"

	countsVersion = 1

	countsHeader = "# GENERATED FILE - DO NOT EDIT BY HAND.\n" +
		"# Per-file counts for the P0.4 source resource census (owner: ga-cp3hwi).\n" +
		"# Regenerate: " + regenerateCommand + "\n"

	// regenerateCommand runs the generator with `bazel run` from the
	// repository root: `bazel test` would write only into its sandbox.
	regenerateCommand = `GC_TEST_REPO_ROOT="$PWD" bazel run //internal/testpolicy/resourcecensus:resourcecensus_test -- -test.run='^TestRepositoryLedgerMatchesCensusAndDocumentation$' -update`

	regenerateLine = "regenerate with: " + regenerateCommand + " (then review the diff of " + CountsPath + ": each changed line is a ceiling change)"

	// rebaseProtocol is the one-paragraph recovery every census failure ends
	// with. It deliberately avoids the phrases of the census failure lines
	// themselves, which operators and the review autofix match on the log.
	rebaseProtocol = "Rebase protocol for a census conflict (TESTING.md section \"Checked source-level resource ratchets\"): " +
		"(1) discard your edits to bootstrapPolicy (internal/testpolicy/resourcecensus/policy.go), to the per-row count fields of test/test-resources.toml and to the TESTING.md census block, taking main's versions of all three; " +
		"(2) re-apply any [[medium]] or [[reviewed_hermetic_body]] rows you added by hand, in sorted (package_dir, package_name, owner) order, in both policy.go and test/test-resources.toml; " +
		"(3) run " + regenerateCommand + "; " +
		"(4) commit " + CountsPath + " with the result, and never hand-edit or hand-merge it."

	// failureEnding closes every census failure message.
	failureEnding = "\n" + regenerateLine + "\n\n" + rebaseProtocol

	// maxRecordLines caps the per-record lines of one ValidateCounts failure.
	maxRecordLines = 20
)

// renameFile is the rename step of an atomic write; tests replace it to
// inject failures.
var renameFile = os.Rename

// retiredCountKeys are the per-row count keys policy version 3 moved into the
// counts file.
var retiredCountKeys = []string{"baseline_calls", "baseline_files"}

// CountRecord is the census of one resource in one file: the calls governed
// by a counted policy row, and how many of them a Medium declaration owns.
type CountRecord struct {
	Path     string
	Resource Resource
	Calls    int
	Medium   int
	Tagged   bool
}

// countsFailure joins problems into one error that ends with the regenerate
// line and the rebase protocol.
func countsFailure(problems ...string) error {
	return errors.New(strings.Join(problems, "\n") + failureEnding)
}

// removedLedgerFields picks the removed per-row count keys out of the
// undecoded ledger fields.
func removedLedgerFields(fields []string) []string {
	var removed []string
	for _, field := range fields {
		for _, key := range retiredCountKeys {
			if field == key || strings.HasSuffix(field, "."+key) {
				removed = append(removed, field)
			}
		}
	}
	return removed
}

// governedBy reports whether any counted policy row counts the occurrence.
func governedBy(rows []Baseline, occurrence Occurrence) bool {
	for _, row := range rows {
		if row.Resource == occurrence.Resource && scopeContains(row.Scope, occurrence) {
			return true
		}
	}
	return false
}

// countedRows returns every row that counts occurrences: audit, debt and
// small debt.
func countedRows(ledger Ledger) []Baseline {
	rows := make([]Baseline, 0, len(ledger.AuditBaseline)+len(ledger.Debt)+len(ledger.SmallDebt))
	rows = append(rows, ledger.AuditBaseline...)
	rows = append(rows, ledger.Debt...)
	return append(rows, ledger.SmallDebt...)
}

// validCountsPath reports why a path cannot be written to the counts file.
func validCountsPath(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	for _, r := range path {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("path %q cannot be written to %s (it holds a quote, backslash or control character)", path, CountsPath)
		}
	}
	return nil
}

// GenerateCounts derives the per-file records from a scan: one record for each
// file and resource that a counted row governs, sorted bytewise by path then
// resource.
func GenerateCounts(ledger Ledger, census Census) ([]CountRecord, error) {
	rows := countedRows(ledger)
	owned := mediumOwnership(ledger.Medium)
	type key struct {
		path     string
		resource Resource
	}
	byKey := map[key]*CountRecord{}
	for _, occurrence := range census.Occurrences {
		if !governedBy(rows, occurrence) {
			continue
		}
		if err := validCountsPath(occurrence.Path); err != nil {
			return nil, err
		}
		k := key{occurrence.Path, occurrence.Resource}
		record := byKey[k]
		if record == nil {
			record = &CountRecord{Path: occurrence.Path, Resource: occurrence.Resource, Tagged: occurrence.Tagged}
			byKey[k] = record
		}
		record.Calls++
		if owned.owns(occurrence) {
			record.Medium++
		}
	}
	records := make([]CountRecord, 0, len(byKey))
	for _, record := range byKey {
		records = append(records, *record)
	}
	sortCountRecords(records)
	return records, nil
}

func sortCountRecords(records []CountRecord) {
	sort.Slice(records, func(i, j int) bool { return countRecordLess(records[i], records[j]) })
}

func countRecordLess(left, right CountRecord) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	return left.Resource < right.Resource
}

// RenderCounts renders records in the one canonical form of the counts file.
func RenderCounts(records []CountRecord) string {
	var out strings.Builder
	out.WriteString(countsHeader)
	fmt.Fprintf(&out, "version = %d\n", countsVersion)
	for _, record := range records {
		out.WriteString("\n[[count]]\n")
		fmt.Fprintf(&out, "path = \"%s\"\n", record.Path)
		fmt.Fprintf(&out, "resource = \"%s\"\n", record.Resource)
		fmt.Fprintf(&out, "calls = %d\n", record.Calls)
		if record.Medium != 0 {
			fmt.Fprintf(&out, "medium = %d\n", record.Medium)
		}
		if record.Tagged {
			out.WriteString("tagged = true\n")
		}
	}
	return out.String()
}

type countsFile struct {
	Version int           `toml:"version"`
	Count   []countsEntry `toml:"count"`
}

type countsEntry struct {
	Path     string `toml:"path"`
	Resource string `toml:"resource"`
	Calls    int    `toml:"calls"`
	Medium   int    `toml:"medium"`
	Tagged   *bool  `toml:"tagged"`
}

// ParseCounts reads a counts file strictly: every field is checked and the
// bytes must be exactly what RenderCounts writes for the parsed records.
func ParseCounts(data []byte) ([]CountRecord, error) {
	records, canonical, err := parseCounts(data)
	if err != nil {
		return nil, err
	}
	if !canonical {
		return nil, errNonCanonical
	}
	return records, nil
}

var errNonCanonical = fmt.Errorf("%s is non-canonical: its bytes differ from what -update writes", CountsPath)

// parseCounts checks everything but the exact bytes, so ValidateCounts can
// compare counts before it judges them. canonical reports whether the bytes
// equal the rendered records.
func parseCounts(data []byte) (records []CountRecord, canonical bool, err error) {
	if hasConflictMarkers(data) {
		return nil, false, fmt.Errorf("%s has merge conflict markers: take main's copy of %s and regenerate it with %s; never hand-merge it", CountsPath, CountsPath, regenerateCommand)
	}
	var file countsFile
	metadata, err := toml.Decode(string(data), &file)
	if err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", CountsPath, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return nil, false, fmt.Errorf("%s has an unknown key: %s", CountsPath, strings.Join(keys, ", "))
	}
	if file.Version != countsVersion {
		return nil, false, fmt.Errorf("%s version = %d, want %d", CountsPath, file.Version, countsVersion)
	}
	for index, entry := range file.Count {
		record, err := entry.record()
		if err != nil {
			return nil, false, fmt.Errorf("%s record %d (path=%s resource=%s): %w", CountsPath, index+1, entry.Path, entry.Resource, err)
		}
		if index > 0 {
			previous := records[index-1]
			switch {
			case previous.Path == record.Path && previous.Resource == record.Resource:
				return nil, false, fmt.Errorf("%s has a duplicate record: path=%s resource=%s", CountsPath, record.Path, record.Resource)
			case countRecordLess(record, previous):
				return nil, false, fmt.Errorf("%s records are not sorted by path then resource: path=%s resource=%s follows path=%s resource=%s", CountsPath, record.Path, record.Resource, previous.Path, previous.Resource)
			}
		}
		records = append(records, record)
	}
	return records, RenderCounts(records) == string(data), nil
}

func (entry countsEntry) record() (CountRecord, error) {
	if err := validCountsPath(entry.Path); err != nil {
		return CountRecord{}, err
	}
	resource := Resource(entry.Resource)
	if _, known := knownResources[resource]; !known {
		return CountRecord{}, fmt.Errorf("unknown resource %q", entry.Resource)
	}
	if entry.Calls < 1 {
		return CountRecord{}, fmt.Errorf("calls = %d, want at least 1", entry.Calls)
	}
	if entry.Medium < 0 || entry.Medium > entry.Calls {
		return CountRecord{}, fmt.Errorf("medium = %d, want 0 through calls (%d)", entry.Medium, entry.Calls)
	}
	if entry.Tagged != nil && !*entry.Tagged {
		return CountRecord{}, errors.New("tagged = false is not written: omit the key for an untagged file")
	}
	return CountRecord{Path: entry.Path, Resource: resource, Calls: entry.Calls, Medium: entry.Medium, Tagged: entry.Tagged != nil}, nil
}

// hasConflictMarkers reports whether any line is a git merge conflict marker.
func hasConflictMarkers(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("<<<<<<<")) || bytes.Equal(line, []byte("=======")) || bytes.HasPrefix(line, []byte(">>>>>>>")) {
			return true
		}
	}
	return false
}

// DerivedCount sums the records a scope contains for resource. It equals
// Census.Count for every counted row.
func DerivedCount(records []CountRecord, scope Scope, resource Resource) Count {
	var count Count
	for _, record := range records {
		if record.Resource != resource || !scopeContains(scope, Occurrence{Path: record.Path, Resource: record.Resource, Tagged: record.Tagged}) {
			continue
		}
		count.Calls += record.Calls
		count.Files++
	}
	return count
}

// DerivedSmallCount is DerivedCount without the calls a Medium declaration
// owns; a file whose calls are all Medium-owned is not a Small file. It
// equals Census.SmallCount for every small_debt row.
func DerivedSmallCount(records []CountRecord, scope Scope, resource Resource) Count {
	var count Count
	for _, record := range records {
		if record.Resource != resource || !scopeContains(scope, Occurrence{Path: record.Path, Resource: record.Resource, Tagged: record.Tagged}) {
			continue
		}
		if small := record.Calls - record.Medium; small > 0 {
			count.Calls += small
			count.Files++
		}
	}
	return count
}

// ValidateCounts compares a stored counts file with a scan. Growth fails the
// build; so does shrinkage, until the file is regenerated to bank it. The
// canonical-bytes check runs last, so a count problem is never hidden behind a
// formatting one.
func ValidateCounts(ledger Ledger, census Census, data []byte) error {
	stored, canonical, err := parseCounts(data)
	if err != nil {
		return countsFailure(err.Error())
	}
	scanned, err := GenerateCounts(ledger, census)
	if err != nil {
		return countsFailure(err.Error())
	}

	problems := aggregateCountProblems(ledger, census, stored)
	problems = append(problems, recordCountProblems(ledger, stored, scanned)...)
	if !canonical {
		problems = append(problems, errNonCanonical.Error())
	}
	if len(problems) == 0 {
		return nil
	}
	return countsFailure(problems...)
}

// aggregateCountProblems compares each counted row's stored total with the
// scan, so a failure leads with the row-level headline the old baselines gave.
func aggregateCountProblems(ledger Ledger, census Census, stored []CountRecord) []string {
	var problems []string
	compare := func(label string, row Baseline, actual, baseline Count) {
		switch {
		case actual.Calls > baseline.Calls || actual.Files > baseline.Files:
			problems = append(problems, fmt.Sprintf("%s census grew: scope=%s resource=%s calls=%d (baseline %d), files=%d (baseline %d)", label, row.Scope, row.Resource, actual.Calls, baseline.Calls, actual.Files, baseline.Files))
		case actual.Calls < baseline.Calls || actual.Files < baseline.Files:
			problems = append(problems, fmt.Sprintf("%s census baseline is stale: scope=%s resource=%s calls=%d (baseline %d), files=%d (baseline %d)", label, row.Scope, row.Resource, actual.Calls, baseline.Calls, actual.Files, baseline.Files))
		}
	}
	for _, row := range append(append([]Baseline(nil), ledger.AuditBaseline...), ledger.Debt...) {
		compare("source resource", row, census.Count(row.Scope, row.Resource), DerivedCount(stored, row.Scope, row.Resource))
	}
	for _, row := range ledger.SmallDebt {
		compare("Small resource", row, census.SmallCount(row.Scope, row.Resource, ledger.Medium), DerivedSmallCount(stored, row.Scope, row.Resource))
	}
	return problems
}

// recordCountProblems compares stored and scanned records file by file,
// capped at maxRecordLines lines.
func recordCountProblems(ledger Ledger, stored, scanned []CountRecord) []string {
	type key struct {
		path     string
		resource Resource
	}
	index := func(records []CountRecord) map[key]CountRecord {
		byKey := make(map[key]CountRecord, len(records))
		for _, record := range records {
			byKey[key{record.Path, record.Resource}] = record
		}
		return byKey
	}
	storedByKey, scannedByKey := index(stored), index(scanned)
	keys := make([]CountRecord, 0, len(storedByKey)+len(scannedByKey))
	for k := range storedByKey {
		keys = append(keys, CountRecord{Path: k.path, Resource: k.resource})
	}
	for k := range scannedByKey {
		if _, both := storedByKey[k]; !both {
			keys = append(keys, CountRecord{Path: k.path, Resource: k.resource})
		}
	}
	sortCountRecords(keys)

	small := func(record CountRecord, present bool) int {
		if !present || !governedBy(ledger.SmallDebt, Occurrence{Path: record.Path, Resource: record.Resource, Tagged: record.Tagged}) {
			return 0
		}
		return record.Calls - record.Medium
	}
	var lines []string
	for _, k := range keys {
		was, hadStored := storedByKey[key{k.Path, k.Resource}]
		now, hasScanned := scannedByKey[key{k.Path, k.Resource}]
		where := fmt.Sprintf("path=%s resource=%s", k.Path, k.Resource)
		switch {
		case now.Calls > was.Calls:
			lines = append(lines, fmt.Sprintf("source resource census grew: %s calls=%d (baseline %d)", where, now.Calls, was.Calls))
		case now.Calls < was.Calls:
			lines = append(lines, fmt.Sprintf("source resource census baseline is stale: %s calls=%d (baseline %d)", where, now.Calls, was.Calls))
		}
		if hadStored && hasScanned && now.Tagged != was.Tagged {
			lines = append(lines, fmt.Sprintf("tagged mismatch: %s tagged=%t (baseline %t)", where, now.Tagged, was.Tagged))
		}
		switch nowSmall, wasSmall := small(now, hasScanned), small(was, hadStored); {
		case nowSmall > wasSmall:
			lines = append(lines, fmt.Sprintf("Small resource census grew: %s small=%d (baseline %d)", where, nowSmall, wasSmall))
		case nowSmall < wasSmall:
			lines = append(lines, fmt.Sprintf("Small resource census baseline is stale: %s small=%d (baseline %d)", where, nowSmall, wasSmall))
		}
	}
	if len(lines) > maxRecordLines {
		more := len(lines) - maxRecordLines
		lines = append(lines[:maxRecordLines], fmt.Sprintf("... and %d more", more))
	}
	return lines
}

// UpdateGenerated rewrites the counts file and the TESTING.md census block
// from a scan, after the ledger's structure has been checked.
func UpdateGenerated(root string, ledger Ledger, census Census) error {
	return updateGenerated(bootstrapPolicy, root, ledger, census)
}

func updateGenerated(policy Ledger, root string, ledger Ledger, census Census) error {
	if err := validateAgainstPolicy(policy, ledger, census); err != nil {
		return err
	}
	records, err := GenerateCounts(ledger, census)
	if err != nil {
		return fmt.Errorf("generate %s: %w", CountsPath, err)
	}
	docPath := filepath.Join(root, "TESTING.md")
	doc, err := os.ReadFile(docPath)
	if err != nil {
		return fmt.Errorf("read TESTING.md: %w", err)
	}
	updatedDoc, err := ReplaceMarkdownBlock(string(doc), RenderMarkdown(ledger))
	if err != nil {
		return err
	}
	if err := writeIfChanged(filepath.Join(root, filepath.FromSlash(CountsPath)), []byte(RenderCounts(records))); err != nil {
		return err
	}
	return writeIfChanged(docPath, []byte(updatedDoc))
}

// writeIfChanged atomically replaces name with content, through a temp file in
// the same directory, unless the bytes already match.
func writeIfChanged(name string, content []byte) error {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(name); err == nil {
		mode = info.Mode().Perm()
		if existing, readErr := os.ReadFile(name); readErr == nil && bytes.Equal(existing, content) {
			return nil
		}
	}
	if err := writeAtomically(name, content, mode); err != nil {
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
			return fmt.Errorf("write %s: %w (the source tree is read-only here: regenerate from a writable checkout with %s)", name, err, regenerateCommand)
		}
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func writeAtomically(name string, content []byte, mode fs.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	fail := func(err error) error {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if _, err := temp.Write(content); err != nil {
		return fail(err)
	}
	if err := temp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := temp.Close(); err != nil {
		return fail(err)
	}
	if err := renameFile(tempName, name); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	return nil
}
