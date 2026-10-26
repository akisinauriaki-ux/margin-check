package act

import (
	"context"
	"errors"
	"strings"
	"testing"

	"margincheck/internal/gemini"
)

type fakePlanner struct {
	reply string
	err   error
	seen  []gemini.Request
}

func (f *fakePlanner) Generate(_ context.Context, req gemini.Request) (gemini.Response, error) {
	f.seen = append(f.seen, req)
	if f.err != nil {
		return gemini.Response{}, f.err
	}
	return gemini.Response{Text: f.reply, TotalTokens: 50, Source: "live"}, nil
}

const goodEarly = `{"calls":[
 {"tool":"paypal.create_invoice","quantity":20,"unit_price_cents":4000,"discount_bp":150,"shipping_cents":3600,"due_in_days":0},
 {"tool":"paypal.set_due_date","quantity":0,"unit_price_cents":0,"discount_bp":0,"shipping_cents":0,"due_in_days":7}],
 "explanation":"Create the draft with the early-payment discount and shipping at cost, then set the due date."}`

func TestTheAIPlanIsUsedWhenItMatchesTheOffer(t *testing.T) {
	_, e := sampleOptions(t)
	g := &fakePlanner{reply: goodEarly}
	pr := Propose(context.Background(), g, "m", "deal-1", e, parties, invoiceDay)
	if pr.Plan.By != "ai" || len(pr.Blocked) != 0 {
		t.Fatalf("by %q, blocked %v", pr.Plan.By, pr.Blocked)
	}
	if Fingerprint(pr.Plan) != Fingerprint(ExpectedPlan("deal-1", e, parties, invoiceDay)) {
		t.Error("an accepted AI plan must run exactly the calls the offer requires")
	}
	if !strings.HasPrefix(pr.Plan.Explanation, "Create the draft") {
		t.Errorf("explanation %q", pr.Plan.Explanation)
	}
}

func TestTheAIPlannerSeesNoEmailAndNoAddresses(t *testing.T) {
	_, e := sampleOptions(t)
	g := &fakePlanner{reply: goodEarly}
	Propose(context.Background(), g, "m", "deal-1", e, parties, invoiceDay)
	sent := g.seen[0].SystemInstruction + g.seen[0].UserText
	for _, s := range []string{parties.SellerEmail, parties.BuyerEmail, "Buyer:", "Seller:"} {
		if strings.Contains(sent, s) {
			t.Errorf("the planner was shown %q", s)
		}
	}
}

func TestAWrongAIPlanIsBlockedAndTheCodePlanOffered(t *testing.T) {
	_, e := sampleOptions(t)
	for name, reply := range map[string]string{
		"bigger discount": strings.Replace(goodEarly, `"discount_bp":150`, `"discount_bp":1000`, 1),
		"no shipping":     strings.Replace(goodEarly, `"shipping_cents":3600`, `"shipping_cents":0`, 1),
		"later due date":  strings.Replace(goodEarly, `"due_in_days":7`, `"due_in_days":60`, 1),
		"third tool":      strings.Replace(goodEarly, `],`, `,{"tool":"paypal.send_invoice","quantity":0,"unit_price_cents":0,"discount_bp":0,"shipping_cents":0,"due_in_days":0}],`, 1),
		"missing call":    `{"calls":[{"tool":"paypal.create_invoice","quantity":20,"unit_price_cents":4000,"discount_bp":150,"shipping_cents":3600,"due_in_days":0}],"explanation":"Create it."}`,
	} {
		pr := Propose(context.Background(), &fakePlanner{reply: reply}, "m", "deal-1", e, parties, invoiceDay)
		if len(pr.Blocked) == 0 {
			t.Errorf("%s: the AI plan was not blocked", name)
		}
		if pr.Plan.By != "code" || Fingerprint(pr.Plan) != Fingerprint(ExpectedPlan("deal-1", e, parties, invoiceDay)) {
			t.Errorf("%s: want the code plan offered instead, got by %q", name, pr.Plan.By)
		}
	}
}

func TestAnExplanationWithNumbersIsReplaced(t *testing.T) {
	_, e := sampleOptions(t)
	reply := strings.Replace(goodEarly, "Create the draft with", "Create a draft for 200 boxes with", 1)
	pr := Propose(context.Background(), &fakePlanner{reply: reply}, "m", "deal-1", e, parties, invoiceDay)
	if strings.ContainsAny(pr.Plan.Explanation, "0123456789") {
		t.Errorf("explanation %q: the AI's words must never state numbers the code didn't check", pr.Plan.Explanation)
	}
	if pr.Plan.By != "ai" {
		t.Errorf("by %q: the calls still matched, so the AI plan stands with the code's explanation", pr.Plan.By)
	}
}

func TestAnUnavailablePlannerFallsBackToTheCodePlan(t *testing.T) {
	_, e := sampleOptions(t)
	pr := Propose(context.Background(), &fakePlanner{err: errors.New("503")}, "m", "deal-1", e, parties, invoiceDay)
	if pr.Plan.By != "code" || pr.Note == "" {
		t.Errorf("by %q, note %q", pr.Plan.By, pr.Note)
	}
}

func TestAnExplanationWithNumbersInAnyScriptIsReplaced(t *testing.T) {
	_, e := sampleOptions(t)
	for _, words := range []string{"Create a draft for ２０ boxes with", "Create a draft at ¥ with", "Create a draft with ½ off and"} {
		reply := strings.Replace(goodEarly, "Create the draft with", words, 1)
		pr := Propose(context.Background(), &fakePlanner{reply: reply}, "m", "deal-1", e, parties, invoiceDay)
		if pr.Plan.Explanation != codeExplanation(e) {
			t.Errorf("explanation %q kept; an explanation stating a number or a currency must be replaced", pr.Plan.Explanation)
		}
	}
}
