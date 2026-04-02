package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestExtractRigFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantRig  string
		wantArgs []string
	}{
		{
			name:     "no rig flag",
			args:     []string{"list", "--limit", "5"},
			wantRig:  "",
			wantArgs: []string{"list", "--limit", "5"},
		},
		{
			name:     "rig flag with space",
			args:     []string{"--rig", "myproject", "list"},
			wantRig:  "myproject",
			wantArgs: []string{"list"},
		},
		{
			name:     "rig flag with equals",
			args:     []string{"--rig=myproject", "list"},
			wantRig:  "myproject",
			wantArgs: []string{"list"},
		},
		{
			name:     "rig flag in middle",
			args:     []string{"show", "--rig", "myproject", "BL-42"},
			wantRig:  "myproject",
			wantArgs: []string{"show", "BL-42"},
		},
		{
			name:     "empty args",
			args:     nil,
			wantRig:  "",
			wantArgs: nil,
		},
		{
			name:     "rig flag at end missing value",
			args:     []string{"list", "--rig"},
			wantRig:  "",
			wantArgs: []string{"list", "--rig"},
		},
	}

	// Save and restore global rigFlag.
	origRigFlag := rigFlag
	defer func() { rigFlag = origRigFlag }()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rigFlag = "" // reset global
			gotRig, gotArgs := extractRigFlag(tt.args)
			if gotRig != tt.wantRig {
				t.Errorf("rig = %q, want %q", gotRig, tt.wantRig)
			}
			if len(gotArgs) != len(tt.wantArgs) {
				t.Fatalf("args len = %d, want %d; got %v", len(gotArgs), len(tt.wantArgs), gotArgs)
			}
			for i := range gotArgs {
				if gotArgs[i] != tt.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, gotArgs[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestExtractRigFlagFallsBackToGlobal(t *testing.T) {
	origRigFlag := rigFlag
	defer func() { rigFlag = origRigFlag }()

	rigFlag = "from-global"
	gotRig, gotArgs := extractRigFlag([]string{"list"})
	if gotRig != "from-global" {
		t.Errorf("rig = %q, want %q", gotRig, "from-global")
	}
	if len(gotArgs) != 1 || gotArgs[0] != "list" {
		t.Errorf("args = %v, want [list]", gotArgs)
	}
}

func TestResolveBdDir(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "wren", Path: "/projects/wren", Prefix: "projectwrenunity"},
			{Name: "gascity", Path: "/projects/gascity"},
		},
	}

	tests := []struct {
		name    string
		rigName string
		args    []string
		wantDir string
	}{
		{
			name:    "explicit rig name",
			rigName: "wren",
			args:    []string{"list"},
			wantDir: "/projects/wren",
		},
		{
			name:    "explicit rig name case insensitive",
			rigName: "Wren",
			args:    []string{"list"},
			wantDir: "/projects/wren",
		},
		{
			name:    "auto-detect from bead prefix",
			rigName: "",
			args:    []string{"show", "projectwrenunity-0xk"},
			wantDir: "/projects/wren",
		},
		{
			name:    "no rig falls back to city",
			rigName: "",
			args:    []string{"list"},
			wantDir: "/city",
		},
		{
			name:    "unknown rig name falls back to auto-detect",
			rigName: "nonexistent",
			args:    []string{"show", "projectwrenunity-abc"},
			wantDir: "/projects/wren",
		},
		{
			name:    "skips flags during auto-detect",
			rigName: "",
			args:    []string{"list", "--status", "open"},
			wantDir: "/city",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveBdDir(cfg, "/city", tt.rigName, tt.args)
			if got != tt.wantDir {
				t.Errorf("resolveBdDir() = %q, want %q", got, tt.wantDir)
			}
		})
	}
}
