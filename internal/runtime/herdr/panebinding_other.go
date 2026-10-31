//go:build !windows

package herdr

// shellProbe folds process-info into the probe verdict. herdr's foreground
// list shows what runs in the pane, so it is the whole answer here.
func (p *Provider) shellProbe(shellPID int, fg []proc) (paneProbe, error) {
	return paneProbeFrom(shellPID, fg), nil
}
