package poolplan

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestCreateBudgetClaimsOnlyAssignedShares(t *testing.T) {
	budget := NewCreateBudget(2)
	budget.ConfigureFairShare([]Demand{
		{Template: "alpha", FreshCreates: 3},
		{Template: "zulu", FreshCreates: 3},
	}, 0)

	if !budget.TryClaim("alpha") {
		t.Fatal("alpha first claim = false, want true")
	}
	if budget.TryClaim("alpha") {
		t.Fatal("alpha second claim = true, want false after assigned share is consumed")
	}
	if !budget.TryClaim("zulu") {
		t.Fatal("zulu first claim = false, want true")
	}
	if budget.TryClaim("zulu") {
		t.Fatal("zulu second claim = true, want false after budget is exhausted")
	}
}

func TestCreateBudgetUsesUnassignedSpareTokens(t *testing.T) {
	budget := NewCreateBudget(3)
	budget.ConfigureFairShare([]Demand{
		{Template: "alpha", FreshCreates: 1},
		{Template: "zulu", FreshCreates: 1},
	}, 0)

	for claim := 1; claim <= 2; claim++ {
		if !budget.TryClaim("alpha") {
			t.Fatalf("alpha claim %d = false, want true while an assigned or spare token remains", claim)
		}
	}
	if budget.TryClaim("alpha") {
		t.Fatal("alpha third claim = true, want false after its share and the spare token are consumed")
	}
	if !budget.TryClaim("zulu") {
		t.Fatal("zulu assigned claim = false, want true")
	}
}

func TestCreateBudgetReleaseMakesTokenReusableAcrossTemplates(t *testing.T) {
	budget := NewCreateBudget(1)
	budget.ConfigureFairShare([]Demand{
		{Template: "alpha", FreshCreates: 1},
		{Template: "zulu", FreshCreates: 1},
	}, 0)

	if !budget.TryClaim("alpha") {
		t.Fatal("alpha claim = false, want true")
	}
	budget.Release()
	if !budget.TryClaim("zulu") {
		t.Fatal("zulu claim after alpha release = false, want released token to be globally reusable")
	}
	if budget.TryClaim("alpha") {
		t.Fatal("claim after reused token = true, want false")
	}
}

func TestCreateBudgetSingleDemandUsesGlobalLimit(t *testing.T) {
	budget := NewCreateBudget(2)
	budget.ConfigureFairShare([]Demand{{Template: "alpha", FreshCreates: 2}}, 0)
	for claim := 1; claim <= 2; claim++ {
		if !budget.TryClaim("unexpected") {
			t.Fatalf("unexpected template claim %d = false, want global limit to permit it", claim)
		}
	}
	if budget.TryClaim("unexpected") {
		t.Fatal("third claim = true, want false after global limit is exhausted")
	}
}

func TestDisabledCreateBudgetIsUnlimited(t *testing.T) {
	budget := NewCreateBudget(0)
	budget.ConfigureFairShare([]Demand{{Template: "alpha", FreshCreates: 1}}, 0)
	for claim := 1; claim <= 2; claim++ {
		if !budget.TryClaim("alpha") {
			t.Fatalf("disabled budget claim %d = false, want unlimited claims", claim)
		}
	}
	budget.Release()
}

func TestCreateBudgetReservesFloorBeforeElasticDemand(t *testing.T) {
	for seed := uint64(0); seed < 5; seed++ {
		budget := NewCreateBudget(1)
		budget.ConfigureFairShare([]Demand{
			{Template: "alpha", FreshCreates: 3},
			{Template: "zulu", FreshCreates: 1, HasFloor: true},
		}, seed)

		if !budget.TryClaim("zulu") {
			t.Errorf("seed=%d: floor claim = false, want true", seed)
		}
		if budget.TryClaim("alpha") {
			t.Errorf("seed=%d: elastic claim = true, want floor to consume the only token", seed)
		}
	}

	budget := NewCreateBudget(3)
	budget.ConfigureFairShare([]Demand{
		{Template: "alpha", FreshCreates: 3},
		{Template: "zulu", FreshCreates: 1, HasFloor: true},
	}, 0)
	if !budget.TryClaim("zulu") {
		t.Fatal("floor claim = false, want true")
	}
	if budget.TryClaim("zulu") {
		t.Fatal("second floor claim = true, want floor-only demand capped at one")
	}
	for claim := 1; claim <= 2; claim++ {
		if !budget.TryClaim("alpha") {
			t.Fatalf("elastic surplus claim %d = false, want both remaining tokens", claim)
		}
	}
}

func TestCreateBudgetReservesElasticSliceWhenFloorsSaturateLimit(t *testing.T) {
	demands := make([]Demand, 0, 9)
	for i := 0; i < 8; i++ {
		demands = append(demands, Demand{
			Template:     string(rune('a' + i)),
			FreshCreates: 1,
			HasFloor:     true,
		})
	}
	demands = append(demands, Demand{Template: "elastic", FreshCreates: 6})

	for seed := uint64(0); seed < uint64(len(demands)); seed++ {
		budget := NewCreateBudget(8)
		budget.ConfigureFairShare(demands, seed)
		for claim := 1; claim <= 2; claim++ {
			if !budget.TryClaim("elastic") {
				t.Fatalf("seed=%d: elastic claim %d = false, want reserved quarter of budget", seed, claim)
			}
		}
		if budget.TryClaim("elastic") {
			t.Fatalf("seed=%d: elastic third claim = true, want exactly one quarter of budget", seed)
		}
		floorClaims := 0
		for _, demand := range demands[:8] {
			if budget.TryClaim(demand.Template) {
				floorClaims++
			}
		}
		if floorClaims != 6 {
			t.Fatalf("seed=%d: floor claims = %d, want remaining three quarters of budget", seed, floorClaims)
		}
	}
}

func TestCreateBudgetRotatesFloorReservation(t *testing.T) {
	templates := []string{"alpha", "mike", "zulu"}
	demands := []Demand{
		{Template: templates[0], FreshCreates: 1, HasFloor: true},
		{Template: templates[1], FreshCreates: 1, HasFloor: true},
		{Template: templates[2], FreshCreates: 1, HasFloor: true},
	}
	reserved := make(map[string]bool, len(templates))
	for seed := uint64(0); seed < 6; seed++ {
		budget := NewCreateBudget(1)
		budget.ConfigureFairShare(demands, seed)
		claims := 0
		for _, template := range templates {
			if budget.TryClaim(template) {
				reserved[template] = true
				claims++
			}
		}
		if claims != 1 {
			t.Errorf("seed=%d: successful floor claims = %d, want 1", seed, claims)
		}
	}
	for _, template := range templates {
		if !reserved[template] {
			t.Errorf("floor template %q was never reserved across rotating seeds", template)
		}
	}
}

func TestCreateBudgetConcurrentClaimsDoNotExceedLimit(t *testing.T) {
	const (
		limit      = 16
		contenders = 128
	)
	budget := NewCreateBudget(limit)
	budget.ConfigureFairShare([]Demand{
		{Template: "alpha", FreshCreates: limit},
		{Template: "zulu", FreshCreates: limit},
	}, 0)
	var start sync.WaitGroup
	start.Add(1)
	var finished sync.WaitGroup
	finished.Add(contenders * 2)
	var alphaClaims atomic.Int64
	var zuluClaims atomic.Int64
	for _, contender := range []struct {
		template string
		claims   *atomic.Int64
	}{
		{template: "alpha", claims: &alphaClaims},
		{template: "zulu", claims: &zuluClaims},
	} {
		for i := 0; i < contenders; i++ {
			go func(template string, claims *atomic.Int64) {
				defer finished.Done()
				start.Wait()
				if budget.TryClaim(template) {
					claims.Add(1)
				}
			}(contender.template, contender.claims)
		}
	}
	start.Done()
	finished.Wait()

	if got := alphaClaims.Load(); got != limit/2 {
		t.Fatalf("alpha successful claims = %d, want %d", got, limit/2)
	}
	if got := zuluClaims.Load(); got != limit/2 {
		t.Fatalf("zulu successful claims = %d, want %d", got, limit/2)
	}
	if got := alphaClaims.Load() + zuluClaims.Load(); got != limit {
		t.Fatalf("total successful claims = %d, want %d", got, limit)
	}
}

func TestCreateBudgetConcurrentRefundsRemainClaimable(t *testing.T) {
	const limit = 16
	budget := NewCreateBudget(limit)
	budget.ConfigureFairShare([]Demand{
		{Template: "alpha", FreshCreates: limit / 2},
		{Template: "zulu", FreshCreates: limit / 2},
	}, 0)
	for _, template := range []string{"alpha", "zulu"} {
		for claim := 0; claim < limit/2; claim++ {
			if !budget.TryClaim(template) {
				t.Fatalf("initial %s claim %d = false, want true", template, claim+1)
			}
		}
	}

	var start sync.WaitGroup
	start.Add(1)
	results := make(chan bool, limit)
	var finished sync.WaitGroup
	finished.Add(limit)
	for i := 0; i < limit; i++ {
		go func() {
			defer finished.Done()
			start.Wait()
			budget.Release()
			results <- budget.TryClaim("replacement")
		}()
	}
	start.Done()
	finished.Wait()
	close(results)

	claimed := 0
	for ok := range results {
		if ok {
			claimed++
		}
	}
	if claimed != limit {
		t.Fatalf("reclaimed concurrent refunds = %d, want %d", claimed, limit)
	}
	if budget.TryClaim("replacement") {
		t.Fatal("claim after all refunds were reused = true, want false")
	}
}
