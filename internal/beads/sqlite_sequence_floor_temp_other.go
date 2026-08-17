//go:build !linux

package beads

func sqliteSequenceFloorTempPattern(_ string, base string) (string, error) {
	return base + "*", nil
}
