package doctor

// OrderFinding is one non-OK per-order result of an order check.
type OrderFinding struct {
	Order    string `json:"order"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
}

// OrderFindingsPayload is the typed Payload of the order checks.
type OrderFindingsPayload struct {
	Findings []OrderFinding `json:"findings"`
}

// OrderFindings returns the findings carried by an order check result.
// RED stub: the real accessor lands with the implementation.
func OrderFindings(_ *CheckResult) ([]OrderFinding, bool) { return nil, false }
