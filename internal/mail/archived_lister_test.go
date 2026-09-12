package mail

import "testing"

// TestFakeAllIncludingArchivedListsArchivedMail pins the fake's
// [ArchivedLister] contract: an archived message leaves All and stays in
// AllIncludingArchived.
func TestFakeAllIncludingArchivedListsArchivedMail(t *testing.T) {
	f := NewFake()
	sent, err := f.Send("controller", "mayor", "PARKED gp-1 [park abc]", "body")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Archive(sent.ID); err != nil {
		t.Fatal(err)
	}
	open, err := f.All("mayor")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("All after Archive = %d, want 0", len(open))
	}
	all, err := f.AllIncludingArchived("mayor")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != sent.ID {
		t.Fatalf("AllIncludingArchived = %+v, want %s", all, sent.ID)
	}
}
