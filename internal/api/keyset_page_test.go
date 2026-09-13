package api

import (
	"fmt"
	"testing"
	"time"
)

type kpRow struct {
	id string
	at time.Time
}

func kpKey(r kpRow) keysetKey { return keysetKey{CreatedAt: r.at, ID: r.id} }

func TestSortKeysetDescTotalOrderWithTies(t *testing.T) {
	ts := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	rows := []kpRow{
		{"b", ts}, {"a", ts.Add(time.Second)}, {"c", ts}, {"d", ts.Add(-time.Second)}, {"a2", ts},
	}
	sortKeysetDesc(rows, kpKey)
	want := []string{"a", "c", "b", "a2", "d"} // newest first; ties by ID DESC
	for i, w := range want {
		if rows[i].id != w {
			t.Fatalf("rows[%d] = %s, want %s (order: %v)", i, rows[i].id, w, rows)
		}
	}
}

func TestResolveKeysetPageWalkNoSkipNoDupWithTies(t *testing.T) {
	ts := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	var rows []kpRow
	for i := 0; i < 17; i++ {
		rows = append(rows, kpRow{id: fmt.Sprintf("m-%02d", i), at: ts.Add(time.Duration(i%3) * time.Second)})
	}
	sortKeysetDesc(rows, kpKey)

	seen := map[string]int{}
	var seek *keysetKey
	pages := 0
	for {
		page, total, hasMore := resolveKeysetPage(rows, kpKey, seek, 4)
		if total != len(rows) {
			t.Fatalf("total = %d, want %d (must keep full-set meaning)", total, len(rows))
		}
		for _, r := range page {
			seen[r.id]++
		}
		if !hasMore {
			break
		}
		tok := mintKeysetNextCursor(page, kpKey, hasMore)
		if tok == "" {
			t.Fatal("hasMore page minted no cursor")
		}
		var err error
		seek, err = keysetSeek(tok)
		if err != nil {
			t.Fatalf("server-minted cursor rejected: %v", err)
		}
		if pages++; pages > 10 {
			t.Fatal("walk did not terminate")
		}
	}
	if len(seen) != len(rows) {
		t.Fatalf("walk saw %d rows, want %d", len(seen), len(rows))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s seen %d times, want 1", id, n)
		}
	}
}

func TestKeysetSeekContract(t *testing.T) {
	if b, err := keysetSeek(""); b != nil || err != nil {
		t.Fatalf("empty cursor = (%v, %v), want (nil, nil)", b, err)
	}
	if _, err := keysetSeek("NTA"); err == nil {
		t.Fatal("legacy offset cursor must be rejected")
	}
	if _, err := keysetSeek(encodeKeysetCursor(keysetCursor{Kind: cursorKindSeq, Seq: 3})); err == nil {
		t.Fatal("seq-kind cursor must be rejected on cb collections")
	}
}
