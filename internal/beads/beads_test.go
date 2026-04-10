package beads

import "testing"

func TestIsContainerType(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"convoy", true},
		{"epic", false},
		{"task", false},
		{"message", false},
		{"", false},
		{"CONVOY", false}, // case-sensitive
	}
	for _, tt := range tests {
		if got := IsContainerType(tt.typ); got != tt.want {
			t.Errorf("IsContainerType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestIsMoleculeType(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"molecule", true},
		{"wisp", true},
		{"task", false},
		{"convoy", false},
		{"step", false},
		{"", false},
		{"MOLECULE", false}, // case-sensitive
	}
	for _, tt := range tests {
		if got := IsMoleculeType(tt.typ); got != tt.want {
			t.Errorf("IsMoleculeType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}

func TestIsReadyExcludedType(t *testing.T) {
	tests := []struct {
		typ  string
		want bool
	}{
		{"merge-request", true},
		{"gate", true},
		{"molecule", true},
		{"message", true},
		{"agent", true},
		{"role", true},
		{"rig", true},
		{"task", false},
		{"convoy", false},
		{"wisp", false},
		{"", false},
		{"MOLECULE", false}, // case-sensitive
	}
	for _, tt := range tests {
		if got := IsReadyExcludedType(tt.typ); got != tt.want {
			t.Errorf("IsReadyExcludedType(%q) = %v, want %v", tt.typ, got, tt.want)
		}
	}
}
