package logutil

import "testing"

func TestDedupFirst(t *testing.T) {
	d := NewDedup(3)

	tests := []struct {
		key  string
		want bool
	}{
		{key: "a", want: true},
		{key: "a", want: false},
		{key: "b", want: true},
		{key: "c", want: true},
		{key: "b", want: false},
	}

	for _, tt := range tests {
		if got := d.First(tt.key); got != tt.want {
			t.Fatalf("First(%q) = %t, want %t", tt.key, got, tt.want)
		}
	}
}

func TestDedupEvictsLeastRecentlyUsedKey(t *testing.T) {
	d := NewDedup(2)

	if !d.First("a") || !d.First("b") {
		t.Fatal("initial keys should be first occurrences")
	}
	if d.First("a") {
		t.Fatal("a should still be present")
	}
	if !d.First("c") {
		t.Fatal("c should be new")
	}
	if !d.First("b") {
		t.Fatal("b should have been evicted as least recently used")
	}
}
