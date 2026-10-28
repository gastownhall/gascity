package opjournal

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// opGen builds random valid ops for codec property tests.
type opGen struct{ r *rand.Rand }

func newOpGen(seed uint64) *opGen {
	return &opGen{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

var genRunes = []rune("abcxyz019-_:/ éü日本語🙂")

func (g *opGen) text(n int) string {
	var b strings.Builder
	for range 1 + g.r.IntN(n) {
		b.WriteRune(genRunes[g.r.IntN(len(genRunes))])
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		return "x"
	}
	return s
}

func (g *opGen) payload() string {
	parts := []string{"hello", "\n", "\t", "日本語", "🙂", " ", "{\"json\": [1, 2]}", "ü"}
	var b strings.Builder
	for range 1 + g.r.IntN(12) {
		b.WriteString(parts[g.r.IntN(len(parts))])
	}
	return b.String()
}

func (g *opGen) token() string {
	return pick(g.r, []string{"a", "x_y", "z.9", "q:r", "custom_reason"}) + fmt.Sprint(g.r.IntN(100))
}

func (g *opGen) time() time.Time {
	return time.Unix(0, 1_700_000_000_000_000_000+g.r.Int64N(1_000_000_000_000_000)).UTC()
}

func (g *opGen) opID() string {
	id, err := DeriveID(pick(g.r, []string{"gc", "gcs", "my-city"}), "gc-"+fmt.Sprint(g.r.IntN(1000)), "p", g.text(8))
	if err != nil {
		panic(err)
	}
	return id
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

func (g *opGen) op() Op {
	r := g.r
	kind := pick(r, Kinds())
	spec, _ := Lookup(kind)
	op := Op{
		Conversation: pick(r, []string{"gc-1", "gc-a3f8e", "my-city-x9", "conv_" + fmt.Sprint(r.IntN(1e6))}),
		Kind:         kind,
		Executor:     pick(r, executors),
		SchemaRev:    SchemaRev,
		ClientKey:    g.text(30),
		Producer: Producer{
			Principal: g.text(20),
			Actor:     pick(r, []string{ActorOperator, ActorAgent, ActorBackground, ActorController, ActorSweep, "t3/bridge"}),
		},
		CreatedAt: g.time(),
	}
	if r.IntN(2) == 0 {
		op.Producer.OwnerEpoch = r.Int64N(1 << 40)
	}
	if spec.PayloadAllowed {
		op.Payload = g.payload()
	}
	op.Policy = applyPolicyDefaults(Policy{}, spec)
	switch kind {
	case KindMessage:
		op.Policy.Message = MessagePolicy{
			When:          pick(r, spec.When),
			Wake:          pick(r, []Wake{WakeIfNeeded, WakeLiveOnly}),
			Role:          pick(r, []Role{RolePrompt, RoleNotice}),
			OnUnsupported: pick(r, []Fallback{FallbackBoundary, FallbackFail}),
		}
		if op.Policy.Message.When == WhenNow && r.IntN(2) == 0 {
			op.Policy.Message.NowGeneration = 1 + r.Int64N(100)
			if r.IntN(2) == 0 {
				op.Policy.Message.NowTurn = g.text(10)
			}
		}
	case KindInterrupt:
		op.Policy.Interrupt.OnFailure = pick(r, []OnFailure{OnFailureFail, OnFailureRestart})
	case KindRestart:
		op.Policy.Restart.Mode = pick(r, []RestartMode{RestartContinue, RestartFresh})
	case KindKill:
		op.Policy.Kill.Grace = time.Duration(r.Int64N(int64(time.Hour)))
	}
	if spec.Lane == LaneConversation {
		op.Policy.Guarantee = pick(r, []Guarantee{GuaranteeAtLeastOnce, GuaranteeAtMostOnce})
	}
	op.Scope = Scope{Kind: pick(r, spec.Scopes)}
	switch op.Scope.Kind {
	case ScopeContext:
		op.Scope.Epoch = 1 + r.Int64N(100)
	case ScopeSession:
		op.Scope.Generation = 1 + r.Int64N(100)
		if spec.TurnScoped && r.IntN(2) == 0 {
			op.Scope.Turn = g.text(10)
		}
		if spec.InteractionScoped {
			op.Scope.Interaction = g.text(10)
		}
	}
	if kind == KindRespond {
		// A respond carries a RespondPayload and, on the lane, answers an
		// occurrence at most once (validateRespondShape).
		p := RespondPayload{Action: pick(r, []string{"approve", "deny", "approve_always"})}
		if r.IntN(2) == 0 {
			p.Text = g.text(12)
		}
		if r.IntN(2) == 0 {
			p.Metadata = map[string]string{"source": g.text(8)}
		}
		op.Payload, _ = EncodeRespondPayload(p)
		if op.Executor == ExecutorLane {
			op.Scope.Interaction = fmt.Sprintf("req-%d#%d", r.IntN(1000), 1+r.IntN(5))
			op.Policy.Guarantee = GuaranteeAtMostOnce
		}
	}
	if spec.AllowsAfter && r.IntN(2) == 0 {
		op.After = g.opID()
	}
	if r.IntN(2) == 0 {
		op.ClientSeq = r.Uint64()
	}
	start := op.CreatedAt
	if r.IntN(2) == 0 {
		op.NotBefore = op.CreatedAt.Add(time.Duration(r.Int64N(int64(time.Hour))))
		start = op.NotBefore
	}
	op.StartBy = start.Add(time.Duration(r.Int64N(int64(time.Hour))))
	op.FinishBy = op.StartBy.Add(time.Duration(r.Int64N(int64(time.Hour))))
	if r.IntN(2) == 0 {
		op.Causation = g.text(12)
		op.Correlation = g.text(12)
	}
	statuses := []Status{StatusSubmitted, StatusAccepted, StatusSettling, StatusClosed}
	if spec.Lane == LaneConversation {
		statuses = append(statuses, StatusExecuting)
	}
	op.Status = pick(r, statuses)
	if op.Executor.Legacy() && op.Status != StatusClosed {
		op.Status = StatusSubmitted // a legacy op stays submitted until it closes
	}
	op.LaneSeq = r.Uint64N(1 << 50)
	op.Attempt = r.IntN(6)
	if spec.Lane == LaneConversation && op.Executor == ExecutorLane && op.Status != StatusClosed && r.IntN(2) == 0 {
		op.Lease = Lease{
			Lane: string(spec.Lane), Conversation: op.Conversation,
			Generation: 1 + r.Int64N(50), Nonce: "n" + fmt.Sprint(r.IntN(1e9)), Deadline: g.time(),
		}
	}
	if r.IntN(3) == 0 {
		op.Cancel = Cancel{By: g.text(10), At: g.time()}
		if r.IntN(2) == 0 {
			op.Cancel.Reason = g.text(20)
		}
	}
	if op.Status == StatusClosed {
		op.Outcome = g.outcome(&op)
	}
	for range 1 + r.IntN(5) {
		op.Transitions = append(op.Transitions, Transition{Status: pick(r, statuses), At: g.time(), By: g.text(10)})
	}
	op.ID, _ = DeriveID(pick(r, []string{"gc", "gcs", "my-city"}), op.Conversation, op.Producer.Principal, op.ClientKey)
	g.laneFields(&op, spec)
	op.PayloadDigest = RequestDigest(op)
	return op
}

// laneFields gives a conversation-lane lane op random lane bookkeeping and, on
// a succeeded outcome, a random delivery record and turn link.
func (g *opGen) laneFields(op *Op, spec Spec) {
	r := g.r
	if op.Executor != ExecutorLane || spec.Lane != LaneConversation {
		return
	}
	if r.IntN(2) == 0 {
		op.RetryAt = g.time()
	}
	op.Failures = r.IntN(6)
	if op.Attempt > 0 {
		op.MaybeDelivered = r.IntN(op.Attempt + 1)
	}
	if r.IntN(2) == 0 {
		op.DeliveryKey = pick(r, []string{op.ID, g.opID()}) + ":" + fmt.Sprint(1+r.IntN(1000))
	}
	if r.IntN(2) == 0 {
		op.LastError = pick(r, []string{"lease_expired", "transient: pane busy " + g.text(20), "unknown: 日本語 🙂"})
	}
	if op.Outcome.Arm != ArmSucceeded || r.IntN(4) == 0 {
		return
	}
	res := &op.Outcome.Result
	res.Delivery = RecordedDelivery{
		Seq: 1 + r.Uint64N(1<<40), At: g.time(),
		Ack: pick(r, []AckKind{AckDelivered, AckDeliveredUnobserved, AckInBand}),
	}
	if r.IntN(2) == 0 {
		res.Delivery.MessageID = g.text(20)
		res.Delivery.MessageAt = g.time()
	}
	res.TurnLink = pick(r, []LinkConfidence{"", LinkExact, LinkCorrelated, LinkAmbiguous, LinkNone})
	if res.TurnLink == "" {
		return
	}
	res.TurnBasis = pick(r, []LinkBasis{BasisRuntimeTurn, BasisHistoryDiff, BasisNoSource, "a_newer_basis"})
	if res.TurnLink == LinkNone {
		res.Turn = ""
	} else if res.Turn == "" {
		res.Turn = g.text(10)
	}
}

func (g *opGen) outcome(op *Op) Outcome {
	r := g.r
	o := Outcome{Arm: pick(r, arms)}
	switch o.Arm {
	case ArmSucceeded:
		o.Result = Result{Generation: r.Int64N(100), ContextEpoch: r.Int64N(10)}
		if r.IntN(2) == 0 {
			o.Result.Turn = g.text(10)
			o.Result.Via = g.text(10)
		}
		if r.IntN(3) == 0 {
			o.Degradation = "restarted_instead_of_interrupted"
		}
	case ArmFailed:
		o.Reason = pick(r, append(append([]string{}, knownFailedReasons...), "refused:policy", g.token()))
	case ArmSkipped:
		o.Reason = pick(r, append(append([]string{}, knownSkippedReasons...), g.token()))
		if o.Reason == ReasonCoalescedInto {
			o.Result.CoalescedInto = g.opID()
		}
	case ArmCanceled:
		if !op.Cancel.Recorded() {
			op.Cancel = Cancel{By: g.text(10), At: g.time()}
		}
	}
	return o
}
