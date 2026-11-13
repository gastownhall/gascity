package workrecord

import "io"

// FormatTable writes a human-readable coverage summary table to w.
func FormatTable(_ io.Writer, _ CoverageReport) error {
	return nil
}
