package doctor

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// darcFooter builds a 220-byte archive footer. Field layout (big-endian):
// indexLength u64, byteSpanCount u32, chunkCount u32, metadataLength u32,
// 192 reserved bytes, version, 7-byte signature.
func darcFooter(indexLen uint64, spans, chunks, metaLen uint32, version byte, sig string) []byte {
	f := make([]byte, doltArchiveFooterSize)
	binary.BigEndian.PutUint64(f[0:8], indexLen)
	binary.BigEndian.PutUint32(f[8:12], spans)
	binary.BigEndian.PutUint32(f[12:16], chunks)
	binary.BigEndian.PutUint32(f[16:20], metaLen)
	f[212] = version
	copy(f[213:], sig)
	return f
}

// darcIndexLen is the index length a consistent footer must carry: four
// sections of N*8, M*8, M*8 and M*12 bytes.
func darcIndexLen(spans, chunks uint32) uint64 {
	return uint64(spans)*8 + uint64(chunks)*8 + uint64(chunks)*8 + uint64(chunks)*12
}

// goodFooter is a valid v3 footer for one chunk and the given dictionary count.
func goodFooter(dicts int) (footer []byte, fileSize int64) {
	chunks := uint32(1)
	spans := chunks + uint32(dicts)
	idx := darcIndexLen(spans, chunks)
	return darcFooter(idx, spans, chunks, 0, 3, "DOLTARC"), int64(idx) + doltArchiveFooterSize
}

// writeDarcFile writes a file of fileSize zero bytes whose last 220 bytes are footer.
func writeDarcFile(t *testing.T, path string, footer []byte, fileSize int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, fileSize)
	copy(buf[fileSize-int64(len(footer)):], footer)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeDarcDicts writes a consistent v3 archive holding dicts dictionaries.
func writeDarcDicts(t *testing.T, path string, dicts int) {
	t.Helper()
	footer, size := goodFooter(dicts)
	writeDarcFile(t, path, footer, size)
}

// darcPath returns the path of an archive under a database's noms directory
// (gen "") or its oldgen directory (gen "oldgen").
func darcPath(dir, db, gen, name string) string {
	parts := []string{dir, ".beads", "dolt", db, ".dolt", "noms"}
	if gen != "" {
		parts = append(parts, gen)
	}
	return filepath.Join(append(parts, name)...)
}

func runArchiveDicts(t *testing.T, dir string) *CheckResult {
	t.Helper()
	return NewDoltArchiveDictCheck(dir, false).Run(&CheckContext{})
}

func detailsText(r *CheckResult) string { return strings.Join(r.Details, "\n") }

func TestParseDoltArchiveFooter_RealFooters(t *testing.T) {
	// Both footers were written by Dolt 2.3.3 on a synthetic repo: the 20-byte
	// head, 192 reserved zero bytes, then version 3 and "DOLTARC".
	const tail = "03444f4c54415243"
	cases := []struct {
		name     string
		head     string
		fileSize int64
		want     int64
	}{
		{"F1 no dictionaries", "0000000000000048000000020000000200000018", 701, 0},
		{"F2 one dictionary", "00000000000306e0000015870000158600000018", 6199422, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			footer, err := hex.DecodeString(tc.head + strings.Repeat("00", 192) + tail)
			if err != nil {
				t.Fatal(err)
			}
			if len(footer) != doltArchiveFooterSize {
				t.Fatalf("fixture footer is %d bytes, want %d", len(footer), doltArchiveFooterSize)
			}
			got, err := parseDoltArchiveFooter(footer, tc.fileSize)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Fatalf("dictionaries = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseDoltArchiveFooter_Rejects(t *testing.T) {
	good, goodSize := goodFooter(5)
	mutate := func(f func(b []byte)) []byte {
		b := append([]byte(nil), good...)
		f(b)
		return b
	}
	cases := []struct {
		name     string
		footer   []byte
		fileSize int64
		reason   string
	}{
		{"file shorter than the footer", good, doltArchiveFooterSize - 1, "shorter than the 220-byte footer"},
		{"bad signature", mutate(func(b []byte) { copy(b[213:], "NOTDARC") }), goodSize, "bad signature"},
		{"version 2", mutate(func(b []byte) { b[212] = 2 }), goodSize, "unsupported archive format version 2"},
		{"version 4", mutate(func(b []byte) { b[212] = 4 }), goodSize, "unsupported archive format version 4"},
		{"index tiling mismatch", mutate(func(b []byte) { binary.BigEndian.PutUint64(b[0:8], darcIndexLen(6, 1)+1) }), goodSize, "index length does not match span and chunk counts"},
		{
			"fewer spans than chunks",
			darcFooter(darcIndexLen(1, 2), 1, 2, 0, 3, "DOLTARC"),
			int64(darcIndexLen(1, 2)) + doltArchiveFooterSize,
			"fewer byte spans than chunks",
		},
		{"index and metadata overrun the file", mutate(func(b []byte) { binary.BigEndian.PutUint32(b[16:20], 1000) }), goodSize, "index and metadata overrun the file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDoltArchiveFooter(tc.footer, tc.fileSize)
			if err == nil {
				t.Fatalf("parse accepted a bad footer and returned %d dictionaries", got)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error = %q, want reason %q", err, tc.reason)
			}
		})
	}
}

func TestDoltArchiveDictCheck_Skipped(t *testing.T) {
	r := NewDoltArchiveDictCheck(t.TempDir(), true).Run(&CheckContext{})
	if r.Status != StatusOK || !strings.Contains(r.Message, "skipped") {
		t.Fatalf("status = %d, message = %q, want OK skipped", r.Status, r.Message)
	}
}

func TestDoltArchiveDictCheck_NonManagedDolt(t *testing.T) {
	dir := setupFreshManagedDoltCity(t)
	r := NewDoltArchiveDictCheck(dir, false).Run(&CheckContext{})
	if r.Status != StatusOK || !strings.Contains(r.Message, "skipped") {
		t.Fatalf("status = %d, message = %q, want OK skipped", r.Status, r.Message)
	}
}

func TestDoltArchiveDictCheck_NoArchives(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "manifest"), 64)
	r := runArchiveDicts(t, dir)
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "dolt-archive-dicts: no archives found") {
		t.Errorf("message = %q, want the no-archives text", r.Message)
	}
}

func TestDoltArchiveDictCheck_ThresholdBoundary(t *testing.T) {
	cases := []struct {
		dicts int
		warn  bool
	}{{199, false}, {200, false}, {201, true}}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.dicts), func(t *testing.T) {
			dir := setupManagedDoltCity(t)
			writeDarcDicts(t, darcPath(dir, "hq", "", "aaaa.darc"), tc.dicts)
			r := runArchiveDicts(t, dir)
			if !tc.warn {
				if r.Status != StatusOK {
					t.Fatalf("status = %d, want OK at %d dictionaries; msg = %s", r.Status, tc.dicts, r.Message)
				}
				if !strings.Contains(r.Message, "dolt-archive-dicts") {
					t.Errorf("message = %q, want dolt-archive-dicts prefix", r.Message)
				}
				return
			}
			if r.Status != StatusWarning {
				t.Fatalf("status = %d, want Warning at %d dictionaries; msg = %s", r.Status, tc.dicts, r.Message)
			}
			for _, want := range []string{"hq", "noms/aaaa.darc", "201"} {
				if !strings.Contains(r.Message, want) {
					t.Errorf("message = %q, want it to contain %q", r.Message, want)
				}
			}
		})
	}
}

func TestDoltArchiveDictCheck_EnvOverride(t *testing.T) {
	t.Run("lowered to 50", func(t *testing.T) {
		t.Setenv("GC_DOLT_ARCHIVE_DICT_WARN_COUNT", "50")
		dir := setupManagedDoltCity(t)
		writeDarcDicts(t, darcPath(dir, "hq", "", "at.darc"), 50)
		if r := runArchiveDicts(t, dir); r.Status != StatusOK {
			t.Fatalf("status = %d, want OK at the overridden limit; msg = %s", r.Status, r.Message)
		}
		writeDarcDicts(t, darcPath(dir, "hq", "", "over.darc"), 51)
		r := runArchiveDicts(t, dir)
		if r.Status != StatusWarning || !strings.Contains(r.Message, "51") {
			t.Fatalf("status = %d, message = %q, want Warning naming 51", r.Status, r.Message)
		}
	})
	for _, bad := range []string{"abc", "0", "-5"} {
		t.Run("invalid "+bad+" uses the default", func(t *testing.T) {
			t.Setenv("GC_DOLT_ARCHIVE_DICT_WARN_COUNT", bad)
			dir := setupManagedDoltCity(t)
			writeDarcDicts(t, darcPath(dir, "hq", "", "at.darc"), 200)
			if r := runArchiveDicts(t, dir); r.Status != StatusOK {
				t.Fatalf("status = %d, want OK at 200 with the default limit; msg = %s", r.Status, r.Message)
			}
			writeDarcDicts(t, darcPath(dir, "hq", "", "over.darc"), 201)
			if r := runArchiveDicts(t, dir); r.Status != StatusWarning {
				t.Fatalf("status = %d, want Warning at 201 with the default limit; msg = %s", r.Status, r.Message)
			}
		})
	}
}

func TestDoltArchiveDictCheck_BothGenerations(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDarcDicts(t, darcPath(dir, "hq", "", "newgen.darc"), 230)
	writeDarcDicts(t, darcPath(dir, "hq", "oldgen", "oldgen.darc"), 220)
	r := runArchiveDicts(t, dir)
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "noms/newgen.darc") || !strings.Contains(r.Message, "230") {
		t.Errorf("message = %q, want the worst archive (noms/newgen.darc, 230)", r.Message)
	}
	if !strings.Contains(r.Message, "1 more") {
		t.Errorf("message = %q, want the count of further over-limit archives", r.Message)
	}
	details := detailsText(r)
	for _, want := range []string{"noms/newgen.darc", "noms/oldgen/oldgen.darc", "230", "220"} {
		if !strings.Contains(details, want) {
			t.Errorf("details = %q, want it to contain %q", details, want)
		}
	}
}

func TestDoltArchiveDictCheck_OldgenAloneWarns(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDarcDicts(t, darcPath(dir, "hq", "oldgen", "only.darc"), 409)
	r := runArchiveDicts(t, dir)
	if r.Status != StatusWarning || !strings.Contains(r.Message, "noms/oldgen/only.darc") || !strings.Contains(r.Message, "409") {
		t.Fatalf("status = %d, message = %q, want Warning naming noms/oldgen/only.darc and 409", r.Status, r.Message)
	}
}

func TestDoltArchiveDictCheck_MultiDatabase(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDarcDicts(t, darcPath(dir, "hq", "", "a.darc"), 250)
	writeDarcDicts(t, darcPath(dir, "de", "oldgen", "b.darc"), 300)
	writeDarcDicts(t, darcPath(dir, "zz", "", "c.darc"), 210)
	writeDarcDicts(t, darcPath(dir, "zz", "", "d.darc"), 5)
	r := runArchiveDicts(t, dir)
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	for _, want := range []string{"de", "noms/oldgen/b.darc", "300", "2 more"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message = %q, want it to contain %q", r.Message, want)
		}
	}
	details := detailsText(r)
	for _, db := range []string{"database hq", "database de", "database zz"} {
		if !strings.Contains(details, db) {
			t.Errorf("details = %q, want a line for %q", details, db)
		}
	}
	// Every over-limit archive is listed, largest first.
	i300 := strings.Index(details, "noms/oldgen/b.darc")
	i250 := strings.Index(details, "noms/a.darc")
	i210 := strings.Index(details, "noms/c.darc")
	if i300 < 0 || i250 < 0 || i210 < 0 || i300 >= i250 || i250 >= i210 {
		t.Errorf("details = %q, want b.darc (300), a.darc (250), c.darc (210) in count-descending order", details)
	}
	if strings.Contains(details, "noms/d.darc") && strings.Contains(strings.SplitN(details, "noms/d.darc", 2)[0], "over-limit") {
		t.Errorf("details = %q, must not list the 5-dictionary archive as over the limit", details)
	}
	if !strings.Contains(details, "GC_DOLT_ARCHIVE_DICT_WARN_COUNT") {
		t.Errorf("details = %q, want the threshold line", details)
	}
}

func TestDoltArchiveDictCheck_LoudFailures(t *testing.T) {
	good, goodSize := goodFooter(5)
	mutate := func(f func(b []byte)) []byte {
		b := append([]byte(nil), good...)
		f(b)
		return b
	}
	cases := []struct {
		name     string
		footer   []byte
		fileSize int64
		reason   string
	}{
		{"bad signature", mutate(func(b []byte) { copy(b[213:], "NOTDARC") }), goodSize, "bad signature"},
		{"version 2", mutate(func(b []byte) { b[212] = 2 }), goodSize, "unsupported archive format version 2"},
		{"version 4", mutate(func(b []byte) { b[212] = 4 }), goodSize, "unsupported archive format version 4"},
		{"tiling mismatch", mutate(func(b []byte) { binary.BigEndian.PutUint64(b[0:8], 1) }), goodSize, "index length does not match span and chunk counts"},
		{"spans below chunks", darcFooter(darcIndexLen(1, 2), 1, 2, 0, 3, "DOLTARC"), int64(darcIndexLen(1, 2)) + doltArchiveFooterSize, "fewer byte spans than chunks"},
		{"overrun", mutate(func(b []byte) { binary.BigEndian.PutUint32(b[16:20], 99999) }), goodSize, "index and metadata overrun the file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupManagedDoltCity(t)
			writeDarcFile(t, darcPath(dir, "hq", "", "bad.darc"), tc.footer, tc.fileSize)
			r := runArchiveDicts(t, dir)
			if r.Status != StatusWarning {
				t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
			}
			text := r.Message + "\n" + detailsText(r)
			for _, want := range []string{"noms/bad.darc", tc.reason} {
				if !strings.Contains(text, want) {
					t.Errorf("message+details = %q, want it to contain %q", text, want)
				}
			}
		})
	}

	t.Run("file shorter than the footer", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeFakeFile(t, darcPath(dir, "hq", "", "tiny.darc"), 100)
		r := runArchiveDicts(t, dir)
		text := r.Message + "\n" + detailsText(r)
		if r.Status != StatusWarning || !strings.Contains(text, "noms/tiny.darc") || !strings.Contains(text, "shorter than the 220-byte footer") {
			t.Fatalf("status = %d, text = %q, want Warning naming noms/tiny.darc and the short-file reason", r.Status, text)
		}
	})

	t.Run("a directory named like an archive", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		if err := os.MkdirAll(darcPath(dir, "hq", "", "dir.darc"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runArchiveDicts(t, dir)
		text := r.Message + "\n" + detailsText(r)
		if r.Status != StatusWarning || !strings.Contains(text, "noms/dir.darc") {
			t.Fatalf("status = %d, text = %q, want Warning naming noms/dir.darc", r.Status, text)
		}
	})

	t.Run("over-limit and unreadable are both reported", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeDarcDicts(t, darcPath(dir, "hq", "", "big.darc"), 201)
		writeDarcFile(t, darcPath(dir, "hq", "", "bad.darc"), mutate(func(b []byte) { b[212] = 9 }), goodSize)
		r := runArchiveDicts(t, dir)
		if r.Status != StatusWarning {
			t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
		}
		for _, want := range []string{"noms/big.darc", "201", "could not be read"} {
			if !strings.Contains(r.Message, want) {
				t.Errorf("message = %q, want it to contain %q", r.Message, want)
			}
		}
		details := detailsText(r)
		if !strings.Contains(details, "unreadable:") || !strings.Contains(details, "noms/bad.darc") || !strings.Contains(details, "unsupported archive format version 9") {
			t.Errorf("details = %q, want an unreadable: line for noms/bad.darc with its reason", details)
		}
	})
}

func TestDoltArchiveDictCheck_VanishedFileIsSkipped(t *testing.T) {
	dir := setupManagedDoltCity(t)
	noms := filepath.Dir(darcPath(dir, "hq", "", "x.darc"))
	if err := os.MkdirAll(noms, 0o755); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink lists in the directory but cannot be opened, the same
	// shape as an archive a GC deletes between the listing and the open.
	if err := os.Symlink(filepath.Join(noms, "gone.darc"), filepath.Join(noms, "ghost.darc")); err != nil {
		t.Fatal(err)
	}
	r := runArchiveDicts(t, dir)
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestDoltArchiveDictCheck_ReadOnly(t *testing.T) {
	dir := setupManagedDoltCity(t)
	paths := []string{
		darcPath(dir, "hq", "", "big.darc"),
		darcPath(dir, "hq", "oldgen", "old.darc"),
		darcPath(dir, "hq", "", "ok.darc"),
	}
	writeDarcDicts(t, paths[0], 300)
	writeDarcDicts(t, paths[1], 250)
	writeDarcDicts(t, paths[2], 3)

	type snap struct {
		data  string
		mtime int64
	}
	snapshot := func() (map[string]snap, []string) {
		files := map[string]snap{}
		var listing []string
		root := filepath.Join(dir, ".beads", "dolt")
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			listing = append(listing, p)
			if info.Mode().IsRegular() {
				b, readErr := os.ReadFile(p) //nolint:gosec // test reads its own temp tree
				if readErr != nil {
					return readErr
				}
				files[p] = snap{data: string(b), mtime: info.ModTime().UnixNano()}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files, listing
	}

	beforeFiles, beforeListing := snapshot()
	if r := runArchiveDicts(t, dir); r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	afterFiles, afterListing := snapshot()

	if strings.Join(beforeListing, "\n") != strings.Join(afterListing, "\n") {
		t.Errorf("directory listing changed:\nbefore: %v\nafter:  %v", beforeListing, afterListing)
	}
	for p, b := range beforeFiles {
		if a, ok := afterFiles[p]; !ok || a != b {
			t.Errorf("%s changed (bytes or mtime) across Run", p)
		}
	}
}

func TestDoltArchiveDictCheck_FixAndWarmup(t *testing.T) {
	c := NewDoltArchiveDictCheck(t.TempDir(), false)
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil", err)
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
	if c.Name() != "dolt-archive-dicts" {
		t.Errorf("Name() = %q, want dolt-archive-dicts", c.Name())
	}
}

func TestDoltArchiveDictCheck_FixHint(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDarcDicts(t, darcPath(dir, "hq", "", "big.darc"), 300)
	r := runArchiveDicts(t, dir)
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	for _, want := range []string{"--archive-level=0", "dolthub/dolt#12177"} {
		if !strings.Contains(r.FixHint, want) {
			t.Errorf("FixHint = %q, want it to contain %q", r.FixHint, want)
		}
	}
	if strings.Contains(r.FixHint, "gc dolt compact") {
		t.Errorf("FixHint = %q, must not recommend gc dolt compact", r.FixHint)
	}
}
