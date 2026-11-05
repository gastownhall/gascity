package pidutil

import (
	"encoding/binary"
	"slices"
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

// procArgs2Buffer lays out a kern.procargs2 buffer as the kernel copies it
// out: argc, the executable path and its alignment padding, each argument,
// then tail (the environment and whatever follows it) verbatim.
func procArgs2Buffer(argv []string, tail string) []byte {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, uint32(len(argv)))
	data = append(data, []byte("/usr/local/bin/gc\x00\x00\x00")...)
	for _, arg := range argv {
		data = append(data, arg...)
		data = append(data, 0)
	}
	return append(data, tail...)
}

// The environment is read as the kernel delimits it, never split on
// whitespace: a TMUX_TMPDIR under a directory holding spaces comes back whole,
// and an argument shaped like an entry stays an argument.
func TestParseProcArgs2EnvironKeepsEachEntryWhole(t *testing.T) {
	argv := []string{"gc", "supervisor", "run", "TMUX_TMPDIR=/from/argv"}
	for _, tc := range []struct {
		name string
		tail string
		want []string
	}{
		{name: "a value holding spaces", tail: "TMUX_TMPDIR=/tmp/gc sockets\x00HOME=/Users/dev\x00\x00\x00", want: []string{"TMUX_TMPDIR=/tmp/gc sockets", "HOME=/Users/dev"}},
		{name: "the kernel's own strings after an alignment gap", tail: "TMUX_TMPDIR=/tmp/gc sockets\x00\x00ptr_munge=\x00main_stack=\x00", want: []string{"TMUX_TMPDIR=/tmp/gc sockets"}},
		{name: "the buffer ends after an entry", tail: "A=1\x00B=two words\x00", want: []string{"A=1", "B=two words"}},
		{name: "an environment the kernel withheld", tail: ""},
		{name: "an empty environment", tail: "\x00\x00\x00\x00ptr_munge=\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := procArgs2Buffer(argv, tc.tail)
			env, err := parseProcArgs2Environ(data)
			if err != nil || !slices.Equal(env, tc.want) {
				t.Fatalf("parseProcArgs2Environ = %q, %v; want %q", env, err, tc.want)
			}
			if got, err := parseProcArgs2(data); err != nil || !slices.Equal(got, argv) {
				t.Fatalf("parseProcArgs2 = %q, %v; want %q", got, err, argv)
			}
		})
	}
}

// A buffer that ends inside an entry has lost the rest of that value, so the
// environment read fails rather than return a truncated value.
func TestParseProcArgs2EnvironRejectsATruncatedEntry(t *testing.T) {
	data := procArgs2Buffer([]string{"gc"}, "A=1\x00TMUX_TMPDIR=/tmp/gc soc")
	if env, err := parseProcArgs2Environ(data); err == nil {
		t.Fatalf("parseProcArgs2Environ = %q, nil; want an error for the truncated entry", env)
	}
}
