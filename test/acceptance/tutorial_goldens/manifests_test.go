//go:build acceptance_c

package tutorialgoldens

import (
	"reflect"
	"testing"
)

type pageManifest struct {
	path     string
	commands []string
}

var tutorialPageManifests = []pageManifest{
	{
		path: "docs/tutorials/01-cities-and-rigs.md",
		commands: []string{
			"brew install gascity",
			"gc version",
			"gc init ~/my-city",
			"gc cities",
			"gc init ~/my-city --provider claude",
			"cd ~/my-city",
			"ls",
			"cat city.toml",
			"gc status",
			"gc rig add ~/my-project",
			"cat city.toml",
			"gc rig list",
			"cd ~/my-project",
			`gc sling my-project/claude "Write hello world in python to the file hello.py"`,
			"gc bd show mp-ff9 --watch",
			"ls",
		},
	},
	{
		path: "docs/tutorials/02-agents.md",
		commands: []string{
			"gc prime",
			"cat > prompts/reviewer.md << 'EOF'",
			"gc prime my-project/reviewer",
			"cd ~/my-project",
			`gc sling my-project/reviewer "Review hello.py and write review.md with feedback"`,
			"ls",
			"cat review.md",
		},
	},
	{
		path: "docs/tutorials/03-sessions.md",
		commands: []string{
			"cat city.toml",
			"gc session list --template my-project/reviewer",
			"gc session peek mc-8sfd",
			"gc session list",
			"gc session peek mayor --lines 3",
			"gc session attach mayor",
			`gc session nudge mayor "What's the current city status?"`,
			"gc session list",
			"gc session logs mayor --tail 1",
			"gc session logs mayor -f",
		},
	},
	{
		path: "docs/tutorials/04-communication.md",
		commands: []string{
			`gc mail send mayor -s "Review needed" -m "Please look at the auth module changes in my-project"`,
			"gc mail check mayor",
			"gc mail inbox mayor",
			`gc session nudge mayor "Check mail and hook status, then act accordingly"`,
			"gc session peek mayor --lines 6",
		},
	},
	{
		path: "docs/tutorials/05-formulas.md",
		commands: []string{
			"gc formula list",
			"gc formula show pancakes",
			"gc sling mayor pancakes --formula",
			"gc formula cook pancakes",
			"gc sling worker mp-2wx",
			`gc formula cook greeting --var name="Alice"`,
			"gc formula cook greeting",
			`gc formula show greeting --var name="Alice"`,
			`gc formula cook feature-work --var title="Auth overhaul" --var branch="develop"`,
			`gc formula cook feature-work --var title="Auth overhaul" --var priority="critical"`,
			`gc formula show feature-work --var title="Auth system"`,
			"gc formula show deploy-flow --var env=dev",
			"gc formula show deploy-flow --var env=staging",
			"gc formula show retry-deploy",
		},
	},
	{
		path: "docs/tutorials/06-beads.md",
		commands: []string{
			"cat city.toml",
			"bd list",
			`bd create "Fix the login bug"`,
			`bd create "Refactor auth module" --type feature`,
			"bd close mc-ykp",
			"bd list --status open --flat",
			"bd list --status in_progress --flat",
			"bd label add mc-a4l priority:high",
			"bd label add mc-a4l frontend",
			"bd list --label priority:high --flat",
			"bd update mc-a4l --set-metadata branch=feature/auth --set-metadata reviewer=sky",
			"bd dep mc-a4l --blocks mc-xp7",
			`gc convoy create "Sprint 42" mc-ykp mc-a4l mc-xp7`,
			"gc convoy status mc-d4g",
			`gc convoy create "Auth rewrite" --owned --target integration/auth`,
			"gc convoy land mc-0ud",
			"gc convoy add mc-d4g mc-xp7",
			"gc convoy check",
			"gc convoy stranded",
			`gc convoy create "Deploy v2" --owner mayor --merge mr --target main`,
			"gc convoy target mc-zk1 develop",
			"bd ready --metadata-field gc.routed_to=my-project/worker --unassigned --limit=1",
			"bd list --status open --type task --flat",
			"bd show mc-a4l",
			"bd close mc-a4l",
		},
	},
	{
		path: "docs/tutorials/07-orders.md",
		commands: []string{
			"gc order list",
			"gc order show review-check",
			"gc order check",
			"gc order run review-check",
			"gc order history",
			"gc order history review-check",
			"gc order list",
			"gc order show test-suite --rig my-api",
			"gc order run test-suite --rig my-api",
			"gc start",
			"gc order list",
			"gc order check",
		},
	},
}

func TestTutorialCommandInventoryMatchesPinnedDocs(t *testing.T) {
	snapshot := loadTutorialSnapshot(t)
	for _, manifest := range tutorialPageManifests {
		t.Run(manifest.path, func(t *testing.T) {
			page, ok := snapshot.pages[manifest.path]
			if !ok {
				t.Fatalf("snapshot missing page %s", manifest.path)
			}
			got := make([]string, 0, len(page.Commands))
			for _, cmd := range page.Commands {
				got = append(got, cmd.Text)
			}
			if !reflect.DeepEqual(got, manifest.commands) {
				t.Fatalf("command inventory drift for %s\nwant: %#v\ngot:  %#v", manifest.path, manifest.commands, got)
			}
		})
	}
}
