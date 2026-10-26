package offer

import (
	"strings"
	"testing"

	"margincheck/internal/profit"
)

func sampleSeller() profit.Seller {
	return profit.Seller{
		UnitCostCents:             2300,
		ShippingCostCentsPerOrder: 600,
		ShippingCostCentsPerBox:   150,
		PayPalFeePercentBP:        349,
		PayPalFixedFeeCents:       49,
		YearlyCostOfMoneyBP:       1000,
		UnderlineMinGiveawayBP:    200,
		Currency:                  "USD",
	}
}

func byKind(t *testing.T, s Set, k Kind) Option {
	t.Helper()
	for _, o := range s.Options {
		if o.Kind == k {
			return o
		}
	}
	t.Fatalf("no %s option; omitted: %v", k, s.Omitted)
	return Option{}
}

// Exam email 17: the buyer asks for 20 boxes at $40 with free shipping and two months to pay.
// Hand calculation (seller.json sample values, rounding half away from zero per line):
//
//	standard 20 boxes, shipping charged, on receipt: 80000 - 46000 - (2792+49) = 31159
//	quantity: 30 boxes free shipping: 120000 - 69000 - (600+4500) - (4188+49) = 41663 >= 31159, so 30
//	          (the first step of 10 above 20)
//	early, 60 days asked -> 7 days: normal profit 80000 - 46000 - 1315 - 2841 = 29844
//	          at 1.5%: 78800 - 46000 - 151 - (2750+49) = 29850 >= 29844
//	          at 1.6%: 78720 - 46000 - 151 - (2747+49) = 29773 <  29844, so break-even is 150 bp
func TestEmail17HandChecked(t *testing.T) {
	s, err := Build(Request{Currency: "USD", Quantity: 20, UnitPriceCents: 4000, RequestedPaymentDays: 60}, sampleSeller(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Options) != 3 {
		t.Fatalf("got %d options, want 3; omitted %v", len(s.Options), s.Omitted)
	}

	std := byKind(t, s, Standard)
	if std.Quantity != 20 || std.FreeShipping || std.ShippingChargeCents != 3600 || std.DueDays != 0 || std.DiscountBP != 0 {
		t.Errorf("standard %+v", std)
	}
	if std.Profit.ProfitCents != 31159 {
		t.Errorf("standard keeps %d, want 31159", std.Profit.ProfitCents)
	}

	q := byKind(t, s, Quantity)
	if q.Quantity != 30 || !q.FreeShipping || q.ShippingChargeCents != 0 {
		t.Errorf("quantity %+v", q)
	}
	if q.Profit.ProfitCents != 41663 {
		t.Errorf("quantity keeps %d, want 41663", q.Profit.ProfitCents)
	}

	e := byKind(t, s, EarlyPayment)
	if e.Quantity != 20 || e.DiscountBP != 150 || e.DueDays != 7 || e.FreeShipping || e.ShippingChargeCents != 3600 {
		t.Errorf("early %+v", e)
	}
	if e.Profit.ProfitCents != 29850 {
		t.Errorf("early keeps %d, want 29850", e.Profit.ProfitCents)
	}
	if e.BaselineProfitCents != 29844 {
		t.Errorf("early baseline (the buyer's asked terms) %d, want 29844", e.BaselineProfitCents)
	}
}

// Exam email 18: 25 boxes at $38, the buyer asks for a discount and free shipping but no longer terms.
//
//	standard: 95000 - 57500 - (3316+49) = 34135
//	quantity: 30 boxes free shipping: 114000 - 69000 - 5100 - (3979+49) = 35872 >= 34135, so 30
//	early: left out, because the buyer did not ask to pay later than 7 days
func TestEmail18HandChecked(t *testing.T) {
	s, err := Build(Request{Currency: "USD", Quantity: 25, UnitPriceCents: 3800}, sampleSeller(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := byKind(t, s, Standard).Profit.ProfitCents; got != 34135 {
		t.Errorf("standard keeps %d, want 34135", got)
	}
	q := byKind(t, s, Quantity)
	if q.Quantity != 30 || q.Profit.ProfitCents != 35872 {
		t.Errorf("quantity %d boxes keeps %d, want 30 and 35872", q.Quantity, q.Profit.ProfitCents)
	}
	if len(s.Options) != 2 || len(s.Omitted) != 1 || !strings.Contains(s.Omitted[0], "early") {
		t.Errorf("options %d, omitted %v: the early choice must be left out with a reason", len(s.Options), s.Omitted)
	}
}

func TestEveryChoiceHasAFairReasonAndAHowOrWhatQuestion(t *testing.T) {
	s, err := Build(Request{Currency: "USD", Quantity: 20, UnitPriceCents: 4000, RequestedPaymentDays: 60}, sampleSeller(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range s.Options {
		if o.Reason == "" || strings.ContainsAny(o.Reason, "0123456789$€£%") {
			t.Errorf("%s: reason %q must exist and carry no numbers", o.Kind, o.Reason)
		}
		if !strings.HasPrefix(o.Question, "How ") && !strings.HasPrefix(o.Question, "What ") {
			t.Errorf("%s: question %q must start with How or What", o.Kind, o.Question)
		}
		if o.Summary == "" {
			t.Errorf("%s: no summary", o.Kind)
		}
	}
}

func TestTheChoicesKeepWhatTheyPromise(t *testing.T) {
	seller := sampleSeller()
	for _, req := range []Request{
		{Currency: "USD", Quantity: 5, UnitPriceCents: 3000, RequestedPaymentDays: 90},
		{Currency: "USD", Quantity: 12, UnitPriceCents: 3500, RequestedPaymentDays: 30},
		{Currency: "USD", Quantity: 47, UnitPriceCents: 4200, RequestedPaymentDays: 45},
		{Currency: "USD", Quantity: 100, UnitPriceCents: 2900, RequestedPaymentDays: 60},
	} {
		s, err := Build(req, seller, Config{})
		if err != nil {
			t.Fatalf("%+v: %v", req, err)
		}
		std := byKind(t, s, Standard)
		for _, o := range s.Options {
			switch o.Kind {
			case Quantity:
				if o.Profit.ProfitCents < std.Profit.ProfitCents {
					t.Errorf("%+v: quantity keeps %d < standard %d", req, o.Profit.ProfitCents, std.Profit.ProfitCents)
				}
				if o.Quantity <= req.Quantity || o.Quantity%10 != 0 {
					t.Errorf("%+v: quantity %d must be a step of 10 above the asked %d", req, o.Quantity, req.Quantity)
				}
			case EarlyPayment:
				if o.Profit.ProfitCents < o.BaselineProfitCents {
					t.Errorf("%+v: early keeps %d < the asked terms %d", req, o.Profit.ProfitCents, o.BaselineProfitCents)
				}
			}
		}
	}
}

// Never split the difference: the early-payment discount is exactly the break-even from the profit engine,
// never a midpoint between what the buyer asked for and nothing.
func TestTheEarlyDiscountIsTheBreakEvenNotAMidpoint(t *testing.T) {
	seller := sampleSeller()
	req := Request{Currency: "USD", Quantity: 20, UnitPriceCents: 4000, RequestedPaymentDays: 60, RequestedDiscountBP: 2000}
	s, err := Build(req, seller, Config{})
	if err != nil {
		t.Fatal(err)
	}
	e := byKind(t, s, EarlyPayment)
	be, err := profit.EarlyPaymentWorthIt(profit.Deal{Currency: "USD", Quantity: 20, UnitPriceCents: 4000}, seller, 60, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if e.DiscountBP != be.BreakEvenDiscountBP {
		t.Errorf("early discount %d bp, want the break-even %d bp", e.DiscountBP, be.BreakEvenDiscountBP)
	}
	if e.DiscountBP == req.RequestedDiscountBP/2 {
		t.Errorf("early discount %d bp is half of the asked %d bp: never split the difference", e.DiscountBP, req.RequestedDiscountBP)
	}
}

func TestRefusesWhatItCannotPriceHonestly(t *testing.T) {
	seller := sampleSeller()
	for name, req := range map[string]Request{
		"no currency":    {Quantity: 20, UnitPriceCents: 4000},
		"other currency": {Currency: "EUR", Quantity: 20, UnitPriceCents: 4000},
		"no quantity":    {Currency: "USD", UnitPriceCents: 4000},
		"no price":       {Currency: "USD", Quantity: 20},
	} {
		if _, err := Build(req, seller, Config{}); err == nil {
			t.Errorf("%s: Build succeeded, want an error", name)
		}
	}
}

func TestFormatting(t *testing.T) {
	cases := map[string]string{
		FormatMoney(4000, "USD"):   "$40.00",
		FormatMoney(-1987, "USD"):  "-$19.87",
		FormatMoney(5, "EUR"):      "€0.05",
		FormatMoney(123456, "JPY"): "JPY 1234.56",
		FormatPercent(150):         "1.5%",
		FormatPercent(1000):        "10%",
		FormatPercent(125):         "1.25%",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// A choice that loses money is never offered. 20 boxes at $23.50 against a $23.00 unit cost and PayPal's fee:
// standard keeps 47000 - 46000 - (1640+49) = -689 cents, and the early-payment choice loses more.
func TestAChoiceThatLosesMoneyIsNeverOffered(t *testing.T) {
	s, err := Build(Request{Currency: "USD", Quantity: 20, UnitPriceCents: 2350, RequestedPaymentDays: 60}, sampleSeller(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range s.Options {
		if o.Profit.ProfitCents < 0 {
			t.Errorf("%s offered while it keeps %d cents", o.Kind, o.Profit.ProfitCents)
		}
	}
	joined := strings.Join(s.Omitted, "; ")
	if !strings.Contains(joined, "standard: at this price the order loses money") {
		t.Errorf("omitted %q: the seller must be told why the standard choice is missing", joined)
	}
}
