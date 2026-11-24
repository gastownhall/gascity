//go:build linux

package session

import "testing"

func TestNativeLinuxPressureEvidenceRejectsMalformedAndDuplicateFrames(t *testing.T) {
	for _, data := range []string{"", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", "full avg10=NaN\n", "full avg10=Inf\n", "full avg10=-1\n", "full avg10=101\n", "full avg10=1 avg10=2\n", "full total=0\nfull avg10=0.1\n", "full avg10=0\nfull avg10=0\n"} {
		if _, err := nativeFullPSI(data); err == nil {
			t.Fatalf("malformed pressure accepted: %q", data)
		}
	}
	if value, err := nativeFullPSI("some avg10=10.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.25 avg60=0.00 avg300=0.00 total=0\n"); err != nil || value != 0.25 {
		t.Fatalf("valid kernel PSI refused: %v %v", value, err)
	}
}
