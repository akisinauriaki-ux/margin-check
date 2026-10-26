package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"margincheck/internal/act"
	"margincheck/internal/check"
	"margincheck/internal/flow"
	"margincheck/internal/offer"
	"margincheck/internal/profit"
	"margincheck/internal/reader"
	"margincheck/internal/writer"
)

func testServer(t *testing.T) (*server, *flow.Deal) {
	t.Helper()
	store, err := flow.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seller := profit.Seller{UnitCostCents: 2300, ShippingCostCentsPerOrder: 600, ShippingCostCentsPerBox: 150,
		PayPalFeePercentBP: 349, PayPalFixedFeeCents: 49, YearlyCostOfMoneyBP: 1000, UnderlineMinGiveawayBP: 200, Currency: "USD"}
	set, err := offer.Build(offer.Request{Currency: "USD", Quantity: 20, UnitPriceCents: 4000, RequestedPaymentDays: 60}, seller, offer.Config{})
	if err != nil {
		t.Fatal(err)
	}
	q := "Sure, happy to help! Free shipping and 60 days is fine for 20 boxes."
	rd := reader.CheckResult{
		Quantity:        reader.IntFieldResult{Value: 20, Sure: true, Quote: "Hi, we want 20 boxes of stoneware mugs at $40 per box for our shop."},
		UnitPriceCents:  reader.IntFieldResult{Value: 4000, Sure: true, Quote: "Hi, we want 20 boxes of stoneware mugs at $40 per box for our shop."},
		FreeShipping:    reader.BoolFieldResult{Value: true, Sure: true, Quote: q},
		PaymentDays:     reader.IntFieldResult{Value: 60, Sure: true, Quote: q},
		DiscountPercent: reader.IntFieldResult{Sure: true}, RebatePercent: reader.IntFieldResult{Sure: true},
		PriceIncreaseDiscounted: reader.BoolFieldResult{Sure: true},
		Terms:                   []reader.TermResult{{Name: "unit_price_cents", Value: 4000, Currency: "USD", By: "buyer", Status: "agreed"}},
	}
	chk := check.CheckEmail(rd, seller, sampleThread)
	draft := writer.Write(nil, writer.Config{}, set)
	var qty offer.Option
	for _, o := range set.Options {
		if o.Kind == offer.Quantity {
			qty = o
		}
	}
	parties := act.Parties{SellerEmail: "seller@example.com", BuyerEmail: "buyer@example.com", ItemName: "Mugs"}
	plan := act.ExpectedPlan("d0123456789ab", qty, parties, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	deal := &flow.Deal{ID: "d0123456789ab", InvoiceDay: "2026-10-26", Thread: sampleThread, Reading: rd, Check: chk,
		Offers: &set, Reply: &draft, Next: flow.NextDraft, NextReason: "The buyer accepted the Quantity choice.",
		Accepted: offer.Quantity, AcceptedQuote: "OK, 30 boxes works.",
		Proposal: &act.Proposal{Plan: plan, Blocked: []string{"call 1 paypal.create_invoice: discount_bp is 1000, the offer requires 0"}}}
	if err := store.Save(deal); err != nil {
		t.Fatal(err)
	}
	s := &server{deps: flow.Deps{Store: store, Seller: seller, Parties: parties}, models: "a,b", sellerPath: "testdata/seller.json"}
	if err := s.init(); err != nil {
		t.Fatal(err)
	}
	return s, deal
}

func get(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestTheDealPageShowsEveryStation(t *testing.T) {
	s, deal := testServer(t)
	page := get(t, s.routes(), "/deal/"+deal.ID)
	for _, want := range []string{
		"You keep $",                 // the one big number
		"class=\"l m\"",              // a highlighted giveaway line
		"free shipping (costs you $", // its money effect, from the engine
		"Three fair choices", "Quantity", "Early payment",
		"Option 2 (Quantity)",     // the drafted reply, filled by code
		"The interceptor blocked", // a blocked AI plan is shown
		"Fingerprint of exactly these calls",
		act.Fingerprint(deal.Proposal.Plan),
		"Create the PayPal draft: ",
		"b***@example.com", // addresses are masked
		"How it decided",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, "buyer@example.com") {
		t.Error("the buyer's full address is on the page")
	}
}

func TestTheResultIsShownAfterApproval(t *testing.T) {
	s, deal := testServer(t)
	deal.Result = &act.Result{InvoiceID: "INV2-AAAA-BBBB-CCCC-7CVE", Status: "DRAFT", ReadBackOK: true,
		Checks: []act.ReadCheck{{Field: "quantity", Want: "30", Got: "30", OK: true}}}
	if err := s.deps.Store.Save(deal); err != nil {
		t.Fatal(err)
	}
	page := get(t, s.routes(), "/deal/"+deal.ID)
	if !strings.Contains(page, "is ready in PayPal: status DRAFT") || strings.Contains(page, "Approve and create") {
		t.Error("after approval the page must show the draft and no second Approve button")
	}
}

func TestAPostWithoutTheFormTokenIsRefused(t *testing.T) {
	s, deal := testServer(t)
	for _, path := range []string{"/check", "/deal/" + deal.ID + "/reply", "/deal/" + deal.ID + "/approve"} {
		form := url.Values{"fingerprint": {act.Fingerprint(deal.Proposal.Plan)}, "thread": {"x"}, "reply": {"x"}}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s without the token: %d, want 403", path, rec.Code)
		}
	}
}

func TestTheHomePageRenders(t *testing.T) {
	s, _ := testServer(t)
	if page := get(t, s.routes(), "/"); !strings.Contains(page, "Check the deal") {
		t.Error("no check form on the home page")
	}
}

func TestEarlyPaymentPlanBLabelRendersOnDealPage(t *testing.T) {
	s, deal := testServer(t)
	page := get(t, s.routes(), "/deal/"+deal.ID)
	const planBLabel = "The discount is written on the invoice itself and the invoice is due in 7 days. PayPal's own early-payment rule isn't used, so the buyer keeps the discount even if they pay late."
	if !strings.Contains(page, planBLabel) {
		t.Errorf("early-payment choice rendered on page but lacks Plan B label %q", planBLabel)
	}

	// When early payment is omitted, line must not render
	seller := profit.Seller{UnitCostCents: 2300, ShippingCostCentsPerOrder: 600, ShippingCostCentsPerBox: 150,
		PayPalFeePercentBP: 349, PayPalFixedFeeCents: 49, YearlyCostOfMoneyBP: 1000, UnderlineMinGiveawayBP: 200, Currency: "USD"}
	setNoEarly, err := offer.Build(offer.Request{Currency: "USD", Quantity: 20, UnitPriceCents: 4000, RequestedPaymentDays: 0}, seller, offer.Config{})
	if err != nil {
		t.Fatal(err)
	}
	deal.Offers = &setNoEarly
	if err := s.deps.Store.Save(deal); err != nil {
		t.Fatal(err)
	}
	pageNoEarly := get(t, s.routes(), "/deal/"+deal.ID)
	if strings.Contains(pageNoEarly, planBLabel) {
		t.Errorf("early payment omitted but page still rendered Plan B label")
	}
}

// The screen's server must not wait forever on a slow or stuck connection, but it must leave the slowest real
// step (an approved run, or reading an email twice and drafting the reply) time to finish.
func TestTheServerHasTimeLimits(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 || srv.WriteTimeout <= 0 {
		t.Fatalf("limits: header %v, read %v, idle %v, write %v; want all set",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout, srv.WriteTimeout)
	}
	if srv.WriteTimeout <= flow.RunTimeLimit {
		t.Errorf("write limit %v must be longer than a run's own limit %v", srv.WriteTimeout, flow.RunTimeLimit)
	}
}

// The Approve button says exactly what it will do, in the plan's own terms, so the seller isn't approving a
// vague "create the draft" they haven't read.
func TestTheApproveButtonNamesTheExactAction(t *testing.T) {
	s, deal := testServer(t)
	page := get(t, s.routes(), "/deal/"+deal.ID)
	want := "Create the PayPal draft: 30 boxes at $40.00 each, free shipping, due on receipt"
	if !strings.Contains(page, want) {
		t.Errorf("the Approve button should read %q", want)
	}
	if strings.Contains(page, ">Approve and create the PayPal draft<") {
		t.Error("the vague button label is still there")
	}
}

// After the buyer's answer is read, the seller sees the three choices as buttons, the AI's reading marked, and
// confirms or corrects it with one tap.
func TestTheSellerConfirmsTheBuyersChoiceWithOneTap(t *testing.T) {
	s, deal := testServer(t)
	deal.BuyerReply = "Buyer: Option 2 please."
	deal.ReplyReading = &reader.CheckResult{}
	deal.AIPick = offer.Quantity
	if err := s.deps.Store.Save(deal); err != nil {
		t.Fatal(err)
	}
	page := get(t, s.routes(), "/deal/"+deal.ID)
	for _, want := range []string{`action="/deal/` + deal.ID + `/choose"`, "Option 1 (Standard)", "Option 2 (Quantity)", "the AI&#39;s reading"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}

	form := url.Values{"csrf": {s.csrf}, "kind": {"standard"}}
	req := httptest.NewRequest(http.MethodPost, "/deal/"+deal.ID+"/choose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST choose: %d %s", rec.Code, rec.Body.String())
	}
	got, err := s.deps.Store.Load(deal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Accepted != offer.Standard || got.Proposal == nil {
		t.Errorf("accepted %q: the seller's tap must set the choice and its plan", got.Accepted)
	}
}
