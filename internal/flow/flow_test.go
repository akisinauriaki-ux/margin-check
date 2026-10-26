package flow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"margincheck/internal/act"
	"margincheck/internal/gemini"
	"margincheck/internal/offer"
	"margincheck/internal/paypal"
	"margincheck/internal/profit"
	"margincheck/internal/reader"
)

// --- stand-ins -------------------------------------------------------------------------------------------

type fakeReader struct {
	byEmail map[string]reader.CheckResult
	seen    *[]string // every text the reader was given, in order
}

// Check finds the reading by the exact text, or by the text the app's added context ends with (the buyer's
// pasted answer comes last).
func (f fakeReader) Check(_ context.Context, id, email string) (reader.CheckResult, error) {
	if f.seen != nil {
		*f.seen = append(*f.seen, email)
	}
	r, ok := f.byEmail[email]
	if !ok {
		for k, v := range f.byEmail {
			if strings.HasSuffix(email, k) {
				r, ok = v, true
				break
			}
		}
	}
	if !ok {
		return reader.CheckResult{}, errors.New("unexpected email")
	}
	r.EmailID = id
	return r, nil
}

type noModel struct{}

func (noModel) Generate(context.Context, gemini.Request) (gemini.Response, error) {
	return gemini.Response{}, errors.New("no model in tests")
}

type fakeTools struct {
	created int
	calls   int
	inv     *paypal.Invoice
}

func (f *fakeTools) CreateInvoice(_ context.Context, a act.CreateInvoiceArgs) (string, bool, error) {
	f.calls++
	if f.inv != nil {
		return f.inv.ID, true, nil
	}
	f.created++
	f.inv = &paypal.Invoice{ID: "INV2-TEST-0001", Status: "DRAFT",
		Detail: paypal.InvoiceDetail{CurrencyCode: a.Currency, InvoiceNumber: a.InvoiceNumber},
		Items: []paypal.InvoiceLineItem{{Quantity: itoa(a.Quantity),
			UnitAmount: paypal.Money{CurrencyCode: a.Currency, Value: cents(a.UnitPriceCents)}}}}
	return f.inv.ID, false, nil
}
func (f *fakeTools) SetDueDate(context.Context, string, act.SetDueDateArgs) error {
	f.calls++
	return nil
}
func (f *fakeTools) ReadBack(context.Context, string) (*paypal.Invoice, error) {
	c := *f.inv
	return &c, nil
}

func sure(v int64, q string) reader.IntFieldResult {
	return reader.IntFieldResult{Value: v, Sure: true, Quote: q}
}
func sureB(v bool, q string) reader.BoolFieldResult {
	return reader.BoolFieldResult{Value: v, Sure: true, Quote: q}
}

const (
	askEmail    = "Buyer: We want 20 boxes at $40 per box, but we need free shipping and two months to pay."
	replyYes    = "Buyer: OK, 30 boxes with free shipping works for us."
	replyMore   = "Buyer: We accept the bulk deal but can we do 35 boxes instead?"
	replyMaybe  = "Buyer: Sounds interesting, let's go ahead with that proposal."
	replyWrongQ = "Buyer: Option 2 please, 35 boxes."
)

func readings() map[string]reader.CheckResult {
	first := reader.CheckResult{
		Quantity:                sure(20, "We want 20 boxes at $40 per box, but we need free shipping and two months to pay."),
		UnitPriceCents:          sure(4000, "We want 20 boxes at $40 per box, but we need free shipping and two months to pay."),
		DiscountPercent:         sure(0, ""),
		FreeShipping:            sureB(false, ""),
		PaymentDays:             sure(0, ""),
		RebatePercent:           sure(0, ""),
		PriceIncreaseDiscounted: sureB(false, ""),
		Terms: []reader.TermResult{
			{Name: "free_shipping", Value: true, By: "buyer", Status: "requested", SourceSentence: "We want 20 boxes at $40 per box, but we need free shipping and two months to pay."},
			{Name: "payment_days", Value: float64(60), By: "buyer", Status: "requested", SourceSentence: "We want 20 boxes at $40 per box, but we need free shipping and two months to pay."},
		},
	}
	yes := first
	yes.Quantity = sure(30, "OK, 30 boxes with free shipping works for us.")
	yes.FreeShipping = sureB(true, "OK, 30 boxes with free shipping works for us.")
	yes.AcceptedOption = reader.StringFieldResult{Value: "quantity", Sure: true, Quote: "OK, 30 boxes with free shipping works for us."}
	more := first
	more.AcceptedOption = reader.StringFieldResult{Value: "changed", Sure: true, Quote: "We accept the bulk deal but can we do 35 boxes instead?"}
	maybe := first
	maybe.AcceptedOption = reader.StringFieldResult{Value: "UNCLEAR", Unknown: true, Quote: "Sounds interesting, let's go ahead with that proposal."}
	maybe.MustAsk = []string{"accepted_option"}
	wrongQ := first
	wrongQ.Quantity = sure(35, "Option 2 please, 35 boxes.")
	wrongQ.AcceptedOption = reader.StringFieldResult{Value: "quantity", Sure: true, Quote: "Option 2 please, 35 boxes."}
	return map[string]reader.CheckResult{askEmail: first, replyYes: yes, replyMore: more, replyMaybe: maybe, replyWrongQ: wrongQ}
}

func deps(t *testing.T, tools act.Tools) Deps {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Reader: fakeReader{byEmail: readings()}, Writer: noModel{}, Planner: noModel{}, Tools: tools,
		Seller: profit.Seller{UnitCostCents: 2300, ShippingCostCentsPerOrder: 600, ShippingCostCentsPerBox: 150,
			PayPalFeePercentBP: 349, PayPalFixedFeeCents: 49, YearlyCostOfMoneyBP: 1000, UnderlineMinGiveawayBP: 200, Currency: "USD"},
		Parties: act.Parties{SellerEmail: "s@example.com", BuyerEmail: "b@example.com", ItemName: "Mugs"},
		Store:   store,
		Now:     func() time.Time { return time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC) },
	}
}

// --- the flow ----------------------------------------------------------------------------------------------

func TestStartShowsWhatTheDealKeepsAndThreeChoices(t *testing.T) {
	d := deps(t, &fakeTools{})
	deal, err := d.Start(context.Background(), askEmail)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Check.Outcome != "evaluated" || deal.Check.Profit == nil {
		t.Fatalf("check %+v", deal.Check)
	}
	if deal.Offers == nil || len(deal.Offers.Options) != 3 {
		t.Fatalf("offers %+v", deal.Offers)
	}
	if deal.Request.RequestedPaymentDays != 60 {
		t.Errorf("asked payment days %d, want 60 from the buyer's request", deal.Request.RequestedPaymentDays)
	}
	if deal.Reply == nil || !deal.Reply.ByCode || !strings.Contains(deal.Reply.Text, "Option 2 (Quantity)") {
		t.Errorf("reply %+v", deal.Reply)
	}
	loaded, err := d.Store.Load(deal.ID)
	if err != nil || loaded.ID != deal.ID {
		t.Fatalf("store round trip: %v", err)
	}
}

func TestAnUnsureDealShowsNoMoneyAndNoChoices(t *testing.T) {
	d := deps(t, &fakeTools{})
	r := readings()[askEmail]
	r.UnitPriceCents = reader.IntFieldResult{Unknown: true, Reason: "readings_disagree"}
	d.Reader = fakeReader{byEmail: map[string]reader.CheckResult{askEmail: r}}
	deal, err := d.Start(context.Background(), askEmail)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Check.Profit != nil || deal.Offers != nil {
		t.Fatalf("a money figure or choices were made from an unsure price: %+v %+v", deal.Check.Profit, deal.Offers)
	}
	if len(deal.Check.Questions) == 0 {
		t.Error("no questions for the seller")
	}
}

func TestTheBuyersYesBecomesAPlanForApproval(t *testing.T) {
	d := deps(t, &fakeTools{})
	deal, _ := d.Start(context.Background(), askEmail)
	deal, err := d.BuyerReplied(context.Background(), deal.ID, replyYes)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Next != NextDraft || deal.Accepted != offer.Quantity || deal.Proposal == nil {
		t.Fatalf("next %q, accepted %q, proposal %v (%s)", deal.Next, deal.Accepted, deal.Proposal != nil, deal.NextReason)
	}
	if c := deal.Proposal.Plan.Calls; len(c) != 1 || c[0].Create.Quantity != 30 {
		t.Errorf("plan %+v", c)
	}
}

func TestUnclearOrChangedRepliesNeverPlanAnything(t *testing.T) {
	for email, want := range map[string]string{replyMaybe: NextAsk, replyMore: NextSellerDecides, replyWrongQ: NextSellerDecides} {
		d := deps(t, &fakeTools{})
		deal, _ := d.Start(context.Background(), askEmail)
		deal, err := d.BuyerReplied(context.Background(), deal.ID, email)
		if err != nil {
			t.Fatal(err)
		}
		if deal.Next != want || deal.Proposal != nil {
			t.Errorf("%q: next %q (want %q), proposal %v", email, deal.Next, want, deal.Proposal != nil)
		}
	}
}

func TestApprovalMustMatchThePlanShown(t *testing.T) {
	tools := &fakeTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)

	if _, err := d.ApproveAndRun(context.Background(), deal.ID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("an approval for a different fingerprint was accepted")
	}
	if tools.calls != 0 {
		t.Fatalf("PayPal was called %d times without a matching approval", tools.calls)
	}

	deal, err := d.ApproveAndRun(context.Background(), deal.ID, act.Fingerprint(deal.Proposal.Plan))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if deal.Result == nil || deal.Result.Status != "DRAFT" || tools.created != 1 {
		t.Fatalf("result %+v, created %d", deal.Result, tools.created)
	}

	// Approving the same plan again must not create a second invoice.
	if _, err := d.ApproveAndRun(context.Background(), deal.ID, act.Fingerprint(deal.Proposal.Plan)); err != nil {
		t.Fatal(err)
	}
	if tools.created != 1 {
		t.Errorf("created %d invoices", tools.created)
	}
}

func TestAPlanEditedOnDiskIsRefused(t *testing.T) {
	tools := &fakeTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)
	shown := act.Fingerprint(deal.Proposal.Plan)

	deal.Proposal.Plan.Calls[0].Create.UnitPriceCents = 100 // someone edits the saved deal
	if err := d.Store.Save(deal); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApproveAndRun(context.Background(), deal.ID, shown); err == nil {
		t.Fatal("a plan changed after it was shown was run")
	}
	if tools.calls != 0 {
		t.Errorf("PayPal was called %d times", tools.calls)
	}
}

func itoa(n int64) string  { return strconv.FormatInt(n, 10) }
func cents(c int64) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

// slowTools is a pretend PayPal whose create takes a moment, like the real toolkit. It records how many runs
// were inside PayPal at once, and what the run's context looked like.
type slowTools struct {
	mu        sync.Mutex
	inside    int
	maxInside int
	created   int
	inv       *paypal.Invoice
	ctxErr    error
	deadline  time.Duration
}

func (f *slowTools) CreateInvoice(ctx context.Context, a act.CreateInvoiceArgs) (string, bool, error) {
	f.mu.Lock()
	f.inside++
	if f.inside > f.maxInside {
		f.maxInside = f.inside
	}
	existing := f.inv
	f.ctxErr = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(dl)
	}
	f.mu.Unlock()

	time.Sleep(50 * time.Millisecond) // both clicks look for the draft before either has made it

	f.mu.Lock()
	defer f.mu.Unlock()
	f.inside--
	if existing != nil {
		return existing.ID, true, nil
	}
	f.created++
	f.inv = &paypal.Invoice{ID: fmt.Sprintf("INV2-TEST-%04d", f.created), Status: "DRAFT",
		Detail: paypal.InvoiceDetail{CurrencyCode: a.Currency, InvoiceNumber: a.InvoiceNumber},
		Items: []paypal.InvoiceLineItem{{Quantity: itoa(a.Quantity),
			UnitAmount: paypal.Money{CurrencyCode: a.Currency, Value: cents(a.UnitPriceCents)}}}}
	return f.inv.ID, false, nil
}
func (f *slowTools) SetDueDate(context.Context, string, act.SetDueDateArgs) error { return nil }
func (f *slowTools) ReadBack(context.Context, string) (*paypal.Invoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *f.inv
	return &c, nil
}

// A double click on Approve sends two requests at once. They must run one after the other, so the second
// finds the draft the first made, and both end with the same good result.
func TestTwoApprovalsAtOnceRunOneAfterTheOther(t *testing.T) {
	tools := &slowTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)
	fp := act.Fingerprint(deal.Proposal.Plan)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = d.ApproveAndRun(context.Background(), deal.ID, fp)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("click %d: %v", i+1, err)
		}
	}
	if tools.maxInside != 1 || tools.created != 1 {
		t.Errorf("%d runs were inside PayPal at once and %d drafts were made; want 1 and 1", tools.maxInside, tools.created)
	}
	saved, err := d.Store.Load(deal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Result == nil || !saved.Result.ReadBackOK || saved.ResultError != "" {
		t.Errorf("saved result %+v, error %q; want the good result", saved.Result, saved.ResultError)
	}
}

// Closing the browser tab cancels the request. A run that has started must not be cut off halfway by that,
// but it must still have its own time limit.
func TestARunIsNotCutOffWhenTheTabCloses(t *testing.T) {
	tools := &slowTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the tab is already closed
	if _, err := d.ApproveAndRun(ctx, deal.ID, act.Fingerprint(deal.Proposal.Plan)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if tools.ctxErr != nil {
		t.Errorf("the run saw the closed tab (%v) and could stop halfway", tools.ctxErr)
	}
	if tools.deadline <= 0 || tools.deadline > 3*time.Minute {
		t.Errorf("the run's time limit is %v; want a limit of at most 3m", tools.deadline)
	}
}

// A damaged invoice day must stop the deal, never become 1 January of year 1 on a plan or a PayPal draft.
func TestADamagedDealDateStopsTheDeal(t *testing.T) {
	tools := &fakeTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)

	deal.InvoiceDay = "not-a-date"
	if err := d.Store.Save(deal); err != nil {
		t.Fatal(err)
	}
	got, err := d.BuyerReplied(context.Background(), deal.ID, replyYes)
	if err == nil || (got != nil && got.Proposal != nil) {
		t.Errorf("err %v: a plan was made from a damaged date", err)
	}

	// Also when the date is damaged after the plan was made.
	deal, _ = d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)
	shown := act.Fingerprint(deal.Proposal.Plan)
	deal.InvoiceDay = "not-a-date"
	if err := d.Store.Save(deal); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApproveAndRun(context.Background(), deal.ID, shown); err == nil || !strings.Contains(err.Error(), "date") {
		t.Errorf("err %v, want a refusal naming the date", err)
	}
	if tools.calls != 0 {
		t.Errorf("PayPal was called %d times with a damaged date", tools.calls)
	}
}

// A buyer's "yes" only becomes a plan when nothing the buyer stated contradicts the choice. A term counts as
// stated when the reading is sure and quotes the sentence; an unstated term is fixed by the choice itself, and
// the seller still sees the plan before approving it.
func TestAStatedChangedTermStopsThePlan(t *testing.T) {
	base := readings()[replyYes] // the buyer accepts the quantity choice: 30 boxes, free shipping, $40, on receipt
	cases := []struct {
		name, reply, want string
		edit              func(*reader.CheckResult)
	}{
		{"unit price", "Buyer: Option 2 works, 30 boxes at $30 each.", "$30.00",
			func(r *reader.CheckResult) { r.UnitPriceCents = sure(3000, "Option 2 works, 30 boxes at $30 each.") }},
		{"payment days", "Buyer: Option 2 works, on net 60.", "60 days",
			func(r *reader.CheckResult) { r.PaymentDays = sure(60, "Option 2 works, on net 60.") }},
		{"free shipping", "Buyer: Option 2 works, we'll pay the shipping.", "shipping",
			func(r *reader.CheckResult) { r.FreeShipping = sureB(false, "Option 2 works, we'll pay the shipping.") }},
		{"discount", "Buyer: Option 2 works with 10% off.", "10%",
			func(r *reader.CheckResult) { r.DiscountPercent = sure(10, "Option 2 works with 10% off.") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base
			r.AcceptedOption = reader.StringFieldResult{Value: "quantity", Sure: true, Quote: strings.TrimPrefix(c.reply, "Buyer: ")}
			c.edit(&r)
			d := deps(t, &fakeTools{})
			d.Reader.(fakeReader).byEmail[c.reply] = r
			deal, err := d.Start(context.Background(), askEmail)
			if err != nil {
				t.Fatal(err)
			}
			deal, err = d.BuyerReplied(context.Background(), deal.ID, c.reply)
			if err != nil {
				t.Fatal(err)
			}
			if deal.Next != NextSellerDecides || deal.Proposal != nil {
				t.Fatalf("next %q, proposal %v: a changed %s must stop the plan", deal.Next, deal.Proposal != nil, c.name)
			}
			if !strings.Contains(deal.NextReason, c.want) {
				t.Errorf("reason %q should name %q", deal.NextReason, c.want)
			}
		})
	}
}

// The demo reply states only that the buyer pays on receipt, which is what the quantity choice says.
func TestTheDemoReplyStillBecomesAPlan(t *testing.T) {
	const reply = "Buyer: We'll take Option 2, payment on receipt."
	r := readings()[askEmail]
	r.Quantity = reader.IntFieldResult{Unknown: true}
	r.PaymentDays = sure(0, "We'll take Option 2, payment on receipt.")
	r.AcceptedOption = reader.StringFieldResult{Value: "quantity", Sure: true, Quote: "We'll take Option 2, payment on receipt."}
	d := deps(t, &fakeTools{})
	d.Reader.(fakeReader).byEmail[reply] = r
	deal, _ := d.Start(context.Background(), askEmail)
	deal, err := d.BuyerReplied(context.Background(), deal.ID, reply)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Next != NextDraft || deal.Proposal == nil {
		t.Fatalf("next %q (%s): the demo reply must still give a plan", deal.Next, deal.NextReason)
	}
}

// When the thread already shows the seller settling payment on receipt, the buyer's earlier ask for two months
// is answered. Offering 1.5% off for paying within 7 days would then keep less than the deal on the table.
func TestNoEarlyPaymentChoiceWhenTheSellerAlreadyHeldFirm(t *testing.T) {
	const thread = "Buyer: We want 20 boxes at $40 per box and two months to pay.\n\nSeller: We can only do payment on receipt."
	r := readings()[askEmail]
	r.PaymentDays = sure(0, "We can only do payment on receipt.")
	d := deps(t, &fakeTools{})
	d.Reader.(fakeReader).byEmail[thread] = r
	deal, err := d.Start(context.Background(), thread)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Offers == nil {
		t.Fatalf("no choices: %s", deal.OffersNote)
	}
	for _, o := range deal.Offers.Options {
		if o.Kind == offer.EarlyPayment {
			t.Fatalf("early-payment choice offered (keeps %d) though the seller already settled payment on receipt", o.Profit.ProfitCents)
		}
	}

	// The unanswered ask (the same request, no seller answer yet) still gets the early-payment choice.
	deal, _ = d.Start(context.Background(), askEmail)
	found := false
	for _, o := range deal.Offers.Options {
		found = found || o.Kind == offer.EarlyPayment
	}
	if !found {
		t.Errorf("an unanswered ask for two months must still get the early-payment choice; omitted %v", deal.Offers.Omitted)
	}
}

// A second approval of a plan that already ran and was read back makes no PayPal call at all.
func TestASecondApprovalOfADoneRunCallsNothing(t *testing.T) {
	tools := &fakeTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)
	fp := act.Fingerprint(deal.Proposal.Plan)
	if _, err := d.ApproveAndRun(context.Background(), deal.ID, fp); err != nil {
		t.Fatal(err)
	}
	before := tools.calls
	again, err := d.ApproveAndRun(context.Background(), deal.ID, fp)
	if err != nil {
		t.Fatal(err)
	}
	if tools.calls != before {
		t.Errorf("the second approval made %d PayPal calls; a done run must not run again", tools.calls-before)
	}
	if again.Result == nil || !again.Result.ReadBackOK || again.Result.AlreadyExisted {
		t.Errorf("result %+v: the first run's result must be kept as it was", again.Result)
	}
}

// Every step of a deal adds one line to its decision log and never rewrites an earlier line. The approval is
// written before PayPal is called, and the log names no email address.
func TestEveryStepIsAppendedToTheDecisionLog(t *testing.T) {
	tools := &fakeTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)
	afterStart, err := d.Store.Log(deal.ID)
	if err != nil {
		t.Fatal(err)
	}
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)
	if _, err := d.ApproveAndRun(context.Background(), deal.ID, act.Fingerprint(deal.Proposal.Plan)); err != nil {
		t.Fatal(err)
	}
	all, err := d.Store.Log(deal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(all, afterStart) {
		t.Fatal("an earlier log line was changed; the log must only grow")
	}
	steps := []string{"read", "choices", "reply", "plan", "approved", "ran"}
	at := 0
	for _, step := range steps {
		i := strings.Index(all[at:], `"step":"`+step+`"`)
		if i < 0 {
			t.Fatalf("step %q missing or out of order in:\n%s", step, all)
		}
		at += i
	}
	if strings.Contains(all, "@example.com") {
		t.Error("the log must not name an email address")
	}
}

// If the decision log can't be written, the approval doesn't run: no PayPal action without its record.
func TestNoRunWithoutItsLogLine(t *testing.T) {
	tools := &fakeTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)
	d.Store.failLog = true
	if _, err := d.ApproveAndRun(context.Background(), deal.ID, act.Fingerprint(deal.Proposal.Plan)); err == nil {
		t.Fatal("the run went ahead without its log line")
	}
	if tools.calls != 0 {
		t.Errorf("PayPal was called %d times without a logged approval", tools.calls)
	}
}

// A short answer such as "Option 2 please" only makes sense next to the choices we sent. The reader is given
// those choices, written by the app, together with the buyer's words.
func TestAShortReplyIsReadWithTheChoicesWeSent(t *testing.T) {
	const reply = "Buyer: Option 2 please."
	r := readings()[askEmail]
	r.Quantity = reader.IntFieldResult{Unknown: true}
	r.AcceptedOption = reader.StringFieldResult{Value: "quantity", Sure: true, Quote: "Option 2 please."}
	var seen []string
	d := deps(t, &fakeTools{})
	fr := d.Reader.(fakeReader)
	fr.byEmail[reply] = r
	fr.seen = &seen
	d.Reader = fr
	deal, _ := d.Start(context.Background(), askEmail)
	deal, err := d.BuyerReplied(context.Background(), deal.ID, reply)
	if err != nil {
		t.Fatal(err)
	}
	last := seen[len(seen)-1]
	if !strings.Contains(last, "Option 2 (Quantity): 30 boxes") || !strings.HasSuffix(last, reply) {
		t.Fatalf("the reader was given %q; it needs the choices we sent, then the buyer's words", last)
	}
	if deal.Next != NextDraft || deal.Proposal == nil {
		t.Fatalf("next %q (%s): a short, clear answer must become a plan", deal.Next, deal.NextReason)
	}
}

// The sentence that proves acceptance must be the buyer's own. The choices the app added can never count as the
// buyer saying yes.
func TestAcceptanceMustBeTheBuyersOwnWords(t *testing.T) {
	const reply = "Buyer: Sounds interesting, let me think."
	r := readings()[askEmail]
	r.AcceptedOption = reader.StringFieldResult{Value: "quantity", Sure: true,
		Quote: "Option 2 (Quantity): 30 boxes at $40.00 each with free shipping, payment on receipt."}
	d := deps(t, &fakeTools{})
	d.Reader.(fakeReader).byEmail[reply] = r
	deal, _ := d.Start(context.Background(), askEmail)
	deal, err := d.BuyerReplied(context.Background(), deal.ID, reply)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Next != NextAsk || deal.Proposal != nil {
		t.Fatalf("next %q: an acceptance quoted from our own choices must not count", deal.Next)
	}
}

// Terms read from the choices the app added are ours, not the buyer's: they must not look like the buyer
// changing the deal.
func TestOurOwnChoicesAreNotTheBuyersStatedTerms(t *testing.T) {
	const reply = "Buyer: Option 2 please."
	r := readings()[askEmail]
	r.Quantity = sure(20, "Option 1 (Standard): 20 boxes at $40.00 each, shipping $36.00 at cost, payment on receipt.")
	r.AcceptedOption = reader.StringFieldResult{Value: "quantity", Sure: true, Quote: "Option 2 please."}
	d := deps(t, &fakeTools{})
	d.Reader.(fakeReader).byEmail[reply] = r
	deal, _ := d.Start(context.Background(), askEmail)
	deal, err := d.BuyerReplied(context.Background(), deal.ID, reply)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Next != NextDraft {
		t.Fatalf("next %q (%s): a quantity quoted from our own Option 1 is not the buyer changing the deal", deal.Next, deal.NextReason)
	}
}

// When the AI can't tell which choice the buyer meant, the seller (who read the email) picks it with one tap,
// and the plan follows exactly as if the AI had read it.
func TestTheSellerPicksWhenTheReplyIsUnclear(t *testing.T) {
	d := deps(t, &fakeTools{})
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyMaybe)
	if deal.Next != NextAsk {
		t.Fatalf("setup: next %q", deal.Next)
	}
	deal, err := d.SellerChose(context.Background(), deal.ID, offer.Quantity)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Next != NextDraft || deal.Proposal == nil || deal.Proposal.Plan.Calls[0].Create.Quantity != 30 {
		t.Fatalf("next %q, proposal %+v: the seller's pick must give the quantity plan", deal.Next, deal.Proposal)
	}
	log, _ := d.Store.Log(deal.ID)
	if !strings.Contains(log, `"step":"seller chose"`) {
		t.Error("the seller's pick must be in the decision log")
	}
}

// The seller can correct the AI's reading with one tap; the plan follows the seller's choice.
func TestTheSellerCanCorrectTheAIsReading(t *testing.T) {
	d := deps(t, &fakeTools{})
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes) // the AI reads the quantity choice
	deal, err := d.SellerChose(context.Background(), deal.ID, offer.Standard)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Accepted != offer.Standard || deal.Proposal == nil || deal.Proposal.Plan.Option != offer.Standard {
		t.Fatalf("accepted %q: the plan must follow the seller's pick", deal.Accepted)
	}
}

// A pick doesn't override a term the buyer stated differently: that still goes to the seller to decide.
func TestASellerPickStillRespectsAChangedTerm(t *testing.T) {
	d := deps(t, &fakeTools{})
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyWrongQ) // "Option 2 please, 35 boxes."
	deal, err := d.SellerChose(context.Background(), deal.ID, offer.Quantity)
	if err != nil {
		t.Fatal(err)
	}
	if deal.Next != NextSellerDecides || deal.Proposal != nil {
		t.Fatalf("next %q: the buyer wrote 35 boxes, so nothing may be planned for 30", deal.Next)
	}
}

// Once a PayPal draft exists, the choice can't be switched.
func TestNoPickAfterTheDraftExists(t *testing.T) {
	tools := &fakeTools{}
	d := deps(t, tools)
	deal, _ := d.Start(context.Background(), askEmail)
	deal, _ = d.BuyerReplied(context.Background(), deal.ID, replyYes)
	if _, err := d.ApproveAndRun(context.Background(), deal.ID, act.Fingerprint(deal.Proposal.Plan)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SellerChose(context.Background(), deal.ID, offer.Standard); err == nil {
		t.Fatal("the choice was switched after the draft was made")
	}
}
