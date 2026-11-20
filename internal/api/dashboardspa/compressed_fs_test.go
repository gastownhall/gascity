package dashboardspa

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"testing"
)

func packTestManifest(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestCompressedAssetsRefuseCorruptionAndAmbiguousPaths(t *testing.T) {
	valid := assetManifest{Version: 1, Files: []packedAsset{{Name: "dist/index.html", Data: []byte("index")}}}
	packed := packTestManifest(t, valid)
	badCRC := bytes.Clone(packed)
	badCRC[len(badCRC)-5] ^= 1
	for name, data := range map[string][]byte{
		"invalid gzip":            []byte("not gzip"),
		"truncated":               packed[:len(packed)-1],
		"checksum":                badCRC,
		"trailing gzip":           append(bytes.Clone(packed), packed...),
		"wrong version":           packTestManifest(t, assetManifest{Version: 2, Files: valid.Files}),
		"empty":                   packTestManifest(t, assetManifest{Version: 1}),
		"escape":                  packTestManifest(t, assetManifest{Version: 1, Files: []packedAsset{{Name: "dist/../outside"}}}),
		"wrong root":              packTestManifest(t, assetManifest{Version: 1, Files: []packedAsset{{Name: "outside/file"}}}),
		"duplicate":               packTestManifest(t, assetManifest{Version: 1, Files: append(valid.Files, valid.Files...)}),
		"file directory conflict": packTestManifest(t, assetManifest{Version: 1, Files: []packedAsset{{Name: "dist/index.html"}, {Name: "dist/index.html/child"}}}),
		"unknown field":           packTestManifest(t, map[string]any{"version": 1, "files": valid.Files, "extra": true}),
	} {
		t.Run(name, func(t *testing.T) {
			filesystem := &compressedDistFS{packed: data}
			for range 2 {
				if _, err := filesystem.Open("dist/index.html"); err == nil {
					t.Fatal("damaged representation exposed a partial filesystem")
				}
			}
		})
	}
}

func TestCompressedAssetsKeepIndependentSeekAndDirectoryState(t *testing.T) {
	filesystem := &compressedDistFS{packed: packTestManifest(t, assetManifest{Version: 1, Files: []packedAsset{
		{Name: "dist/b", Data: []byte("second")}, {Name: "dist/a", Data: []byte("abcdef")},
	}})}
	first, err := filesystem.Open("dist/a")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close() //nolint:errcheck // immutable in-memory file
	seeker := first.(io.ReadSeeker)
	if _, err := seeker.Seek(-2, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(seeker)
	if err != nil || string(got) != "ef" {
		t.Fatalf("end-relative seek=%q,%v", got, err)
	}
	for _, offset := range []int64{-1, 7} {
		if _, err := seeker.Seek(offset, io.SeekStart); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("out-of-bounds seek %d differs from embed.FS: %v", offset, err)
		}
		if _, err := first.(io.ReaderAt).ReadAt(make([]byte, 1), offset); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("out-of-bounds ReadAt %d differs from embed.FS: %v", offset, err)
		}
	}
	if n, err := first.(io.ReaderAt).ReadAt(nil, 6); n != 0 || err != nil {
		t.Fatalf("empty ReadAt at EOF differs from embed.FS: %d,%v", n, err)
	}
	again, err := fs.ReadFile(filesystem, "dist/a")
	if err != nil || string(again) != "abcdef" {
		t.Fatalf("read position leaked across opens: %q,%v", again, err)
	}
	for _, name := range []string{"../dist/a", "/dist/a"} {
		if _, err := filesystem.Open(name); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("invalid path %q: %v", name, err)
		}
	}
	directory, err := filesystem.Open("dist")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close() //nolint:errcheck // immutable in-memory directory
	rd := directory.(fs.ReadDirFile)
	for _, want := range []string{"a", "b"} {
		entries, err := rd.ReadDir(1)
		if err != nil || len(entries) != 1 || entries[0].Name() != want {
			t.Fatalf("directory order/offset=%v,%v want %s", entries, err, want)
		}
	}
	if _, err := rd.ReadDir(1); !errors.Is(err, io.EOF) {
		t.Fatalf("directory EOF=%v", err)
	}
}
