package session

// The package's tests predate the runtime lease and run Managers without a
// city path; TestLeaselessManagerFailsAtUse turns the refusal back on.
func init() { AllowManagersWithoutCityForTest() }
