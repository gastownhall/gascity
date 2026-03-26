package account

import "testing"

func TestResolve(t *testing.T) {
	reg := Registry{
		Accounts: []Account{
			{Handle: "work", ConfigDir: "/home/user/.claude-work"},
			{Handle: "personal", ConfigDir: "/home/user/.claude-personal"},
		},
		Default: "work",
	}

	got, err := Resolve(reg, "work")
	if err != nil {
		t.Fatalf("Resolve(work): %v", err)
	}
	if got.ConfigDir != "/home/user/.claude-work" {
		t.Errorf("ConfigDir = %q, want /home/user/.claude-work", got.ConfigDir)
	}

	_, err = Resolve(reg, "nonexistent")
	if err == nil {
		t.Fatal("Resolve(nonexistent): expected error")
	}
}

func TestDefaultAccount(t *testing.T) {
	reg := Registry{
		Accounts: []Account{
			{Handle: "work", ConfigDir: "/work"},
		},
		Default: "work",
	}

	got, err := DefaultAccount(reg)
	if err != nil {
		t.Fatalf("DefaultAccount: %v", err)
	}
	if got.Handle != "work" {
		t.Errorf("Handle = %q, want work", got.Handle)
	}

	reg.Default = ""
	_, err = DefaultAccount(reg)
	if err == nil {
		t.Fatal("DefaultAccount with no default: expected error")
	}

	reg.Default = "missing"
	_, err = DefaultAccount(reg)
	if err == nil {
		t.Fatal("DefaultAccount with missing handle: expected error")
	}
}

func TestAdd(t *testing.T) {
	var reg Registry
	if err := Add(&reg, Account{Handle: "a", ConfigDir: "/a"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}
	if len(reg.Accounts) != 1 {
		t.Fatalf("len = %d, want 1", len(reg.Accounts))
	}

	// Duplicate.
	if err := Add(&reg, Account{Handle: "a", ConfigDir: "/a2"}); err == nil {
		t.Fatal("Add duplicate: expected error")
	}
}

func TestRemove(t *testing.T) {
	reg := Registry{
		Accounts: []Account{
			{Handle: "a", ConfigDir: "/a"},
			{Handle: "b", ConfigDir: "/b"},
		},
		Default: "a",
	}

	if err := Remove(&reg, "a"); err != nil {
		t.Fatalf("Remove(a): %v", err)
	}
	if len(reg.Accounts) != 1 {
		t.Fatalf("len = %d, want 1", len(reg.Accounts))
	}
	if reg.Default != "" {
		t.Errorf("Default = %q, want empty (cleared after removing default)", reg.Default)
	}

	if err := Remove(&reg, "nonexistent"); err == nil {
		t.Fatal("Remove(nonexistent): expected error")
	}
}

func TestSetDefault(t *testing.T) {
	reg := Registry{
		Accounts: []Account{{Handle: "a"}, {Handle: "b"}},
	}
	if err := SetDefault(&reg, "b"); err != nil {
		t.Fatalf("SetDefault(b): %v", err)
	}
	if reg.Default != "b" {
		t.Errorf("Default = %q, want b", reg.Default)
	}

	if err := SetDefault(&reg, "missing"); err == nil {
		t.Fatal("SetDefault(missing): expected error")
	}
}
