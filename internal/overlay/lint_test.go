package overlay

import "testing"

func TestFindBareHookEntries_FlagsBareEntry(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"ok"}]},
		{"type":"command","command":"bare"}
	]}}`)
	bare, err := FindBareHookEntries(data)
	if err != nil {
		t.Fatalf("FindBareHookEntries: %v", err)
	}
	if len(bare) != 1 {
		t.Fatalf("bare entries = %d, want 1", len(bare))
	}
	if bare[0].Category != "PreToolUse" || bare[0].Index != 1 {
		t.Errorf("got %+v, want {PreToolUse 1}", bare[0])
	}
}

func TestFindBareHookEntries_AllWrappedIsClean(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"ok"}]}]}}`)
	bare, err := FindBareHookEntries(data)
	if err != nil {
		t.Fatalf("FindBareHookEntries: %v", err)
	}
	if len(bare) != 0 {
		t.Errorf("bare entries = %d, want 0", len(bare))
	}
}

func TestFindBareHookEntries_NoHooksObject(t *testing.T) {
	bare, err := FindBareHookEntries([]byte(`{"editorMode":"vim"}`))
	if err != nil {
		t.Fatalf("FindBareHookEntries: %v", err)
	}
	if len(bare) != 0 {
		t.Errorf("bare entries = %d, want 0", len(bare))
	}
}

func TestFindBareHookEntries_MultipleCategoriesSorted(t *testing.T) {
	data := []byte(`{"hooks":{
		"PreToolUse":[{"type":"command","command":"a"}],
		"Stop":[{"type":"command","command":"b"}]
	}}`)
	bare, err := FindBareHookEntries(data)
	if err != nil {
		t.Fatalf("FindBareHookEntries: %v", err)
	}
	if len(bare) != 2 {
		t.Fatalf("bare entries = %d, want 2", len(bare))
	}
	// Deterministic, sorted by category: PreToolUse before Stop.
	if bare[0].Category != "PreToolUse" || bare[1].Category != "Stop" {
		t.Errorf("categories not sorted: %+v", bare)
	}
}

func TestFindBareHookEntries_InvalidJSON(t *testing.T) {
	if _, err := FindBareHookEntries([]byte(`not json`)); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestFindInvalidHookMatchers_FlagsPermissionSyntax(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[
		{"matcher":"Bash(*bd mol pour*patrol*)","hooks":[{"type":"command","command":"ok"}]}
	]}}`)
	invalid, err := FindInvalidHookMatchers(data)
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(invalid) != 1 {
		t.Fatalf("invalid matchers = %d, want 1", len(invalid))
	}
	if invalid[0].Category != "PreToolUse" || invalid[0].Index != 0 {
		t.Errorf("got %+v, want {PreToolUse 0 ...}", invalid[0])
	}
}

func TestFindInvalidHookMatchers_FlagsOtherInvalidRegex(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[{"matcher":"[unterminated","hooks":[{"type":"command","command":"ok"}]}]}}`)
	invalid, err := FindInvalidHookMatchers(data)
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(invalid) != 1 {
		t.Fatalf("invalid matchers = %d, want 1", len(invalid))
	}
}

func TestFindInvalidHookMatchers_AcceptsMatchAllForms(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[
		{"matcher":"","hooks":[{"type":"command","command":"ok"}]},
		{"matcher":"*","hooks":[{"type":"command","command":"ok"}]}
	]}}`)
	invalid, err := FindInvalidHookMatchers(data)
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(invalid) != 0 {
		t.Errorf("invalid matchers = %d, want 0: %+v", len(invalid), invalid)
	}
}

func TestFindInvalidHookMatchers_AcceptsValidToolNameMatcher(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"ok"}]}]}}`)
	invalid, err := FindInvalidHookMatchers(data)
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(invalid) != 0 {
		t.Errorf("invalid matchers = %d, want 0: %+v", len(invalid), invalid)
	}
}

func TestFindInvalidHookMatchers_SkipsBareEntries(t *testing.T) {
	data := []byte(`{"hooks":{"PreToolUse":[{"type":"command","command":"bare"}]}}`)
	invalid, err := FindInvalidHookMatchers(data)
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(invalid) != 0 {
		t.Errorf("invalid matchers = %d, want 0: %+v", len(invalid), invalid)
	}
}

func TestFindInvalidHookMatchers_NoHooksObject(t *testing.T) {
	invalid, err := FindInvalidHookMatchers([]byte(`{"editorMode":"vim"}`))
	if err != nil {
		t.Fatalf("FindInvalidHookMatchers: %v", err)
	}
	if len(invalid) != 0 {
		t.Errorf("invalid matchers = %d, want 0", len(invalid))
	}
}

func TestFindInvalidHookMatchers_InvalidJSON(t *testing.T) {
	if _, err := FindInvalidHookMatchers([]byte(`not json`)); err == nil {
		t.Error("expected error for invalid JSON")
	}
}
