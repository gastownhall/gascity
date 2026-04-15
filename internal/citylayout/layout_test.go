package citylayout

import "testing"

func TestAccountsFilePath(t *testing.T) {
	got := AccountsFilePath("myCity")
	want := "myCity/.gc/accounts.json"
	if got != want {
		t.Fatalf("AccountsFilePath(%q) = %q, want %q", "myCity", got, want)
	}
}

func TestQuotaFilePath(t *testing.T) {
	got := QuotaFilePath("myCity")
	want := "myCity/.gc/quota.json"
	if got != want {
		t.Fatalf("QuotaFilePath(%q) = %q, want %q", "myCity", got, want)
	}
}
