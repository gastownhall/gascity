package orders

import (
	"strings"
	"testing"
	"time"
)

// (e) A trigger="webhook" order without [order.params] fails validation.
func TestValidate_WebhookTriggerRequiresParams(t *testing.T) {
	noParams := Order{Name: "pr-review", Formula: "pr-review", Trigger: "webhook"}
	err := Validate(noParams)
	if err == nil {
		t.Fatal("webhook order with no [order.params] must fail validation")
	}
	if !strings.Contains(err.Error(), "params") {
		t.Errorf("error = %q, want it to mention params", err)
	}

	withParams := Order{
		Name:    "pr-review",
		Formula: "pr-review",
		Trigger: "webhook",
		Params:  map[string]OrderParam{"repo": {Required: true}},
	}
	if err := Validate(withParams); err != nil {
		t.Fatalf("webhook order with [order.params] should validate: %v", err)
	}
}

// A webhook-triggered order is never tick-fired (behaves like manual).
func TestCheckTrigger_WebhookNeverDue(t *testing.T) {
	a := Order{
		Name:    "pr-review",
		Formula: "pr-review",
		Trigger: "webhook",
		Params:  map[string]OrderParam{"repo": {Required: true}},
	}
	res := CheckTrigger(a, time.Now(), nil, nil, nil)
	if res.Due {
		t.Fatalf("webhook-triggered order must never be tick-due; got %+v", res)
	}
}
