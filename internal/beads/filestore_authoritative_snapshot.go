package beads

var _ AuthoritativeSnapshotter = (*FileStore)(nil)

// AuthoritativeSnapshot refreshes FileStore's cross-process view once, then
// delegates to the embedded MemStore's single-lock snapshot. The explicit
// override prevents method promotion from bypassing on-disk freshness.
func (fs *FileStore) AuthoritativeSnapshot() ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.AuthoritativeSnapshot()
}
