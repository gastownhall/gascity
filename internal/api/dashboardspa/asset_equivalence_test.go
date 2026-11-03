package dashboardspa

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Raw canonical assets are embedded only in the test binary, never in gc.
//
//go:embed all:dist
var canonicalDist embed.FS

func TestEmbeddedAssetsEqualCanonicalBytesAndMetadata(t *testing.T) {
	err := fs.WalkDir(canonicalDist, "dist", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		want, err := entry.Info()
		if err != nil {
			return err
		}
		got, err := fs.Stat(distFS, name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got.Name() != want.Name() || got.Size() != want.Size() || got.Mode() != want.Mode() || !got.ModTime().Equal(want.ModTime()) {
			t.Errorf("metadata differs for %s: got %v, want %v", name, got, want)
		}
		if entry.IsDir() {
			got, err := fs.ReadDir(distFS, name)
			if err != nil {
				return err
			}
			want, err := fs.ReadDir(canonicalDist, name)
			if err != nil {
				return err
			}
			if len(got) != len(want) {
				t.Fatalf("directory %s has %d entries, want %d", name, len(got), len(want))
			}
			for i := range want {
				if got[i].Name() != want[i].Name() || got[i].Type() != want[i].Type() {
					t.Errorf("directory %s entry %d differs", name, i)
				}
			}
			return nil
		}
		wantBytes, err := canonicalDist.ReadFile(name)
		if err != nil {
			return err
		}
		gotBytes, err := fs.ReadFile(distFS, name)
		if err != nil || !bytes.Equal(gotBytes, wantBytes) {
			t.Fatalf("asset bytes differ for %s: %v", name, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedAssetsServeExactBytesHeadAndRanges(t *testing.T) {
	handler := newHandler(t)
	assets, err := fs.ReadDir(canonicalDist, "dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	var ranged bool
	for _, entry := range assets {
		if entry.IsDir() {
			continue
		}
		name := "assets/" + entry.Name()
		want, err := canonicalDist.ReadFile("dist/" + name)
		if err != nil {
			t.Fatal(err)
		}
		response := get(t, handler, "/"+name)
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), want) || response.Header().Get("Content-Encoding") != "" {
			t.Fatalf("served asset differs: %s status=%d", name, response.Code)
		}
		if !strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("asset cache policy changed: %s", name)
		}
		head := httptest.NewRecorder()
		handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/"+name, nil))
		if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(len(want)) {
			t.Fatalf("HEAD changed for %s: status=%d bytes=%d headers=%v", name, head.Code, head.Body.Len(), head.Header())
		}
		if !ranged && len(want) > 16 {
			request := httptest.NewRequest(http.MethodGet, "/"+name, nil)
			request.Header.Set("Range", "bytes=3-12")
			part := httptest.NewRecorder()
			handler.ServeHTTP(part, request)
			if part.Code != http.StatusPartialContent || !bytes.Equal(part.Body.Bytes(), want[3:13]) || part.Header().Get("Content-Range") != fmt.Sprintf("bytes 3-12/%d", len(want)) {
				t.Fatalf("Range changed for %s: status=%d headers=%v", name, part.Code, part.Header())
			}
			file, err := distFS.Open("dist/" + name)
			if err != nil {
				t.Fatal(err)
			}
			seeker, ok := file.(io.ReadSeeker)
			if !ok {
				t.Fatal("embedded asset lost seek support")
			}
			if _, err := seeker.Seek(3, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 10)
			if _, err := io.ReadFull(seeker, got); err != nil || !bytes.Equal(got, want[3:13]) {
				t.Fatalf("seek/read differs: %q, %v", got, err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			ranged = true
		}
	}
	if !ranged {
		t.Fatal("no real asset exercised Range and Seek")
	}
}
