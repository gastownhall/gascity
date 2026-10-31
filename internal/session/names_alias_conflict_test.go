package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// Kills a conflict source dropped from the shared alias rule (the C7d-1
// review's G15): another open row holds the alias by its session name, its
// alias or its concrete agent name (unless the claimant owns that name), a
// configured named session reserves its own; a closed row, a released
// failed create and the claimant's own row hold nothing.
func TestAliasConflictInfo(t *testing.T) {
	cfg := &config.City{NamedSessions: []config.NamedSession{{Name: "boss", Template: "worker"}}}
	row := func(id string, mod func(*Info)) Info {
		i := Info{ID: id, Type: BeadType}
		mod(&i)
		return i
	}
	for _, tc := range []struct {
		name, alias, owner string
		rows               []Info
		self               *Info
		taken              bool
	}{
		{"free", "solo", "", nil, nil, false},
		{"another row's session name", "solo", "", []Info{row("gc-9", func(i *Info) { i.SessionNameMetadata = "solo" })}, nil, true},
		{"another row's alias", "solo", "", []Info{row("gc-9", func(i *Info) { i.Alias = "solo" })}, nil, true},
		{"another row's agent name", "solo", "", []Info{row("gc-9", func(i *Info) { i.AgentName = "solo" })}, nil, true},
		{"an agent name the claimant owns", "solo", "solo", []Info{row("gc-9", func(i *Info) { i.AgentName = "solo" })}, nil, false},
		{"a closed row", "solo", "", []Info{row("gc-9", func(i *Info) { i.Alias, i.Closed = "solo", true })}, nil, false},
		{"a released failed create", "solo", "", []Info{row("gc-9", func(i *Info) { i.Alias, i.MetadataState = "solo", string(StateFailedCreate) })}, nil, false},
		{"the claimant's own row", "solo", "", []Info{row("gc-1", func(i *Info) { i.Alias = "solo" })}, nil, false},
		{"a named reservation", "boss", "", nil, nil, true},
		{"a named reservation its owner claims", "boss", "boss", nil, nil, false},
		{"a named reservation its own row claims", "boss", "", nil, &Info{ID: "gc-1", Type: BeadType, ConfiguredNamedSession: true, ConfiguredNamedIdentity: "boss"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := AliasConflictInfo(tc.rows, cfg, tc.alias, "gc-1", tc.owner, tc.self)
			if (err != nil) != tc.taken || err != nil && !errors.Is(err, ErrSessionAliasExists) {
				t.Fatalf("AliasConflictInfo = %v, want taken %v", err, tc.taken)
			}
		})
	}
}
