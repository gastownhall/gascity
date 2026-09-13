package testutil

import "time"

// GoroutineRaceTimeout is the minimum safe deadline for a test timer that races
// a goroutine under CI CPU saturation. A sub-second value for such a timer is a
// CI reliability defect. See TESTING.md "Test deadline rule."
const GoroutineRaceTimeout = 10 * time.Second

// ExecRaceTimeout is the minimum safe deadline for a test timer that races a
// subprocess start (exec, ps, dolt, bd) under CI CPU saturation.
const ExecRaceTimeout = 10 * time.Second
