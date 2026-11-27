package doctor

import "github.com/gastownhall/gascity/internal/orders"

// OrderFinding is one non-OK per-order result of an order check. Status and
// Severity use the spelling of gc doctor --json ("warning"/"error",
// "blocking"/"advisory"); Detail is byte-identical to the check's Details line
// for the order.
type OrderFinding struct {
	Order    string `json:"order"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
}

// OrderFindingsPayload is the typed Payload of order-firing-current and
// order-outcome-healthy. Findings holds only the non-OK orders, and is a
// non-nil empty slice for an evaluated all-OK result so --json prints [].
type OrderFindingsPayload struct {
	Findings []OrderFinding `json:"findings"`
}

// OrderFindings returns the findings an order check result carries. ok is
// false when the result has no order-findings payload (an early-return
// failure, a timeout, or a result from another check).
func OrderFindings(r *CheckResult) ([]OrderFinding, bool) {
	if r == nil {
		return nil, false
	}
	payload, ok := r.Payload.(*OrderFindingsPayload)
	if !ok || payload == nil {
		return nil, false
	}
	return payload.Findings, true
}

// orderFindingSet collects one order check's per-order results. record keeps
// the Details line and the finding in one statement so Detail can never drift
// from the line it mirrors.
type orderFindingSet struct {
	findings []OrderFinding
}

func newOrderFindingSet() *orderFindingSet {
	return &orderFindingSet{findings: []OrderFinding{}}
}

// record appends detail to result.Details and, when status is not OK, a
// finding carrying that same detail.
func (s *orderFindingSet) record(result *CheckResult, order orders.Order, status CheckStatus, severity CheckSeverity, detail string) {
	result.Details = append(result.Details, detail)
	if status == StatusOK {
		return
	}
	s.findings = append(s.findings, OrderFinding{
		Order:    orderDisplayName(order),
		Status:   orderFindingStatus(status),
		Severity: orderFindingSeverity(severity),
		Detail:   detail,
	})
}

// payload returns the set as a check payload.
func (s *orderFindingSet) payload() *OrderFindingsPayload {
	return &OrderFindingsPayload{Findings: s.findings}
}

func orderFindingStatus(s CheckStatus) string {
	if s == StatusError {
		return "error"
	}
	return "warning"
}

func orderFindingSeverity(s CheckSeverity) string {
	if s == SeverityAdvisory {
		return "advisory"
	}
	return "blocking"
}
