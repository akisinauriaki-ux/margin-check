package act

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"margincheck/internal/offer"
	"margincheck/internal/paypal"
	"margincheck/internal/profit"
)

var invoiceDay = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func sampleOptions(t *testing.T) (offer.Option, offer.Option) {
	t.Helper()
	seller := profit.Seller{UnitCostCents: 2300, ShippingCostCentsPerOrder: 600, ShippingCostCentsPerBox: 150,
		PayPalFeePercentBP: 349, PayPalFixedFeeCents: 49, YearlyCostOfMoneyBP: 1000, UnderlineMinGiveawayBP: 200, Currency: "USD"}
	s, err := offer.Build(offer.Request{Currency: "USD", Quantity: 20, UnitPriceCents: 4000, RequestedPaymentDays: 60}, seller, offer.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var q, e offer.Option
	for _, o := range s.Options {
		switch o.Kind {
		case offer.Quantity:
			q = o
		case offer.EarlyPayment:
			e = o
		}
	}
	return q, e
}

var parties = Parties{SellerEmail: "seller@example.com", BuyerEmail: "buyer@example.com", ItemName: "Stoneware mugs (box)"}

// fakeTools is a pretend PayPal: it remembers invoices by number, so a repeat finds the same draft.
type fakeTools struct {
	byNumber   map[string]*paypal.Invoice
	creates    int
	dueDates   int
	failDue    bool
	tamperQty  bool
	callsOrder []string
}

func newFake() *fakeTools { return &fakeTools{byNumber: map[string]*paypal.Invoice{}} }

func (f *fakeTools) CreateInvoice(_ context.Context, a CreateInvoiceArgs) (string, bool, error) {
	f.callsOrder = append(f.callsOrder, ToolCreateInvoice)
	if inv, ok := f.byNumber[a.InvoiceNumber]; ok {
		return inv.ID, true, nil
	}
	f.creates++
	qty := a.Quantity
	if f.tamperQty {
		qty++
	}
	inv := &paypal.Invoice{ID: "INV2-FAKE-" + a.InvoiceNumber, Status: "DRAFT",
		Detail: paypal.InvoiceDetail{CurrencyCode: a.Currency, InvoiceNumber: a.InvoiceNumber, InvoiceDate: a.InvoiceDate},
		Items: []paypal.InvoiceLineItem{{Name: a.ItemName, Quantity: itoa(qty),
			UnitAmount: paypal.Money{CurrencyCode: a.Currency, Value: centsString(a.UnitPriceCents)}}}}
	if a.DiscountBP > 0 {
		inv.Items[0].Discount = &paypal.ItemDiscount{Percent: bpString(a.DiscountBP)}
	}
	if a.ShippingCents > 0 {
		inv.Amount = &paypal.InvoiceAmount{Breakdown: &paypal.AmountBreakdown{Shipping: &paypal.ShippingCost{
			Amount: &paypal.Money{CurrencyCode: a.Currency, Value: centsString(a.ShippingCents)}}}}
	}
	f.byNumber[a.InvoiceNumber] = inv
	return inv.ID, false, nil
}

func (f *fakeTools) SetDueDate(_ context.Context, invoiceID string, a SetDueDateArgs) error {
	f.callsOrder = append(f.callsOrder, ToolSetDueDate)
	if f.failDue {
		return errors.New("PayPal 503")
	}
	f.dueDates++
	for _, inv := range f.byNumber {
		if inv.ID == invoiceID {
			inv.Detail.PaymentTerm = &paypal.InvoicePaymentTerm{TermType: "DUE_ON_DATE_SPECIFIED", DueDate: a.DueDate}
		}
	}
	return nil
}

func (f *fakeTools) ReadBack(_ context.Context, invoiceID string) (*paypal.Invoice, error) {
	for _, inv := range f.byNumber {
		if inv.ID == invoiceID {
			c := *inv
			return &c, nil
		}
	}
	return nil, errors.New("not found")
}

func TestThePlanForEachChoice(t *testing.T) {
	q, e := sampleOptions(t)

	pq := ExpectedPlan("deal-1", q, parties, invoiceDay)
	if len(pq.Calls) != 1 || pq.Calls[0].Tool != ToolCreateInvoice {
		t.Fatalf("quantity plan %+v, want one create_invoice", pq.Calls)
	}
	c := pq.Calls[0].Create
	if c.Quantity != 30 || c.UnitPriceCents != 4000 || c.ShippingCents != 0 || c.DiscountBP != 0 || c.Currency != "USD" {
		t.Errorf("quantity create args %+v", c)
	}

	pe := ExpectedPlan("deal-1", e, parties, invoiceDay)
	if len(pe.Calls) != 2 || pe.Calls[0].Tool != ToolCreateInvoice || pe.Calls[1].Tool != ToolSetDueDate {
		t.Fatalf("early plan %+v, want create_invoice then set_due_date", pe.Calls)
	}
	if ce := pe.Calls[0].Create; ce.Quantity != 20 || ce.DiscountBP != 150 || ce.ShippingCents != 3600 {
		t.Errorf("early create args %+v", ce)
	}
	if d := pe.Calls[1].DueDate; d.DueDate != "2026-11-02" || d.InvoiceNumber != pe.Calls[0].Create.InvoiceNumber {
		t.Errorf("due date args %+v, want 2026-11-02 on the same invoice number", d)
	}
	if pq.Calls[0].Create.InvoiceNumber == pe.Calls[0].Create.InvoiceNumber {
		t.Error("two different choices must never share an invoice number")
	}
}

func TestTheFingerprintCoversEveryArgumentAndTheOrder(t *testing.T) {
	_, e := sampleOptions(t)
	base := ExpectedPlan("deal-1", e, parties, invoiceDay)
	fp := Fingerprint(base)
	if fp != Fingerprint(ExpectedPlan("deal-1", e, parties, invoiceDay)) {
		t.Fatal("the same plan must give the same fingerprint")
	}
	changes := map[string]func(p *Plan){
		"unit price": func(p *Plan) { p.Calls[0].Create.UnitPriceCents++ },
		"quantity":   func(p *Plan) { p.Calls[0].Create.Quantity++ },
		"discount":   func(p *Plan) { p.Calls[0].Create.DiscountBP += 10 },
		"buyer":      func(p *Plan) { p.Calls[0].Create.BuyerEmail = "other@example.com" },
		"due date":   func(p *Plan) { p.Calls[1].DueDate.DueDate = "2026-12-31" },
		"order":      func(p *Plan) { p.Calls[0], p.Calls[1] = p.Calls[1], p.Calls[0] },
		"extra call": func(p *Plan) { p.Calls = append(p.Calls, p.Calls[1]) },
	}
	for name, change := range changes {
		p := clonePlan(base)
		change(&p)
		if Fingerprint(p) == fp {
			t.Errorf("%s: the fingerprint did not change", name)
		}
	}
	p := clonePlan(base)
	p.Explanation = "different words"
	if Fingerprint(p) != fp {
		t.Error("the explanation is not executed, so it must not change the fingerprint")
	}
}

func TestTheInterceptorBlocksAnyDifferenceFromTheOffer(t *testing.T) {
	_, e := sampleOptions(t)
	want := ExpectedPlan("deal-1", e, parties, invoiceDay)
	if diffs := Intercept(clonePlan(want), want); len(diffs) != 0 {
		t.Fatalf("an identical plan was blocked: %v", diffs)
	}
	cases := map[string]func(p *Plan){
		"unit_price": func(p *Plan) { p.Calls[0].Create.UnitPriceCents = 3500 },
		"discount":   func(p *Plan) { p.Calls[0].Create.DiscountBP = 5000 },
		"shipping":   func(p *Plan) { p.Calls[0].Create.ShippingCents = 0 },
		"extra":      func(p *Plan) { p.Calls = append(p.Calls, p.Calls[0]) },
		"missing":    func(p *Plan) { p.Calls = p.Calls[:1] },
		"order":      func(p *Plan) { p.Calls[0], p.Calls[1] = p.Calls[1], p.Calls[0] },
		"unknown":    func(p *Plan) { p.Calls[1].Tool = "paypal.send_invoice" },
		"due_date":   func(p *Plan) { p.Calls[1].DueDate.DueDate = "2027-01-01" },
		"recipient":  func(p *Plan) { p.Calls[0].Create.BuyerEmail = "attacker@example.com" },
	}
	for name, change := range cases {
		p := clonePlan(want)
		change(&p)
		diffs := Intercept(p, want)
		if len(diffs) == 0 {
			t.Errorf("%s: the interceptor let a changed plan through", name)
		}
	}
}

func TestApprovedPlanRunsAndIsReadBack(t *testing.T) {
	_, e := sampleOptions(t)
	plan := ExpectedPlan("deal-1", e, parties, invoiceDay)
	appr := Approve(plan)
	f := newFake()
	res, err := Execute(context.Background(), f, appr, plan, e, parties, invoiceDay)
	if err != nil {
		t.Fatalf("execute: %v (%+v)", err, res)
	}
	if !res.ReadBackOK || res.Status != "DRAFT" || !res.DueDateSet {
		t.Errorf("result %+v", res)
	}
	if strings.Join(f.callsOrder, ",") != ToolCreateInvoice+","+ToolSetDueDate {
		t.Errorf("calls %v", f.callsOrder)
	}
}

func TestAPlanChangedAfterApprovalNeverRuns(t *testing.T) {
	_, e := sampleOptions(t)
	plan := ExpectedPlan("deal-1", e, parties, invoiceDay)
	appr := Approve(plan)
	changed := clonePlan(plan)
	changed.Calls[0].Create.Quantity = 200
	f := newFake()
	if _, err := Execute(context.Background(), f, appr, changed, e, parties, invoiceDay); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("err %v, want ErrFingerprintMismatch", err)
	}
	if len(f.callsOrder) != 0 {
		t.Errorf("PayPal was called %v after a mismatch", f.callsOrder)
	}
}

func TestAnApprovedButWrongPlanIsStillBlockedAtExecution(t *testing.T) {
	// Defence in depth: even if a tampered plan was approved, execution rebuilds the plan from the offer.
	_, e := sampleOptions(t)
	tampered := ExpectedPlan("deal-1", e, parties, invoiceDay)
	tampered.Calls[0].Create.DiscountBP = 4000
	appr := Approve(tampered)
	f := newFake()
	if _, err := Execute(context.Background(), f, appr, tampered, e, parties, invoiceDay); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err %v, want ErrBlocked", err)
	}
	if len(f.callsOrder) != 0 {
		t.Errorf("PayPal was called %v for a blocked plan", f.callsOrder)
	}
}

func TestARepeatCreatesNoSecondInvoice(t *testing.T) {
	_, e := sampleOptions(t)
	plan := ExpectedPlan("deal-1", e, parties, invoiceDay)
	appr := Approve(plan)
	f := newFake()
	first, err := Execute(context.Background(), f, appr, plan, e, parties, invoiceDay)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Execute(context.Background(), f, appr, plan, e, parties, invoiceDay)
	if err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || second.InvoiceID != first.InvoiceID || !second.AlreadyExisted {
		t.Errorf("creates %d, ids %s / %s, already existed %v", f.creates, first.InvoiceID, second.InvoiceID, second.AlreadyExisted)
	}
}

func TestAHalfDoneRunIsVisibleAndResumes(t *testing.T) {
	_, e := sampleOptions(t)
	plan := ExpectedPlan("deal-1", e, parties, invoiceDay)
	appr := Approve(plan)
	f := newFake()
	f.failDue = true
	res, err := Execute(context.Background(), f, appr, plan, e, parties, invoiceDay)
	if err == nil || res.InvoiceID == "" || res.DueDateSet {
		t.Fatalf("err %v, result %+v: want the draft created, the due date not set, and an error", err, res)
	}
	f.failDue = false
	res2, err := Execute(context.Background(), f, appr, plan, e, parties, invoiceDay)
	if err != nil || !res2.DueDateSet || f.creates != 1 {
		t.Fatalf("resume: err %v, result %+v, creates %d", err, res2, f.creates)
	}
}

func TestAReadBackMismatchIsReported(t *testing.T) {
	q, _ := sampleOptions(t)
	plan := ExpectedPlan("deal-1", q, parties, invoiceDay)
	f := newFake()
	f.tamperQty = true
	res, err := Execute(context.Background(), f, Approve(plan), plan, q, parties, invoiceDay)
	if err == nil || res.ReadBackOK {
		t.Fatalf("err %v, read-back ok %v: a draft that differs from the plan must be reported", err, res.ReadBackOK)
	}
	found := false
	for _, c := range res.Checks {
		if c.Field == "quantity" && !c.OK {
			found = true
		}
	}
	if !found {
		t.Errorf("checks %+v, want a failed quantity check", res.Checks)
	}
}

// The read-back is the last check that PayPal holds what the seller approved, so it must read PayPal's amounts
// exactly: a sign, extra non-zero decimals or anything unreadable is a mismatch, never a near value.
func TestTheReadBackReadsAmountsExactly(t *testing.T) {
	want := &CreateInvoiceArgs{Currency: "USD", InvoiceNumber: "MC-1", Quantity: 1, UnitPriceCents: 50, DiscountBP: 150}
	held := func(unit, pct string) *paypal.Invoice {
		return &paypal.Invoice{Status: "DRAFT", Detail: paypal.InvoiceDetail{CurrencyCode: "USD", InvoiceNumber: "MC-1"},
			Items: []paypal.InvoiceLineItem{{Quantity: "1", UnitAmount: paypal.Money{CurrencyCode: "USD", Value: unit},
				Discount: &paypal.ItemDiscount{Percent: pct}}}}
	}
	ok := func(checks []ReadCheck, field string) bool {
		for _, c := range checks {
			if c.Field == field {
				return c.OK
			}
		}
		t.Fatalf("no %s check", field)
		return false
	}
	for _, tc := range []struct {
		unit, pct     string
		unitOK, pctOK bool
	}{
		{"0.50", "1.5", true, true},
		{"0.5", "1.50", true, true},
		{"0.50", "1.500", true, true},
		{"-0.50", "1.5", false, true},
		{"+0.50", "1.5", false, true},
		{"0.509", "1.5", false, true},
		{"0.50", "1.505", true, false},
		{"abc", "1.5", false, true},
		{"0.5x", "1.5", false, true},
		{"0.-5", "1.5", false, true},
	} {
		checks := compare(held(tc.unit, tc.pct), want, nil)
		if got := ok(checks, "unit_price_cents"); got != tc.unitOK {
			t.Errorf("unit amount %q read back OK=%v, want %v", tc.unit, got, tc.unitOK)
		}
		if got := ok(checks, "discount_bp"); got != tc.pctOK {
			t.Errorf("discount %q read back OK=%v, want %v", tc.pct, got, tc.pctOK)
		}
	}
}
