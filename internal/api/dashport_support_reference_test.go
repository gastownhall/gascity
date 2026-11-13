//go:build integration

package api

import "github.com/gastownhall/gascity/internal/beads"

// t7b2FrozenSeededBeadStore is main's seededState.BeadStore (origin/main
// 8d2fb3120c, dashport_support.go), which T7d.2 deleted with
// State.BeadStore: the frozen reference copies still read a seeded state's rig
// store through it (t7b2FrozenBeadStore).
func (s *seededState) t7b2FrozenSeededBeadStore(rig string) beads.Store { return s.rigStores[rig] }
