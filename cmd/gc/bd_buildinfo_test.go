package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGoBinary writes a file with a Go 1.18+ build-info blob for mod at a
// 16-byte-aligned offset straddling the first read chunk boundary, after a
// decoy copy of the magic at an unaligned offset.
func fakeGoBinary(t *testing.T, vers, mod string) string {
	t.Helper()
	var blob []byte
	blob = append(blob, goBuildInfoMagic...)
	blob = append(blob, 8, 0x2)
	blob = append(blob, make([]byte, 32-len(blob))...)
	sentinel := strings.Repeat("\x30", 15) + "\x77"
	framed := sentinel + mod + "\n" + strings.Repeat("\x31", 15) + "\x88"
	for _, s := range []string{vers, framed} {
		blob = binary.AppendUvarint(blob, uint64(len(s)))
		blob = append(blob, s...)
	}
	data := make([]byte, (1<<20)-16)
	copy(data[101:], goBuildInfoMagic) // unaligned decoy
	data = append(data, blob...)
	data = append(data, make([]byte, 4096)...)
	p := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(p, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadGoBuildInfo(t *testing.T) {
	mod := "path\tgithub.com/steveyegge/beads/cmd/bd\nmod\tgithub.com/steveyegge/beads\t(devel)\t\ndep\tgithub.com/spf13/cobra\tv1.10.2\t\ndep\tgithub.com/spf13/pflag\tv1.0.10\t\nbuild\tCGO_ENABLED=1"
	bi, err := readGoBuildInfo(fakeGoBinary(t, "go1.26.10", mod))
	if err != nil {
		t.Fatal(err)
	}
	if bi.Path != "github.com/steveyegge/beads/cmd/bd" || bi.GoVersion != "go1.26.10" || len(bi.Deps) != 2 || bi.Deps[1].Version != "v1.0.10" {
		t.Fatalf("build info = %+v", bi)
	}
	script := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec bd \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readGoBuildInfo(script); err == nil {
		t.Fatal("a shell script has no Go build info")
	}
}
