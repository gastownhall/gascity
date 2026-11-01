package tmux

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// bindClaudeQuestion names the native tool invocation, so an identical question
// raised again cannot accept a response to the first one. Older runtimes with
// no native identity stay visible and guarded, but offer terminal answers only.
func (t *Tmux) bindClaudeQuestion(target string, q *parsedQuestion) error {
	read := t.claudeQuestionTranscript
	if read == nil {
		read = t.readClaudeQuestionTranscript
	}
	tail, err := read(target)
	if err != nil {
		return err
	}
	if tail == "" {
		return nil
	}
	type question struct {
		Header   string `json:"header"`
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
		MultiSelect bool `json:"multiSelect"`
	}
	type call struct {
		ID        string
		Questions []question
	}
	var pending []call
	for _, line := range strings.Split(tail, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return fmt.Errorf("%w: native Claude question record is incomplete", runtime.ErrRuntimeUnavailable)
		}
		content := strings.TrimSpace(string(row.Message.Content))
		if !strings.HasPrefix(content, "[") {
			continue
		} // Plain user text cannot name a tool interaction.
		var items []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
			Input     struct {
				Questions []question `json:"questions"`
			} `json:"input"`
		}
		if err := json.Unmarshal(row.Message.Content, &items); err != nil {
			return fmt.Errorf("%w: native Claude question content is incomplete", runtime.ErrRuntimeUnavailable)
		}

		for _, item := range items {
			if row.Type == "assistant" && item.Type == "tool_use" && item.Name == "AskUserQuestion" && item.ID != "" {
				pending = append(pending, call{item.ID, item.Input.Questions})
			}
			if row.Type == "user" && item.Type == "tool_result" && item.ToolUseID != "" {
				keep := pending[:0]
				for _, c := range pending {
					if c.ID != item.ToolUseID {
						keep = append(keep, c)
					}
				}
				pending = keep
			}
		}
	}
	match := ""
	for _, c := range pending {
		for index, native := range c.Questions {
			var options []string
			for _, option := range native.Options {
				options = append(options, option.Label)
			}
			if native.MultiSelect || strings.Join(strings.Fields(native.Question), " ") != strings.Join(strings.Fields(q.Question), " ") || !equalQuestionChoices(options, q.answerChoices()) {
				continue
			}
			if match != "" {
				return fmt.Errorf("%w: native Claude question is ambiguous", runtime.ErrRuntimeUnavailable)
			}
			match = fmt.Sprintf("tmux-question-%s-%d", c.ID, index)
			q.Question = native.Question
		}
	}
	if match == "" {
		return fmt.Errorf("%w: visible question has no matching native Claude invocation", runtime.ErrRuntimeUnavailable)
	}
	q.NativeID = match
	return nil
}

func (t *Tmux) readClaudeQuestionTranscript(target string) (string, error) {
	env, err := t.GetAllEnvironment(target)
	if err != nil {
		return "", fmt.Errorf("reading question runtime identity: %w", err)
	}
	id := env["GC_PROVIDER_SESSION_ID"]
	if id == "" {
		return "", nil
	}
	workDir := env["GC_DIR"]
	if workDir == "" {
		return "", fmt.Errorf("%w: native question runtime has no work directory", runtime.ErrRuntimeUnavailable)
	}
	root := env["CLAUDE_CONFIG_DIR"]
	if root == "" {
		home := env["HOME"]
		if home == "" {
			home, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolving native question home: %w", err)
			}
		}
		root = filepath.Join(home, ".claude")
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(workDir, root)
	}
	path := sessionlog.FindSessionFileByID([]string{filepath.Join(root, "projects")}, workDir, id)
	if path == "" {
		return "", fmt.Errorf("%w: native question transcript is not yet available", runtime.ErrRuntimeUnavailable)
	}
	return readQuestionTranscriptTail(path)
}

func readQuestionTranscriptTail(path string) (string, error) {
	const limit = 8 * 1024 * 1024
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("reading native question transcript: %w", err)
	}
	tail, err := readFileTail(path, limit)
	if err != nil {
		return "", fmt.Errorf("reading native question transcript: %w", err)
	}
	if info.Size() > limit {
		_, rest, found := strings.Cut(tail, "\n")
		if !found {
			return "", fmt.Errorf("%w: native question record exceeds observation limit", runtime.ErrRuntimeUnavailable)
		}
		tail = rest
	}
	return tail, nil
}
