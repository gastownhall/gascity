package dashboardspa

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// The committed blob is trusted build input, but a damaged blob must fail
// handler construction rather than publish a partial filesystem or unbounded data.
const maxExpandedDistBytes = 64 << 20

type assetManifest struct {
	Version int           `json:"version"`
	Files   []packedAsset `json:"files"`
}

type packedAsset struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// compressedDistFS changes only storage representation. Open returns ordinary
// uncompressed, seekable read-only files, so HTTP serving retains byte/range and
// metadata semantics. Decode happens once when the dashboard is first used.
type compressedDistFS struct {
	packed []byte
	once   sync.Once
	nodes  map[string]*assetNode
	err    error
}

func (f *compressedDistFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	f.once.Do(func() { f.nodes, f.err = decodeAssetManifest(f.packed) })
	if f.err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: f.err}
	}
	node, ok := f.nodes[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if node.dir {
		return &assetDirectory{node: node}, nil
	}
	return &assetFile{Reader: bytes.NewReader(node.data), node: node}, nil
}

func decodeAssetManifest(packed []byte) (map[string]*assetNode, error) {
	compressed := bytes.NewReader(packed)
	reader, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("dashboard assets gzip: %w", err)
	}
	reader.Multistream(false)
	raw, readErr := io.ReadAll(io.LimitReader(reader, maxExpandedDistBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("dashboard assets gzip: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("dashboard assets gzip: %w", closeErr)
	}
	if len(raw) > maxExpandedDistBytes || compressed.Len() != 0 {
		return nil, fmt.Errorf("dashboard assets exceed bounds or contain trailing compressed data")
	}
	var manifest assetManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("dashboard assets manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("dashboard assets manifest contains trailing data")
	}
	if manifest.Version != 1 || len(manifest.Files) == 0 {
		return nil, fmt.Errorf("dashboard assets manifest version or file set is invalid")
	}
	nodes := map[string]*assetNode{".": {name: ".", dir: true}}
	for _, asset := range manifest.Files {
		if !fs.ValidPath(asset.Name) || !strings.HasPrefix(asset.Name, "dist/") || strings.Contains(asset.Name, "\\") {
			return nil, fmt.Errorf("dashboard asset path is invalid: %q", asset.Name)
		}
		if _, exists := nodes[asset.Name]; exists {
			return nil, fmt.Errorf("dashboard asset path is duplicated: %q", asset.Name)
		}
		nodes[asset.Name] = &assetNode{name: asset.Name, data: asset.Data}
		for parent := path.Dir(asset.Name); parent != "."; parent = path.Dir(parent) {
			if existing, exists := nodes[parent]; exists {
				if !existing.dir {
					return nil, fmt.Errorf("dashboard file conflicts with directory: %q", parent)
				}
				break
			}
			nodes[parent] = &assetNode{name: parent, dir: true}
		}
	}
	for name, node := range nodes {
		if name != "." {
			parent := nodes[path.Dir(name)]
			parent.children = append(parent.children, node)
		}
	}
	for _, node := range nodes {
		sort.Slice(node.children, func(i, j int) bool { return node.children[i].Name() < node.children[j].Name() })
	}
	return nodes, nil
}

type assetNode struct {
	name     string
	data     []byte
	dir      bool
	children []fs.DirEntry
}

func (n *assetNode) Name() string               { return path.Base(n.name) }
func (n *assetNode) Size() int64                { return int64(len(n.data)) }
func (n *assetNode) ModTime() time.Time         { return time.Time{} }
func (n *assetNode) IsDir() bool                { return n.dir }
func (n *assetNode) Sys() any                   { return nil }
func (n *assetNode) Type() fs.FileMode          { return n.Mode().Type() }
func (n *assetNode) Info() (fs.FileInfo, error) { return n, nil }
func (n *assetNode) Mode() fs.FileMode {
	if n.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

type assetFile struct {
	*bytes.Reader
	node *assetNode
}

func (f *assetFile) Stat() (fs.FileInfo, error) { return f.node, nil }
func (f *assetFile) Close() error               { return nil }

// Match embed.FS: seeking past the file or reading at an invalid offset is
// fs.ErrInvalid, and a refused seek does not change this open file's position.
func (f *assetFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += f.node.Size() - int64(f.Len())
	case io.SeekEnd:
		offset += f.node.Size()
	}
	if offset < 0 || offset > f.node.Size() {
		return 0, &fs.PathError{Op: "seek", Path: f.node.name, Err: fs.ErrInvalid}
	}
	return f.Reader.Seek(offset, io.SeekStart)
}

func (f *assetFile) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 || offset > f.node.Size() {
		return 0, &fs.PathError{Op: "read", Path: f.node.name, Err: fs.ErrInvalid}
	}
	n := copy(buffer, f.node.data[offset:])
	if n < len(buffer) {
		return n, io.EOF
	}
	return n, nil
}

type assetDirectory struct {
	node   *assetNode
	offset int
}

func (d *assetDirectory) Stat() (fs.FileInfo, error) { return d.node, nil }
func (d *assetDirectory) Close() error               { return nil }
func (d *assetDirectory) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.node.name, Err: fs.ErrInvalid}
}

func (d *assetDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	if n > 0 && d.offset >= len(d.node.children) {
		return nil, io.EOF
	}
	end := len(d.node.children)
	if n > 0 && n < end-d.offset {
		end = d.offset + n
	}
	entries := append([]fs.DirEntry{}, d.node.children[d.offset:end]...)
	d.offset = end
	return entries, nil
}
