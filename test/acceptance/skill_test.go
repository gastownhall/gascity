//go:build acceptance_a

// Skill command acceptance tests.
//
// These exercise gc skills as a black box: listing available built-in
// topics and displaying individual topic references.
package acceptance_test

import (
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestSkillCommands(t *testing.T) {
	t.Run("ListTopics", func(t *testing.T) {
		out, err := helpers.RunGC(testEnv, "", "skills")
		if err != nil {
			t.Fatalf("gc skills failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc skills produced empty output")
		}
	})

	t.Run("WorkTopic", func(t *testing.T) {
		out, err := helpers.RunGC(testEnv, "", "skills", "work")
		if err != nil {
			t.Fatalf("gc skills work failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc skills work produced empty output")
		}
	})

	t.Run("UnknownTopic", func(t *testing.T) {
		_, err := helpers.RunGC(testEnv, "", "skills", "nonexistent-topic-xyz")
		if err == nil {
			t.Fatal("expected error for unknown skill topic")
		}
	})
}
