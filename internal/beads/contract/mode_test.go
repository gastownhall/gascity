package contract

import "testing"

func TestIsDoltBackendAcceptsBDAlias(t *testing.T) {
	if !IsDoltBackend("bd") {
		t.Fatal("IsDoltBackend(\"bd\") = false, want true")
	}
}
