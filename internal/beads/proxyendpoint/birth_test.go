package proxyendpoint

import (
	"strings"
	"testing"
)

// TestParseProcStatStartTime pins the parse against the shapes /proc actually
// produces, including the two that break a naive field split.
func TestParseProcStatStartTime(t *testing.T) {
	// A real row, trimmed to the fields the parse walks. Fields after comm are:
	// state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt
	// utime stime cutime cstime priority nice num_threads itrealvalue starttime.
	const postComm = "S 1 4242 4242 0 -1 4194560 372 0 0 0 5 3 0 0 20 0 9 0 99887766 1234 5678"

	cases := []struct {
		name    string
		stat    string
		want    string
		wantErr bool
	}{
		{
			name: "an ordinary row",
			stat: "4242 (bd) " + postComm,
			want: "99887766",
		},
		{
			// A comm with spaces and parentheses is why the parse anchors on the
			// LAST ')' rather than splitting on whitespace.
			name: "a comm containing spaces and parentheses",
			stat: "4242 (bd db-proxy (child)) " + postComm,
			want: "99887766",
		},
		{
			name: "a trailing newline",
			stat: "4242 (bd) " + postComm + "\n",
			want: "99887766",
		},
		{
			// A zombie's start time would otherwise validate a record whose
			// process can no longer serve anything.
			name:    "a zombie is not a live process",
			stat:    "4242 (bd) Z 1 4242 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 99887766",
			wantErr: true,
		},
		{
			name:    "a dead process",
			stat:    "4242 (bd) X 1 4242 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 99887766",
			wantErr: true,
		},
		{
			name:    "no comm terminator",
			stat:    "4242 bd S 1",
			wantErr: true,
		},
		{
			name:    "truncated before starttime",
			stat:    "4242 (bd) S 1 4242",
			wantErr: true,
		},
		{
			name:    "a non-numeric starttime",
			stat:    "4242 (bd) S 1 4242 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 notanumber",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProcStatStartTime(tc.stat)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseProcStatStartTime(%q) = %q, want an error", tc.stat, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseProcStatStartTime: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseProcStatStartTime = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBirthTokenMatchesBdsSpelling pins the assembled token byte for byte. gc
// compares its recompute against a string bd wrote, so a stray space or a
// different separator would make every live proxy look like a restarted one.
func TestBirthTokenMatchesBdsSpelling(t *testing.T) {
	const bootID = "5f0b9c1e-aaaa-bbbb-cccc-0123456789ab"
	want := "linux-v1:" + bootID + ":99887766"

	if got := BirthToken(bootID, "99887766"); got != want {
		t.Fatalf("BirthToken = %q, want %q", got, want)
	}
	// /proc/sys/kernel/random/boot_id is newline-terminated, and bd trims it
	// before assembling the token.
	if got := BirthToken(bootID+"\n", " 99887766 "); got != want {
		t.Fatalf("BirthToken with untrimmed inputs = %q, want %q", got, want)
	}
	if !strings.HasPrefix(want, BirthTokenPrefix) {
		t.Fatalf("BirthTokenPrefix %q does not prefix the token this package builds", BirthTokenPrefix)
	}
}
