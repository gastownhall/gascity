package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

type startupSelectionFixture struct {
	name, heading, footer string
	choices               []string
	target, initial       int
	accept                func(context.Context, *startupDialogBudget, func(int) (string, error), func(...string) error) error
}

func startupSelectionFixtures() []startupSelectionFixture {
	return []startupSelectionFixture{
		{"resume", "This session is 1h old", "Enter to confirm · Esc to cancel", []string{"Resume from summary (recommended)", "Resume full session as-is", "Don't ask me again"}, 1, 0, acceptClaudeResumeDialog},
		{"mcp", "New MCP server found in this project: probe", "Enter to confirm · Esc to cancel", []string{"Use this MCP server", "Use this and all future MCP servers in this project", "Continue without using this MCP server"}, 1, 0, acceptMCPTrustDialog},
		{"hooks", "Hooks need review", "Press enter to confirm or esc to go back", []string{"Review hooks", "Trust all and continue", "Continue without trusting (hooks won't run)"}, 1, 0, acceptCodexHookReviewDialog},
		{"bypass", "Bypass Permissions mode", "Enter to confirm · Esc to cancel", []string{"No, exit", "Yes, I accept"}, 1, 0, acceptBypassPermissionsWarning},
		{"api key", "Detected a custom API key in your environment\nDo you want to use this API key?", "Enter to confirm · Esc to cancel", []string{"Yes", "No (recommended)"}, 0, 1, acceptCustomAPIKeyDialog},
		{"rate limit", "Usage limit reached for gemini-3-flash-preview.", "Enter to confirm · Esc to cancel", []string{"Keep trying", "Stop"}, 1, 0, dismissRateLimitDialog},
	}
}

func (f startupSelectionFixture) frame(selected int) string {
	var b strings.Builder
	b.WriteString(f.heading + "\n")
	for i, choice := range f.choices {
		marker := "  "
		if selected == i {
			marker = "❯ "
		}
		fmt.Fprintf(&b, "%s%d. %s\n", marker, i+1, choice)
	}
	return b.String() + f.footer
}

// These invoke each existing phase directly: old production compiles, but its
// fixed move/Enter sequence cannot prove a reread selected the requested row.
func TestStartupSelectionConfirmsEachKnownModalAfterReread(t *testing.T) {
	for _, fixture := range startupSelectionFixtures() {
		for _, mode := range []string{"normal", "dropped first move", "first paint reset", "passive header and composer"} {
			t.Run(fixture.name+"/"+mode, func(t *testing.T) {
				withZeroDialogTimings(t)
				useSteppedDialogClock(t)
				dialogPollInterval = time.Millisecond
				bypassDialogConfirmDelay = time.Millisecond
				selected, moves, targetReads, resets := fixture.initial, 0, 0, 0
				var sent []string
				err := fixture.accept(t.Context(), newStartupDialogBudget(time.Second), func(int) (string, error) {
					if mode == "first paint reset" && targetReads == 1 && resets == 0 {
						selected = fixture.initial
						targetReads = 0
						resets++
					}
					if selected == fixture.target {
						targetReads++
					} else {
						targetReads = 0
					}
					frame := fixture.frame(selected)
					if mode == "passive header and composer" {
						prefix := ">_ OpenAI Codex (v0.162.1)\n› Ask Codex to do anything\n\n"
						if fixture.name == "rate limit" {
							prefix = "Gemini CLI\n› Ask a question\n\n"
						}
						frame = prefix + frame
					}
					return frame, nil
				}, func(keys ...string) error {
					for _, key := range keys {
						sent = append(sent, key)
						switch key {
						case "Down", "Up":
							moves++
							if mode == "dropped first move" && moves == 1 {
								continue
							}
							if key == "Down" {
								selected++
							} else {
								selected--
							}
						case "Enter":
							if selected != fixture.target || targetReads < 2 {
								t.Errorf("confirmed without two current selected frames: selected=%d reads=%d", selected, targetReads)
							}
						default:
							t.Errorf("unexpected key %q", key)
						}
					}
					return nil
				})
				if err != nil {
					t.Fatalf("selection should recover safely: %v", err)
				}
				if len(sent) == 0 || sent[len(sent)-1] != "Enter" {
					t.Fatalf("did not confirm target: %v", sent)
				}
			})
		}
	}
}

func TestStartupSelectionRefusesDroppedUnknownOrReplacedFrames(t *testing.T) {
	for _, fixture := range startupSelectionFixtures() {
		for _, mode := range []string{"all moves dropped", "question after move", "missing cursor", "duplicate cursor", "partial choices", "late initial frame", "late target frame", "same menu question footer", "trailing question", "unknown extra option"} {
			t.Run(fixture.name+"/"+mode, func(t *testing.T) {
				withZeroDialogTimings(t)
				clock := useSteppedDialogClock(t)
				dialogPollInterval = time.Millisecond
				bypassDialogConfirmDelay = time.Millisecond
				selected, moves := fixture.initial, 0
				var sent []string
				budget := newStartupDialogBudget(100 * time.Millisecond)
				originalDeadline := budget.deadline
				_ = fixture.accept(t.Context(), budget, func(int) (string, error) {
					frame := fixture.frame(selected)
					switch mode {
					case "question after move":
						if moves > 0 {
							return "Which animal?\n❯ 1. Otter\n  2. Owl\nEnter to select", nil
						}
					case "same menu question footer":
						frame = strings.Replace(frame, fixture.footer, "Enter to select · Esc to cancel", 1)
					case "trailing question":
						frame += "\nWhich animal?\n❯ 1. Otter\n  2. Owl\nEnter to select"
					case "unknown extra option":
						frame = strings.Replace(frame, fixture.footer, "  9. Unrecognized extra choice\n"+fixture.footer, 1)
					case "missing cursor":
						frame = strings.ReplaceAll(frame, "❯ ", "  ")
					case "duplicate cursor":
						frame += "\n❯ 1. Another native question"
					case "partial choices":
						frame = strings.Replace(frame, fixture.choices[fixture.target], "unrecognized option", 1)
					case "late initial frame":
						clock.advance(101 * time.Millisecond)
					case "late target frame":
						if moves > 0 {
							clock.advance(101 * time.Millisecond)
						}
					}
					return frame, nil
				}, func(keys ...string) error {
					for _, key := range keys {
						sent = append(sent, key)
						if key == "Enter" {
							t.Error("unproven selected frame received Enter")
						}
						if key == "Down" || key == "Up" {
							moves++
							if mode != "all moves dropped" {
								if key == "Down" {
									selected++
								} else {
									selected--
								}
							}
						}
					}
					return nil
				})
				if slices.Contains(sent, "Enter") {
					t.Fatalf("unsafe confirmation: %v", sent)
				}
				if moves > 3 {
					t.Fatalf("unbounded selection moves: %d", moves)
				}
				if (mode == "late initial frame" || mode == "late target frame") && budget.deadline.After(originalDeadline) {
					t.Fatalf("expired observation reset original deadline: %v -> %v", originalDeadline, budget.deadline)
				}
			})
		}
	}
}
