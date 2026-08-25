package pidutil

import (
	"encoding/binary"
	"testing"
)

func TestParseProcArgs2PreservesArgumentBoundaries(t *testing.T) {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, 4)
	data = append(data, []byte("/usr/local/bin/gc\x00\x00\x00")...)
	data = append(data, []byte("gc\x00nudge\x00--city\x00/tmp/city with spaces\x00TOKEN=secret\x00")...)

	got, err := parseProcArgs2(data)
	if err != nil {
		t.Fatalf("parseProcArgs2: %v", err)
	}
	want := []string{"gc", "nudge", "--city", "/tmp/city with spaces"}
	if len(got) != len(want) {
		t.Fatalf("parseProcArgs2 = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseProcArgs2 = %q, want %q", got, want)
		}
	}
}

func TestParseProcArgs2RejectsTruncatedArgv(t *testing.T) {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, 2)
	data = append(data, []byte("/usr/bin/gc\x00gc\x00")...)

	if _, err := parseProcArgs2(data); err == nil {
		t.Fatal("parseProcArgs2 accepted fewer arguments than argc")
	}
}
