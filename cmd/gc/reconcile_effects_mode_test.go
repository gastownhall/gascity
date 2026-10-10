package main

import "testing"

// Kills the knob enabling effects on its own, or without its exact value
// (D-14): effects run only under the skeleton override with the knob "1".
func TestV2EffectsEnabledNeedsOverrideAndKnob(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{v2EffectsEnv: "1"}, false},
		{map[string]string{v2SkeletonEnv: "1"}, false},
		{map[string]string{v2SkeletonEnv: "1", v2EffectsEnv: "true"}, false},
		{map[string]string{v2SkeletonEnv: "1", v2EffectsEnv: "1"}, true},
	} {
		lookup := func(k string) (string, bool) { v, ok := c.env[k]; return v, ok }
		if got := v2EffectsEnabled(lookup); got != c.want {
			t.Errorf("env %v: enabled %v, want %v", c.env, got, c.want)
		}
	}
	if v2EffectsEnabled(nil) {
		t.Error("a nil lookup enabled effects")
	}
}
