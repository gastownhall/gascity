package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

// goBuildInfoMagic opens the build-info blob every Go 1.18+ binary carries.
var goBuildInfoMagic = []byte("\xff Go buildinf:")

// readGoBuildInfo reads a Go binary's embedded build info. It scans the file
// for the build-info blob instead of parsing the executable format, which
// debug/buildinfo does by linking debug/elf, macho, pe, xcoff and plan9obj —
// enough to push gc past its binary-size cap for one check.
func readGoBuildInfo(path string) (*debug.BuildInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	const chunk = 1 << 20
	buf := make([]byte, chunk+len(goBuildInfoMagic))
	var off int64
	carry := 0
	for {
		n, rerr := io.ReadFull(f, buf[carry:])
		data := buf[:carry+n]
		// The linker aligns the blob to 16 bytes (as debug/buildinfo
		// assumes); an unaligned copy of the magic is some other data.
		for from := 0; ; {
			i := bytes.Index(data[from:], goBuildInfoMagic)
			if i < 0 {
				break
			}
			at := off - int64(carry) + int64(from+i)
			if at%16 == 0 {
				return parseGoBuildInfoAt(f, at)
			}
			from += i + 1
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return nil, errors.New("no Go build info")
			}
			return nil, rerr
		}
		off += int64(n)
		carry = len(goBuildInfoMagic) - 1
		copy(buf, data[len(data)-carry:])
	}
}

// parseGoBuildInfoAt decodes the blob at off: a 32-byte header (magic,
// pointer size, flags), then — in the inline format every Go 1.18+ linker
// writes — the Go version and the module info as uvarint-length strings, the
// module info framed by 16-byte sentinels.
func parseGoBuildInfoAt(r io.ReaderAt, off int64) (*debug.BuildInfo, error) {
	blob := make([]byte, 64<<10)
	n, err := r.ReadAt(blob, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	blob = blob[:n]
	if len(blob) < 32 {
		return nil, errors.New("truncated Go build info")
	}
	if blob[15]&0x2 == 0 {
		return nil, errors.New("build info in the pre-Go-1.18 format")
	}
	rest := blob[32:]
	readString := func() (string, error) {
		l, k := binary.Uvarint(rest)
		if k <= 0 || l > uint64(len(rest)-k) {
			return "", errors.New("malformed Go build info")
		}
		s := string(rest[k : k+int(l)])
		rest = rest[k+int(l):]
		return s, nil
	}
	vers, err := readString()
	if err != nil {
		return nil, err
	}
	mod, err := readString()
	if err != nil {
		return nil, err
	}
	if len(mod) >= 33 && mod[len(mod)-17] == '\n' {
		mod = mod[16 : len(mod)-16]
	}
	bi, err := debug.ParseBuildInfo(mod)
	if err != nil {
		return nil, fmt.Errorf("parsing Go build info: %w", err)
	}
	bi.GoVersion = vers
	return bi, nil
}
