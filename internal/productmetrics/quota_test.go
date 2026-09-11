package productmetrics

import (
	"math"
	"strings"
	"testing"
)

func TestQuotaCodecIsStrictAndBounded(t *testing.T) {
	quota := spoolQuota{Events: 17, Bytes: 4096}
	encoded, err := encodeSpoolQuota(quota)
	if err != nil {
		t.Fatalf("encodeSpoolQuota: %v", err)
	}
	decoded, err := decodeSpoolQuota(encoded)
	if err != nil {
		t.Fatalf("decodeSpoolQuota: %v", err)
	}
	if decoded != quota {
		t.Fatalf("quota round trip = %+v, want %+v", decoded, quota)
	}

	cases := map[string][]byte{
		"empty":         nil,
		"missing field": []byte("quota_schema = 1\nreserved_events = 1\n"),
		"unknown field": append(append([]byte(nil), encoded...), []byte("extra = 1\n")...),
		"nested field":  append(append([]byte(nil), encoded...), []byte("[nested]\nvalue = 1\n")...),
		"future schema": []byte("quota_schema = 2\nreserved_events = 0\nreserved_bytes = 0\n"),
		"too large":     []byte(strings.Repeat("#", maximumQuotaBytes+1)),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSpoolQuota(input); err == nil {
				t.Fatal("decodeSpoolQuota unexpectedly succeeded")
			}
		})
	}
}

func TestRelocationCursorCodecIsStrictAndBounded(t *testing.T) {
	cursor := relocationCursor{Next: 17}
	encoded, err := encodeRelocationCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRelocationCursor(encoded)
	if err != nil || decoded != cursor {
		t.Fatalf("relocation cursor round trip = (%+v, %v)", decoded, err)
	}
	for name, input := range map[string][]byte{
		"empty":         nil,
		"missing field": []byte("cursor_schema = 1\n"),
		"unknown field": append(append([]byte(nil), encoded...), []byte("extra = 1\n")...),
		"future schema": []byte("cursor_schema = 2\nrelocation_next = 0\n"),
		"too large":     []byte(strings.Repeat("#", maximumRelocationBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRelocationCursor(input); err == nil {
				t.Fatal("decodeRelocationCursor unexpectedly succeeded")
			}
		})
	}
}

func TestQuotaReservationCapsAndOverflowFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		quota   spoolQuota
		bytes   uint64
		wantErr bool
	}{
		{name: "last event and byte", quota: spoolQuota{Events: maximumSpoolEvents - 1, Bytes: maximumSpoolBytes - 1}, bytes: 1},
		{name: "event cap", quota: spoolQuota{Events: maximumSpoolEvents}, bytes: 1, wantErr: true},
		{name: "byte cap", quota: spoolQuota{Bytes: maximumSpoolBytes}, bytes: 1, wantErr: true},
		{name: "event too large", bytes: maximumEventBytes + 1, wantErr: true},
		{name: "zero event", bytes: 0, wantErr: true},
		{name: "counter overflow", quota: spoolQuota{Events: 1, Bytes: math.MaxUint64}, bytes: 1, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.quota.reserve(test.bytes)
			if (err != nil) != test.wantErr {
				t.Fatalf("reserve error = %v, wantErr %v", err, test.wantErr)
			}
			if !test.wantErr && (got.Events != test.quota.Events+1 || got.Bytes != test.quota.Bytes+test.bytes) {
				t.Fatalf("reserved quota = %+v", got)
			}
		})
	}
}

func TestQuotaReleaseNeverUnderflows(t *testing.T) {
	quota := spoolQuota{Events: 2, Bytes: 300}
	got, err := quota.release(1, 100)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if got != (spoolQuota{Events: 1, Bytes: 200}) {
		t.Fatalf("released quota = %+v", got)
	}
	for _, release := range []spoolQuota{{Events: 3}, {Bytes: 301}, {Events: math.MaxUint64, Bytes: 1}} {
		if _, err := quota.release(release.Events, release.Bytes); err == nil {
			t.Fatalf("release %+v unexpectedly succeeded", release)
		}
	}
}

func TestSpoolLimitsMatchApprovedContract(t *testing.T) {
	if maximumSpoolBytes != 4*1024*1024 || maximumSpoolEvents != 5000 {
		t.Fatalf("root quota = (%d bytes, %d events)", maximumSpoolBytes, maximumSpoolEvents)
	}
	if maximumEventBytes != 4*1024 || maximumBatchEvents != 25 || maximumRequestBytes != 64*1024 {
		t.Fatalf("event/batch/request caps = (%d, %d, %d)", maximumEventBytes, maximumBatchEvents, maximumRequestBytes)
	}
	if maximumEventAgeHours != 7*24 {
		t.Fatalf("age cap = %d hours", maximumEventAgeHours)
	}
	budget := defaultSpoolWorkBudget()
	if budget.maxEntries != 6000 || budget.maxDirectories != 512 || budget.maxReadBytes != 5*1024*1024 || budget.maxNameBytes != 1024*1024 {
		t.Fatalf("cleanup budget = %+v", budget)
	}
	if maximumEnumerationEvents != 5001 || maximumStorageNameBytes != 128 || maximumConfigBytes != 16*1024 || maximumControlFileBytes != 4*1024 || maximumQuotaBytes != maximumControlFileBytes {
		t.Fatal("one or more declared storage bounds drifted")
	}
}

func FuzzDecodeSpoolQuota(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("quota_schema = 1\nreserved_events = 0\nreserved_bytes = 0\n"),
		[]byte(""),
		[]byte("quota_schema = 18446744073709551615\n"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > maximumQuotaBytes+64 {
			return
		}
		quota, err := decodeSpoolQuota(input)
		if err != nil {
			return
		}
		encoded, err := encodeSpoolQuota(quota)
		if err != nil {
			t.Fatalf("accepted quota cannot be re-encoded: %v", err)
		}
		if len(encoded) > maximumQuotaBytes {
			t.Fatalf("encoded accepted quota has %d bytes", len(encoded))
		}
	})
}

func FuzzDecodeRelocationCursor(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("cursor_schema = 1\nrelocation_next = 0\n"),
		[]byte(""),
		[]byte("cursor_schema = 2\nrelocation_next = 0\n"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > maximumRelocationBytes+64 {
			return
		}
		cursor, err := decodeRelocationCursor(input)
		if err != nil {
			return
		}
		encoded, err := encodeRelocationCursor(cursor)
		if err != nil {
			t.Fatalf("accepted relocation cursor cannot be re-encoded: %v", err)
		}
		if len(encoded) > maximumRelocationBytes {
			t.Fatalf("encoded accepted relocation cursor has %d bytes", len(encoded))
		}
	})
}
