package doctor

// ErrorCheck returns a check that always reports StatusError with the
// supplied name and message.
func ErrorCheck(name, message string) Check {
	return &errorCheck{name: name, message: message}
}

type errorCheck struct {
	name    string
	message string
}

func (c *errorCheck) Name() string { return c.name }

func (c *errorCheck) Run(_ *CheckContext) *CheckResult {
	return &CheckResult{
		Name:    c.name,
		Status:  StatusError,
		Message: c.message,
	}
}

func (c *errorCheck) CanFix() bool { return false }

func (c *errorCheck) Fix(_ *CheckContext) error { return nil }

func (c *errorCheck) WarmupEligible() bool { return false }
