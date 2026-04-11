//go:build acceptance_c

package tutorialgoldens

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

type tutorialSnapshot struct {
	pages map[string]*tutorialPage
}

type tutorialPage struct {
	RelativePath string
	Title        string
	Commands     []tutorialCommand
	Snippets     []tutorialSnippet
}

type tutorialCommand struct {
	Section string
	Line    int
	Text    string
}

type tutorialSnippet struct {
	Section  string
	Language string
	Line     int
	Body     string
}

var (
	snapshotOnce sync.Once
	snapshotData *tutorialSnapshot
	snapshotErr  error
)

func loadTutorialSnapshot(t *testing.T) *tutorialSnapshot {
	t.Helper()
	snapshotOnce.Do(func() {
		snapshotData, snapshotErr = parseTutorialSnapshot()
	})
	if snapshotErr != nil {
		t.Fatalf("loading tutorial snapshot: %v", snapshotErr)
	}
	return snapshotData
}

func parseTutorialSnapshot() (*tutorialSnapshot, error) {
	files := []string{
		"01-cities-and-rigs.md",
		"02-agents.md",
		"03-sessions.md",
		"04-communication.md",
		"05-formulas.md",
		"06-beads.md",
		"07-orders.md",
	}
	root := filepath.Join(helpers.FindModuleRoot(), canonicalTutorialRoot)
	s := &tutorialSnapshot{pages: make(map[string]*tutorialPage, len(files))}
	for _, name := range files {
		rel := filepath.Join("docs", "tutorials", name)
		page, err := parseTutorialPage(filepath.Join(root, name), rel)
		if err != nil {
			return nil, err
		}
		s.pages[rel] = page
	}
	return s, nil
}

func parseTutorialPage(path, rel string) (*tutorialPage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	page := &tutorialPage{RelativePath: rel}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		lineNo           int
		currentSection   string
		inFence          bool
		fenceLang        string
		inMDXComment     bool
		currentSnippet   []string
		currentSnippetLn int
	)

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if strings.Contains(trimmed, "{/*") {
			inMDXComment = true
		}
		if inMDXComment {
			if strings.Contains(trimmed, "*/}") {
				inMDXComment = false
			}
			continue
		}

		if strings.HasPrefix(trimmed, "title: ") && page.Title == "" {
			page.Title = strings.Trim(strings.TrimPrefix(trimmed, "title: "), `"`)
		}
		if strings.HasPrefix(trimmed, "## ") {
			currentSection = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
		}

		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				page.Snippets = append(page.Snippets, tutorialSnippet{
					Section:  currentSection,
					Language: fenceLang,
					Line:     currentSnippetLn,
					Body:     strings.Join(currentSnippet, "\n"),
				})
				inFence = false
				fenceLang = ""
				currentSnippet = nil
				currentSnippetLn = 0
				continue
			}
			inFence = true
			fenceLang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			currentSnippetLn = lineNo + 1
			currentSnippet = nil
			continue
		}

		if !inFence {
			continue
		}
		currentSnippet = append(currentSnippet, line)
		if fenceLang == "shell" && strings.HasPrefix(strings.TrimSpace(line), "$ ") {
			page.Commands = append(page.Commands, tutorialCommand{
				Section: currentSection,
				Line:    lineNo,
				Text:    strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "$ ")),
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s: %w", rel, err)
	}
	return page, nil
}
