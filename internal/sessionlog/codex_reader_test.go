package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCodexFileNormalizesExecCommandReadInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path, map[string]any{
		"timestamp": "2026-06-01T00:00:00Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type":      "function_call",
			"call_id":   "call-read",
			"name":      "exec_command",
			"arguments": `{"cmd":"nl -ba src/app.ts | sed -n '12,14p'"}`,
		},
	})

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	input := codexReaderToolInput(t, sess, "call-read")
	if got := jsonStringValue(input["command"]); got != "nl -ba src/app.ts | sed -n '12,14p'" {
		t.Fatalf("command = %q, want original shell command; input = %s", got, mustMarshal(input))
	}
	if got := jsonStringValue(input["file_path"]); got != "src/app.ts" {
		t.Fatalf("file_path = %q, want src/app.ts; input = %s", got, mustMarshal(input))
	}
	if _, ok := input["cmd"]; ok {
		t.Fatalf("input leaked native cmd key: %s", mustMarshal(input))
	}
}

func TestReadProviderFileCodexDisambiguatesRepeatedResponseItemRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	row := map[string]any{
		"timestamp": "2026-06-01T00:00:00Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "Done."}},
		},
	}
	// Two byte-identical response_item rows (no distinguishing timestamp) must not
	// collide onto one entry ID and hard-fail the uniqueness gate.
	writeCodexReaderFixture(t, path, row, row)

	sess, err := ReadProviderFile("codex/tmux-cli", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile with byte-identical response_item rows: %v", err)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want two entries", len(sess.Messages))
	}
	if sess.Messages[0].UUID == sess.Messages[1].UUID {
		t.Fatalf("byte-identical response_item rows share entry ID %q", sess.Messages[0].UUID)
	}
}

func TestReadCodexFilePreservesMessageImageBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path, map[string]any{
		"timestamp": "2026-06-01T00:00:00Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "inspect this screenshot"},
				{"type": "input_image", "file_path": "screens/shot.png", "mime_type": "image/png", "image_url": "https://example.com/shot.png"},
				{"type": "input_image", "file_path": "screens/local.png", "mime_type": "image/png", "image_url": "data:image/png;base64,ignored"},
			},
		},
	})

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	if len(sess.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(sess.Messages))
	}
	blocks := sess.Messages[0].ContentBlocks()
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v, want text plus two images", blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "inspect this screenshot" {
		t.Fatalf("blocks[0] = %+v, want text prompt", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].FilePath != "screens/shot.png" || blocks[1].MIMEType != "image/png" || blocks[1].ImageURL != "https://example.com/shot.png" {
		t.Fatalf("blocks[1] = %+v, want external image metadata", blocks[1])
	}
	if blocks[2].Type != "image" || blocks[2].FilePath != "screens/local.png" || blocks[2].MIMEType != "image/png" {
		t.Fatalf("blocks[2] = %+v, want local image metadata", blocks[2])
	}
	if blocks[2].ImageURL != "" {
		t.Fatalf("blocks[2].ImageURL = %q, want inline data URL omitted from structured block", blocks[2].ImageURL)
	}
}

func TestReadCodexFileNormalizesExecCommandGrepInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path, map[string]any{
		"timestamp": "2026-06-01T00:00:00Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type":      "function_call",
			"call_id":   "call-grep",
			"name":      "exec_command",
			"arguments": `{"cmd":"rg -n \"needle\" README.md src/app.ts"}`,
		},
	})

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	input := codexReaderToolInput(t, sess, "call-grep")
	if got := jsonStringValue(input["command"]); got != `rg -n "needle" README.md src/app.ts` {
		t.Fatalf("command = %q, want original shell command; input = %s", got, mustMarshal(input))
	}
	if got := jsonStringValue(input["pattern"]); got != "needle" {
		t.Fatalf("pattern = %q, want needle; input = %s", got, mustMarshal(input))
	}
	var paths []string
	if err := json.Unmarshal(input["paths"], &paths); err != nil {
		t.Fatalf("paths are not a string array in %s: %v", mustMarshal(input), err)
	}
	if len(paths) != 2 || paths[0] != "README.md" || paths[1] != "src/app.ts" {
		t.Fatalf("paths = %#v, want README.md/src/app.ts; input = %s", paths, mustMarshal(input))
	}
	if _, ok := input["cmd"]; ok {
		t.Fatalf("input leaked native cmd key: %s", mustMarshal(input))
	}
}

func TestReadCodexFileNormalizesWebSearchInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path, map[string]any{
		"timestamp": "2026-06-01T00:00:00Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type":    "web_search_call",
			"call_id": "call-search",
			"name":    "web_search_call",
			"query":   "weather tomorrow",
			"input": map[string]any{
				"query":  "ignored fallback",
				"scope":  "web",
				"region": "US",
			},
			"action": map[string]any{
				"type":   "search",
				"source": "web",
			},
		},
	})

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	input := codexReaderToolInput(t, sess, "call-search")
	if got := jsonStringValue(input["query"]); got != "weather tomorrow" {
		t.Fatalf("query = %q, want top-level query; input = %s", got, mustMarshal(input))
	}
	if got := jsonStringValue(input["scope"]); got != "web" {
		t.Fatalf("scope = %q, want web; input = %s", got, mustMarshal(input))
	}
	if got := jsonStringValue(input["region"]); got != "US" {
		t.Fatalf("region = %q, want US; input = %s", got, mustMarshal(input))
	}
	action := jsonStringValue(input["action"])
	if !strings.Contains(action, `"source":"web"`) || !strings.Contains(action, `"type":"search"`) {
		t.Fatalf("action = %q, want compact neutral JSON string; input = %s", action, mustMarshal(input))
	}
}

func TestReadCodexFileNormalizesWebSearchResultItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "web_search_call",
				"call_id": "call-search",
				"name":    "web_search_call",
				"query":   "structured stream format",
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-search",
				"output":  "Output:\nhttps://example.com/structured: Structured Stream Format\nhttps://example.com/mc - MC Data Algorithms\n",
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	result := codexReaderToolResultContent(t, sess, "call-search")
	if got := jsonStringValue(result["query"]); got != "structured stream format" {
		t.Fatalf("query = %q, want structured stream format; result = %s", got, mustMarshal(result))
	}
	var items []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(result["result_items"], &items); err != nil {
		t.Fatalf("result_items are not neutral item array in %s: %v", mustMarshal(result), err)
	}
	if len(items) != 2 || items[0].Title != "Structured Stream Format" || items[0].URL != "https://example.com/structured" {
		t.Fatalf("result_items = %#v, want typed title/url results", items)
	}
	if got, ok := jsonIntValue(result["num_results"]); !ok || got != 2 {
		t.Fatalf("num_results = %d/%v, want 2/true; result = %s", got, ok, mustMarshal(result))
	}
	for _, forbidden := range []string{"results", "content"} {
		if strings.Contains(string(result["result_items"]), forbidden) {
			t.Fatalf("result_items leaked provider-native key %q: %s", forbidden, result["result_items"])
		}
	}
}

func TestReadCodexFileNormalizesExecCommandReadResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-read",
				"name":      "exec_command",
				"arguments": `{"cmd":"sed -n '12,14p' src/app.ts"}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-read",
				"output":  "Command: sed -n '12,14p' src/app.ts\nOutput:\nline 12\nline 13\nline 14\n",
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	result := codexReaderToolResultContent(t, sess, "call-read")
	if got := jsonStringValue(result["output"]); got != "line 12\nline 13\nline 14\n" {
		t.Fatalf("output = %q, want wrapper-stripped read output; result = %s", got, mustMarshal(result))
	}
	if got := jsonStringValue(result["content"]); got != "line 12\nline 13\nline 14\n" {
		t.Fatalf("content = %q, want neutral read content; result = %s", got, mustMarshal(result))
	}
	if got := jsonStringValue(result["file_path"]); got != "src/app.ts" {
		t.Fatalf("file_path = %q, want src/app.ts; result = %s", got, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["start_line"]); !ok || got != 12 {
		t.Fatalf("start_line = %d/%v, want 12/true; result = %s", got, ok, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["total_lines"]); !ok || got != 14 {
		t.Fatalf("total_lines = %d/%v, want 14/true; result = %s", got, ok, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["num_lines"]); !ok || got != 3 {
		t.Fatalf("num_lines = %d/%v, want 3/true; result = %s", got, ok, mustMarshal(result))
	}
	for _, key := range []string{"cmd", "Command:", "Output:"} {
		if _, ok := result[key]; ok {
			t.Fatalf("result leaked native/wrapper key %q: %s", key, mustMarshal(result))
		}
	}
}

func TestReadCodexFileNormalizesExecCommandNumberedReadResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-read",
				"name":      "exec_command",
				"arguments": `{"cmd":"nl -ba src/app.ts | sed -n '12,13p'"}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-read",
				"output":  "Command: nl -ba src/app.ts | sed -n '12,13p'\nOutput:\n    12\tline 12\n    13\tline 13\n",
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	result := codexReaderToolResultContent(t, sess, "call-read")
	if got := jsonStringValue(result["output"]); got != "    12\tline 12\n    13\tline 13\n" {
		t.Fatalf("output = %q, want original numbered output; result = %s", got, mustMarshal(result))
	}
	if got := jsonStringValue(result["content"]); got != "line 12\nline 13\n" {
		t.Fatalf("content = %q, want line-number-stripped read content; result = %s", got, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["start_line"]); !ok || got != 12 {
		t.Fatalf("start_line = %d/%v, want 12/true; result = %s", got, ok, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["num_lines"]); !ok || got != 2 {
		t.Fatalf("num_lines = %d/%v, want 2/true; result = %s", got, ok, mustMarshal(result))
	}
}

func TestReadCodexFileNormalizesExecCommandGrepResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-grep",
				"name":      "exec_command",
				"arguments": `{"cmd":"rg -n \"needle\" README.md src/app.ts"}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-grep",
				"output":  "Command: rg -n \"needle\" README.md src/app.ts\nOutput:\nREADME.md:1:needle\nsrc/app.ts:7:needle\n",
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	result := codexReaderToolResultContent(t, sess, "call-grep")
	if got := jsonStringValue(result["output"]); got != "README.md:1:needle\nsrc/app.ts:7:needle\n" {
		t.Fatalf("output = %q, want wrapper-stripped grep output; result = %s", got, mustMarshal(result))
	}
	if got := jsonStringValue(result["content"]); got != "README.md:1:needle\nsrc/app.ts:7:needle\n" {
		t.Fatalf("content = %q, want neutral grep content; result = %s", got, mustMarshal(result))
	}
	if got := jsonStringValue(result["pattern"]); got != "needle" {
		t.Fatalf("pattern = %q, want needle; result = %s", got, mustMarshal(result))
	}
	if got := jsonStringValue(result["mode"]); got != "content" {
		t.Fatalf("mode = %q, want content; result = %s", got, mustMarshal(result))
	}
	filenames := codexReaderStringSlice(t, result["filenames"])
	if len(filenames) != 2 || filenames[0] != "README.md" || filenames[1] != "src/app.ts" {
		t.Fatalf("filenames = %#v, want README.md/src/app.ts; result = %s", filenames, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["num_files"]); !ok || got != 2 {
		t.Fatalf("num_files = %d/%v, want 2/true; result = %s", got, ok, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["num_lines"]); !ok || got != 2 {
		t.Fatalf("num_lines = %d/%v, want 2/true; result = %s", got, ok, mustMarshal(result))
	}
}

func TestReadCodexFileStripsLiveExecCommandWrapperForReadAndGrep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-read",
				"name":      "exec_command",
				"arguments": `{"cmd":"sed -n '1,3p' /tmp/sample.txt"}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-read",
				"output":  "Chunk ID: 434495\nWall time: 0.0000 seconds\nProcess exited with code 0\nOriginal token count: 8\nOutput:\nalpha\nbeta\nneedle codex claude\n",
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:02Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-grep",
				"name":      "exec_command",
				"arguments": `{"cmd":"rg -n needle /tmp/sample.txt /tmp/data.json"}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:03Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-grep",
				"output":  "Chunk ID: 414539\nWall time: 0.0000 seconds\nProcess exited with code 0\nOriginal token count: 34\nOutput:\n/tmp/sample.txt:3:needle codex claude\n/tmp/data.json:1:{\"name\":\"live-rich\",\"needle\":true}\n",
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	read := codexReaderToolResultContent(t, sess, "call-read")
	if got := jsonStringValue(read["content"]); got != "alpha\nbeta\nneedle codex claude\n" {
		t.Fatalf("read content = %q, want wrapper-stripped payload; result = %s", got, mustMarshal(read))
	}
	if got := jsonStringValue(read["output"]); got != "alpha\nbeta\nneedle codex claude\n" {
		t.Fatalf("read output = %q, want wrapper-stripped payload; result = %s", got, mustMarshal(read))
	}

	grep := codexReaderToolResultContent(t, sess, "call-grep")
	if got := jsonStringValue(grep["content"]); got != "/tmp/sample.txt:3:needle codex claude\n/tmp/data.json:1:{\"name\":\"live-rich\",\"needle\":true}\n" {
		t.Fatalf("grep content = %q, want wrapper-stripped payload; result = %s", got, mustMarshal(grep))
	}
	filenames := codexReaderStringSlice(t, grep["filenames"])
	if len(filenames) != 2 || filenames[0] != "/tmp/data.json" || filenames[1] != "/tmp/sample.txt" {
		t.Fatalf("filenames = %#v, want only matched files; result = %s", filenames, mustMarshal(grep))
	}
	if got, ok := jsonIntValue(grep["num_files"]); !ok || got != 2 {
		t.Fatalf("num_files = %d/%v, want 2/true; result = %s", got, ok, mustMarshal(grep))
	}
}

func TestReadCodexFileNormalizesExecCommandGrepCountResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-grep",
				"name":      "exec_command",
				"arguments": `{"cmd":"rg -c \"needle\" README.md src/app.ts"}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-grep",
				"output":  "Command: rg -c \"needle\" README.md src/app.ts\nOutput:\nREADME.md:2\nsrc/app.ts:5\n",
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	result := codexReaderToolResultContent(t, sess, "call-grep")
	if got := jsonStringValue(result["mode"]); got != "count" {
		t.Fatalf("mode = %q, want count; result = %s", got, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["num_results"]); !ok || got != 7 {
		t.Fatalf("num_results = %d/%v, want 7/true; result = %s", got, ok, mustMarshal(result))
	}
	var counts []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(result["counts"], &counts); err != nil {
		t.Fatalf("counts are not neutral argument array in %s: %v", mustMarshal(result), err)
	}
	if len(counts) != 2 || counts[0].Name != "README.md" || counts[0].Value != "2" || counts[1].Name != "src/app.ts" || counts[1].Value != "5" {
		t.Fatalf("counts = %#v, want README.md=2/src.app.ts=5; result = %s", counts, mustMarshal(result))
	}
	filenames := codexReaderStringSlice(t, result["filenames"])
	if len(filenames) != 2 || filenames[0] != "README.md" || filenames[1] != "src/app.ts" {
		t.Fatalf("filenames = %#v, want README.md/src/app.ts; result = %s", filenames, mustMarshal(result))
	}
}

func TestReadCodexFileNormalizesExecCommandGrepNoMatchResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-grep",
				"name":      "exec_command",
				"arguments": `{"cmd":"rg \"missing\" README.md"}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-grep",
				"output":  `{"stdout":"","stderr":"","exitCode":1}`,
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	result := codexReaderToolResultContent(t, sess, "call-grep")
	block := codexReaderToolResultBlock(t, sess, "call-grep")
	if block.IsError {
		t.Fatalf("IsError = true, want false for grep no-match; result = %s", mustMarshal(result))
	}
	if got := jsonStringValue(result["mode"]); got != "files_with_matches" {
		t.Fatalf("mode = %q, want files_with_matches; result = %s", got, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["num_files"]); !ok || got != 0 {
		t.Fatalf("num_files = %d/%v, want 0/true; result = %s", got, ok, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["num_results"]); !ok || got != 0 {
		t.Fatalf("num_results = %d/%v, want 0/true; result = %s", got, ok, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["exit_code"]); !ok || got != 1 {
		t.Fatalf("exit_code = %d/%v, want 1/true for audit; result = %s", got, ok, mustMarshal(result))
	}
	if _, ok := result["exitCode"]; ok {
		t.Fatalf("result leaked native exitCode key: %s", mustMarshal(result))
	}
}

func TestReadCodexFileMarksExecCommandJSONFailureResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-command",
				"name":      "exec_command",
				"arguments": `{"cmd":"go test ./..."}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-command",
				"output":  `{"stdout":"","stderr":"boom\n","exitCode":2}`,
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	block := codexReaderToolResultBlock(t, sess, "call-command")
	if !block.IsError {
		t.Fatalf("IsError = false, want true for nonzero command exit; content = %s", block.Content)
	}
	result := codexReaderToolResultContent(t, sess, "call-command")
	if got := jsonStringValue(result["stderr"]); got != "boom\n" {
		t.Fatalf("stderr = %q, want boom; result = %s", got, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["exit_code"]); !ok || got != 2 {
		t.Fatalf("exit_code = %d/%v, want 2/true; result = %s", got, ok, mustMarshal(result))
	}
}

func TestReadCodexFileMarksExecCommandTextFailureResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-command",
				"name":      "exec_command",
				"arguments": `{"cmd":"go test ./..."}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-command",
				"output":  "Command: go test ./...\nOutput:\nProcess exited with code 2\n",
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	block := codexReaderToolResultBlock(t, sess, "call-command")
	if !block.IsError {
		t.Fatalf("IsError = false, want true for textual nonzero command exit; content = %s", block.Content)
	}
}

func TestReadCodexFileNormalizesJSONStringCommandResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-codex.jsonl")
	writeCodexReaderFixture(t, path,
		map[string]any{
			"timestamp": "2026-06-01T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"call_id":   "call-command",
				"name":      "exec_command",
				"arguments": `{"cmd":"go test ./..."}`,
			},
		},
		map[string]any{
			"timestamp": "2026-06-01T00:00:01Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type":    "function_call_output",
				"call_id": "call-command",
				"output":  `{"stdout":"ok ./...\n","stderr":"","exitCode":0}`,
			},
		},
	)

	sess, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("ReadCodexFile: %v", err)
	}
	result := codexReaderToolResultContent(t, sess, "call-command")
	if got := jsonStringValue(result["stdout"]); got != "ok ./...\n" {
		t.Fatalf("stdout = %q, want parsed stdout; result = %s", got, mustMarshal(result))
	}
	if got, ok := jsonIntValue(result["exit_code"]); !ok || got != 0 {
		t.Fatalf("exit_code = %d/%v, want 0/true; result = %s", got, ok, mustMarshal(result))
	}
	if _, ok := result["exitCode"]; ok {
		t.Fatalf("result leaked native exitCode key: %s", mustMarshal(result))
	}
}

func writeCodexReaderFixture(t *testing.T, path string, entries ...map[string]any) {
	t.Helper()
	var body []byte
	for _, entry := range entries {
		row, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal codex row: %v", err)
		}
		body = append(body, row...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write codex fixture: %v", err)
	}
}

func codexReaderToolInput(t *testing.T, sess *Session, callID string) map[string]json.RawMessage {
	t.Helper()
	for _, entry := range sess.Messages {
		for _, block := range entry.ContentBlocks() {
			if block.Type != "tool_use" || block.ID != callID {
				continue
			}
			var input map[string]json.RawMessage
			if err := json.Unmarshal(block.Input, &input); err != nil {
				t.Fatalf("unmarshal input %s: %v", block.Input, err)
			}
			return input
		}
	}
	t.Fatalf("missing tool_use %q in session: %+v", callID, sess.Messages)
	return nil
}

func codexReaderToolResultContent(t *testing.T, sess *Session, callID string) map[string]json.RawMessage {
	t.Helper()
	block := codexReaderToolResultBlock(t, sess, callID)
	var result map[string]json.RawMessage
	if err := json.Unmarshal(block.Content, &result); err != nil {
		t.Fatalf("unmarshal result %s: %v", block.Content, err)
	}
	return result
}

func codexReaderToolResultBlock(t *testing.T, sess *Session, callID string) ContentBlock {
	t.Helper()
	for _, entry := range sess.Messages {
		for _, block := range entry.ContentBlocks() {
			if block.Type != "tool_result" || block.ToolUseID != callID {
				continue
			}
			return block
		}
	}
	t.Fatalf("missing tool_result %q in session: %+v", callID, sess.Messages)
	return ContentBlock{}
}

func codexReaderStringSlice(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatalf("unmarshal string slice %s: %v", raw, err)
	}
	return values
}
