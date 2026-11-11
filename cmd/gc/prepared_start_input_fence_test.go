package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// Kills a controller start whose runtime env names another incarnation
// than session.InputFenceFor reads off the row it returns: a lane fence
// built from the row would then never match the runtime it started. Covers
// buildPreparedStart, shared by the legacy start and v2's prepareRow.
func TestPreparedStartRuntimeEnvMatchesSessionInputFence(t *testing.T) {
	raws := []string{"", "0", "-2", "x", " 3", "1", "7"}
	tokens := []string{"", "tok", "0123456789abcdef0123456789abcdef"}
	for i, gen := range raws {
		for j, token := range tokens {
			epoch := raws[(i+j+1)%len(raws)]
			t.Run(fmt.Sprintf("gen=%q epoch=%q token=%q", gen, epoch, token), func(t *testing.T) {
				store := beads.NewMemStore()
				meta := map[string]string{
					"session_name": "worker", "template": "worker", "state": "asleep",
					"generation": gen, "continuation_epoch": epoch, "instance_token": token,
				}
				bead, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
				if err != nil {
					t.Fatal(err)
				}
				candidate := startCandidate{
					info: sessiontest.SeedBead(t, bead),
					tp:   TemplateParams{TemplateName: "worker", SessionName: "worker", Command: "claude", EffectiveSessionProvider: "tmux"},
				}
				prepared, info, err := buildPreparedStart(candidate, &config.City{}, store)
				if err != nil {
					t.Fatalf("buildPreparedStart: %v", err)
				}
				fromEnv, ok := runtime.InputFenceFromEnv(prepared.cfg.Env)
				if !ok {
					t.Fatal("prepared start env names no incarnation")
				}
				if fromRow, ok := sessionpkg.InputFenceFor(info); !ok || fromRow != fromEnv {
					t.Fatalf("InputFenceFor(row) = %v, %v; the start env names %v", fromRow, ok, fromEnv)
				}
			})
		}
	}
}
