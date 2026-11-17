package doctor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	// doltArchiveFooterSize is the length in bytes of a Dolt archive (.darc)
	// format v3 footer, the last bytes of the file.
	doltArchiveFooterSize = 220

	// doltArchiveDictWarnDefault is the per-archive dictionary count above
	// which the check warns. Dolt's GC duplicates a zstd dictionary on every
	// reader-cache miss once an archive holds more than 256 of them; 200
	// leaves headroom to act before that.
	doltArchiveDictWarnDefault = int64(200)

	doltArchiveDictWarnEnv = "GC_DOLT_ARCHIVE_DICT_WARN_COUNT"

	// doltArchiveVersion is the only archive format version the footer
	// parser understands.
	doltArchiveVersion = 3
	// doltArchiveSignature is the 7-byte magic that ends every archive.
	doltArchiveSignature = "DOLTARC"
	// doltArchiveVersionOffset is the footer offset of the version byte; the
	// signature follows it. Both v2 and v3 footers end with the version byte
	// and signature, so this offset is right inside the 220-byte window for
	// either.
	doltArchiveVersionOffset = doltArchiveFooterSize - 1 - len(doltArchiveSignature)
)

// DoltArchiveDictCheck warns when a managed Dolt archive holds more zstd
// dictionaries than Dolt's reader cache can keep.
//
// Registered once per city — the managed Dolt server is shared across all rigs.
// The check only reads the last 220 bytes of each *.darc file.
type DoltArchiveDictCheck struct {
	cityPath        string
	skip            bool
	applicableKnown bool
	applicable      bool
	scopeRoots      []string
}

// NewDoltArchiveDictCheck creates a Dolt archive dictionary check.
func NewDoltArchiveDictCheck(cityPath string, skip bool) *DoltArchiveDictCheck {
	return &DoltArchiveDictCheck{cityPath: cityPath, skip: skip}
}

// NewDoltArchiveDictCheckForConfig creates a Dolt archive dictionary check
// using a preloaded city config.
func NewDoltArchiveDictCheckForConfig(cityPath string, skip bool, cfg *config.City, cfgErr error) *DoltArchiveDictCheck {
	return &DoltArchiveDictCheck{
		cityPath:        cityPath,
		skip:            skip,
		applicableKnown: true,
		applicable:      ManagedLocalDoltChecksApplicableForConfig(cityPath, cfg, cfgErr),
		scopeRoots:      managedDoltScopeRootsForConfig(cityPath, cfg, cfgErr),
	}
}

func (c *DoltArchiveDictCheck) managedApplicable() bool {
	if c.applicableKnown {
		return c.applicable
	}
	return managedLocalDoltChecksApplicable(c.cityPath)
}

// Name returns the check identifier.
func (c *DoltArchiveDictCheck) Name() string { return "dolt-archive-dicts" }

// doltArchiveInfo is the dictionary count of one readable archive.
type doltArchiveInfo struct {
	db    string
	rel   string // path relative to <db>/.dolt, slash-separated
	dicts int64
}

// doltArchiveProblem is an archive that could not be counted.
type doltArchiveProblem struct {
	db     string
	rel    string
	reason string
}

// doltArchiveDBStat is the per-database summary shown in the details.
type doltArchiveDBStat struct {
	db       string
	archives int
	largest  int64
}

// Run reads the footer of every *.darc archive under each managed Dolt
// database's noms and noms/oldgen directories and warns when one holds more
// zstd dictionaries than the threshold. An archive that cannot be read, or
// whose footer is not a format Run understands, is reported rather than
// counted as zero.
func (c *DoltArchiveDictCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip || !c.managedApplicable() {
		r.Status = StatusOK
		r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		return r
	}

	limit := doltEnvByteThreshold(doltArchiveDictWarnEnv, doltArchiveDictWarnDefault)

	targets, unresolved := managedLocalDoltScanTargets(c.cityPath)
	if c.applicableKnown {
		targets, unresolved = managedLocalDoltScanTargetsForScopeRoots(c.cityPath, c.scopeRoots)
	}
	if len(targets) == 0 {
		r.Status = StatusOK
		if unresolved {
			r.Message = "skipped (dolt target unresolved)"
		} else {
			r.Message = "skipped (file backend, external dolt endpoint, or GC_DOLT=skip)"
		}
		return r
	}

	dataDir := resolveManagedDoltDataDir(c.cityPath)

	var (
		archives []doltArchiveInfo
		problems []doltArchiveProblem
		dbStats  []doltArchiveDBStat
	)
	for _, target := range targets {
		stat := doltArchiveDBStat{db: doltArchiveDBLabel(target.Database)}
		for _, sub := range []string{"noms", filepath.Join("noms", "oldgen")} {
			dir := filepath.Join(target.ScanRoot, sub)
			names, err := doltArchiveNames(dir)
			if err != nil {
				problems = append(problems, doltArchiveProblem{
					db:     stat.db,
					rel:    filepath.ToSlash(sub),
					reason: err.Error(),
				})
				continue
			}
			for _, name := range names {
				if ctx.Canceled() {
					r.Status = StatusWarning
					r.Message = "dolt-archive-dicts: scan canceled before it finished"
					return r
				}
				rel := filepath.ToSlash(filepath.Join(sub, name))
				dicts, err := readDoltArchiveDicts(filepath.Join(dir, name))
				switch {
				case errors.Is(err, fs.ErrNotExist):
					// Deleted between the listing and the open (a GC does this).
					continue
				case err != nil:
					problems = append(problems, doltArchiveProblem{db: stat.db, rel: rel, reason: err.Error()})
					continue
				}
				archives = append(archives, doltArchiveInfo{db: stat.db, rel: rel, dicts: dicts})
				stat.archives++
				if dicts > stat.largest {
					stat.largest = dicts
				}
			}
		}
		dbStats = append(dbStats, stat)
	}

	var over []doltArchiveInfo
	for _, a := range archives {
		if a.dicts > limit {
			over = append(over, a)
		}
	}
	sort.Slice(over, func(i, j int) bool {
		if over[i].dicts != over[j].dicts {
			return over[i].dicts > over[j].dicts
		}
		if over[i].db != over[j].db {
			return over[i].db < over[j].db
		}
		return over[i].rel < over[j].rel
	})
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].db != problems[j].db {
			return problems[i].db < problems[j].db
		}
		return problems[i].rel < problems[j].rel
	})

	details := []string{fmt.Sprintf("scan path: %s", dataDir)}
	for _, s := range dbStats {
		details = append(details, fmt.Sprintf("database %s: %d archive(s), largest holds %d dictionaries", s.db, s.archives, s.largest))
	}
	for _, a := range over {
		details = append(details, fmt.Sprintf("over limit: %s %s holds %d dictionaries", a.db, a.rel, a.dicts))
	}
	for _, p := range problems {
		details = append(details, fmt.Sprintf("unreadable: %s %s: %s", p.db, p.rel, p.reason))
	}
	details = append(details, fmt.Sprintf("warn threshold: %s (default %d, current %d)", doltArchiveDictWarnEnv, doltArchiveDictWarnDefault, limit))
	r.Details = details

	if len(over) > 0 || len(problems) > 0 {
		r.Status = StatusWarning
		var parts []string
		if len(over) > 0 {
			worst := over[0]
			msg := fmt.Sprintf("dolt archive %s %s holds %d zstd dictionaries (limit %d)", worst.db, worst.rel, worst.dicts, limit)
			if len(over) > 1 {
				msg += fmt.Sprintf(" and %d more archive(s) are over the limit", len(over)-1)
			}
			parts = append(parts, msg)
			r.FixHint = "Dolt's GC duplicates a zstd dictionary on every reader-cache miss once an archive holds more than 256 " +
				"(dolthub/dolt#12177), pinning about 137 KB of C memory each until the GC ends. " +
				"A GC run with --archive-level=0 rewrites archives as plain table files."
		}
		if len(problems) > 0 {
			first := problems[0]
			parts = append(parts, fmt.Sprintf("%d archive(s) could not be read (first: %s %s: %s)", len(problems), first.db, first.rel, first.reason))
		}
		r.Message = strings.Join(parts, "; ")
		return r
	}

	r.Status = StatusOK
	if len(archives) == 0 {
		r.Message = fmt.Sprintf("dolt-archive-dicts: no archives found (%d database(s) scanned)", len(dbStats))
		return r
	}
	var largest doltArchiveInfo
	for _, a := range archives {
		if a.dicts > largest.dicts || largest.rel == "" {
			largest = a
		}
	}
	r.Message = fmt.Sprintf("dolt-archive-dicts: largest archive holds %d zstd dictionaries (%s; %d archive(s) across %d database(s))",
		largest.dicts, largest.db, len(archives), len(dbStats))
	return r
}

// CanFix returns false — the check never modifies Dolt data.
func (c *DoltArchiveDictCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DoltArchiveDictCheck) Fix(_ *CheckContext) error { return nil }

func doltArchiveDBLabel(db string) string {
	if label := strings.TrimSpace(db); label != "" {
		return label
	}
	return "managed dolt data"
}

// doltArchiveNames returns the sorted names of the *.darc entries of dir. A
// directory that does not exist has none.
func doltArchiveNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".darc") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// readDoltArchiveDicts returns the number of zstd dictionaries in the archive
// at path, reading only its last 220 bytes.
func readDoltArchiveDicts(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from listing the managed Dolt data directory
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("not a regular file")
	}
	size := info.Size()
	if size < doltArchiveFooterSize {
		return 0, fmt.Errorf("file is shorter than the %d-byte footer (%d bytes)", doltArchiveFooterSize, size)
	}
	footer := make([]byte, doltArchiveFooterSize)
	n, err := f.ReadAt(footer, size-doltArchiveFooterSize)
	if err != nil {
		// ReadAt may report io.EOF together with a full read at the file's end.
		if n != doltArchiveFooterSize || !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("read footer: %w", err)
		}
	}
	return parseDoltArchiveFooter(footer, size)
}

// parseDoltArchiveFooter returns the zstd dictionary count recorded in a v3
// archive footer: the byte span count minus the chunk count. fileSize is the
// size of the whole file. The first inconsistency found is returned as an
// error; a footer is never guessed at.
//
// Footer layout, big-endian: index length u64, byte span count u32, chunk
// count u32, metadata length u32, 192 reserved bytes, version byte, "DOLTARC".
func parseDoltArchiveFooter(footer []byte, fileSize int64) (int64, error) {
	if fileSize < doltArchiveFooterSize || len(footer) != doltArchiveFooterSize {
		return 0, fmt.Errorf("file is shorter than the %d-byte footer (%d bytes)", doltArchiveFooterSize, fileSize)
	}
	if string(footer[doltArchiveVersionOffset+1:]) != doltArchiveSignature {
		return 0, errors.New("bad signature")
	}
	if v := footer[doltArchiveVersionOffset]; v != doltArchiveVersion {
		return 0, fmt.Errorf("unsupported archive format version %d", v)
	}
	indexLen := binary.BigEndian.Uint64(footer[0:8])
	spans := uint64(binary.BigEndian.Uint32(footer[8:12]))
	chunks := uint64(binary.BigEndian.Uint32(footer[12:16]))
	metaLen := uint64(binary.BigEndian.Uint32(footer[16:20]))

	// The index is four sections of spans*8, chunks*8, chunks*8 and chunks*12
	// bytes. Checking this first keeps the size check below free of overflow.
	if indexLen != spans*8+chunks*8+chunks*8+chunks*12 {
		return 0, errors.New("index length does not match span and chunk counts")
	}
	if spans < chunks {
		return 0, errors.New("fewer byte spans than chunks")
	}
	if indexLen+metaLen+doltArchiveFooterSize > uint64(fileSize) {
		return 0, errors.New("index and metadata overrun the file")
	}
	return int64(spans - chunks), nil
}
