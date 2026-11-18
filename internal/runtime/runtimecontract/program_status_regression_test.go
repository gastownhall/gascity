package runtimecontract

import (
	"slices"
	"testing"
)

func TestPStatusGroupRedAtBase(t *testing.T) {
	codes := []Code{"RPP-PSTATUS-001", "RPP-PSTATUS-002", "RPP-PSTATUS-003"}
	report, err := run(t.Context(), writeGolden(t), Options{}, func(c Code) bool { return slices.Contains(codes, c) })
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range codes {
		found := false
		for _, r := range report.Results {
			if r.Code == code {
				found = true
				if r.Status != StatusPass {
					t.Errorf("%s: %s %s", code, r.Status, r.Detail)
				}
			}
		}
		if !found {
			t.Errorf("missing conformance result %s", code)
		}
	}
}
