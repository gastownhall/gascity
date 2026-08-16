package builtinpacks

import "testing"

// BenchmarkValidateSyntheticRepo measures the full cache validation directly.
//
// The readiness-pass benchmark in cmd/gc under-resolves changes to this
// function: the validation is a fraction of that pass, so a saving here lands
// inside its noise. A change to the traversal has to be measured against the
// traversal.
func BenchmarkValidateSyntheticRepo(b *testing.B) {
	dst := b.TempDir()
	if err := MaterializeSyntheticRepo(dst, testCommit); err != nil {
		b.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
		b.Fatalf("fixture is invalid: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
			b.Fatalf("ValidateSyntheticRepo: %v", err)
		}
	}
}
