//go:build windows

package runtime

func defaultSkipTrackedOverlays() bool {
	return true
}
