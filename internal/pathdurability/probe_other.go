//go:build !linux

package pathdurability

import "errors"

// errUnsupported reports that this platform has no durability probe. Classify
// turns it into Unknown, so non-Linux builds never refuse a path.
var errUnsupported = errors.New("pathdurability: unsupported platform")

func deviceID(_ string) (uint64, error) { return 0, errUnsupported }

func filesystemType(_ string) (uint32, error) { return 0, errUnsupported }
