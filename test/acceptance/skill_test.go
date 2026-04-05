//go:build acceptance_a

// Skill command acceptance tests.
//
// These exercise gc skill as a black box: listing available topics
// and displaying individual topic references.
package acceptance_test

import (
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestSkillCommands(t *testing.T) {
	t.Run("ListTopics", func(t *testing.T) {
		out, err := helpers.RunGC(testEnv, "", "skill")
		if err != nil {
			t.Fatalf("gc skill failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc skill produced empty output")
		}
	})

	t.Run("WorkTopic", func(t *testing.T) {
		out, err := helpers.RunGC(testEnv, "", "skill", "work")
		if err != nil {
			t.Fatalf("gc skill work failed: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc skill work produced empty output")
		}
	})

	t.Run("UnknownTopic", func(t *testing.T) {
		_, err := helpers.RunGC(testEnv, "", "skill", "nonexistent-topic-xyz")
		if err == nil {
			t.Fatal("expected error for unknown skill topic")
		}
	})
}
