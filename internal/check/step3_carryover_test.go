package check_test

import (
	"testing"

	"margincheck/internal/check"
	"margincheck/internal/reader"
)

// Step-3 carry-overs from the step-2 review (docs/briefs/SPEC-paypal-build-step-3.md, item 0).

func hasQuestion(qs []string, want string) bool {
	for _, q := range qs {
		if q == want {
			return true
		}
	}
	return false
}

func TestCurrencyComesFromThePriceQuoteWhenTermsCarryNone(t *testing.T) {
	reading := baseSureResult()
	reading.Terms = []reader.TermResult{
		{Name: "quantity", Value: 40, By: "buyer", Status: "agreed"},
		{Name: "unit_price_cents", Value: 4000, By: "seller", Status: "agreed"},
	}
	res := check.Check(reading, sampleSeller())
	if res.Outcome != check.OutcomeEvaluated {
		t.Fatalf("outcome %s, questions %v, refusal %q", res.Outcome, res.Questions, res.RefusalReason)
	}
	if res.Deal.Currency != "USD" {
		t.Errorf("deal currency %q, want USD from the quote %q", res.Deal.Currency, reading.UnitPriceCents.Quote)
	}
}

func TestNoCurrencyAnywhereAsksInsteadOfAssuming(t *testing.T) {
	reading := baseSureResult()
	reading.UnitPriceCents.Quote = "It is 40 per box."
	reading.Terms = []reader.TermResult{
		{Name: "quantity", Value: 40, By: "buyer", Status: "agreed"},
		{Name: "unit_price_cents", Value: 4000, By: "seller", Status: "agreed"},
	}
	res := check.Check(reading, sampleSeller())
	if res.Outcome != check.OutcomeAsk {
		t.Fatalf("outcome %s, want ask; deal %+v", res.Outcome, res.Deal)
	}
	if !hasQuestion(res.Questions, "currency") {
		t.Errorf("questions %v, want \"currency\"", res.Questions)
	}
	if res.Profit != nil {
		t.Error("a money figure was computed although the currency is unknown")
	}
}

func TestARequestInAnotherCurrencyDoesNotMakeTheDealMixed(t *testing.T) {
	reading := baseSureResult()
	reading.Terms = append(reading.Terms,
		reader.TermResult{Name: "unit_price_cents", Value: 3500, Currency: "EUR", By: "buyer", Status: "requested"})
	res := check.Check(reading, sampleSeller())
	if res.Outcome != check.OutcomeEvaluated {
		t.Fatalf("outcome %s, refusal %q: a buyer's request is not part of the deal's money", res.Outcome, res.RefusalReason)
	}
}

func TestAnOfferedTermInAnotherCurrencyIsStillRefused(t *testing.T) {
	reading := baseSureResult()
	reading.Terms = append(reading.Terms,
		reader.TermResult{Name: "shipping_cents", Value: 4500, Currency: "EUR", By: "seller", Status: "offered"})
	res := check.Check(reading, sampleSeller())
	if res.Outcome != check.OutcomeRefused {
		t.Fatalf("outcome %s, want refused for USD price with EUR shipping offered", res.Outcome)
	}
}

func TestDiscountedPriceIncreaseWithoutOldPriceAsks(t *testing.T) {
	reading := baseSureResult()
	reading.Quantity.Value = 30
	reading.UnitPriceCents.Value = 4400
	reading.UnitPriceCents.Quote = "The new price is $44 per box."
	reading.DiscountPercent.Value = 5
	reading.PriceIncreaseDiscounted.Value = true
	reading.PriceIncreaseDiscounted.Quote = "Please apply your 5% discount to the new price."
	reading.Terms = []reader.TermResult{
		{Name: "unit_price_cents", Value: 4400, Currency: "USD", By: "seller", Status: "agreed"},
	}
	res := check.Check(reading, sampleSeller())
	if res.Outcome != check.OutcomeAsk {
		t.Fatalf("outcome %s, want ask: the double-discount warning must never silently disappear", res.Outcome)
	}
	if !hasQuestion(res.Questions, "old_unit_price") {
		t.Errorf("questions %v, want \"old_unit_price\"", res.Questions)
	}
}

func TestOldPriceIsFoundInTheEmailWhenQuotesNameOnlyTheNewPrice(t *testing.T) {
	reading := baseSureResult()
	reading.Quantity.Value = 30
	reading.UnitPriceCents.Value = 4400
	reading.UnitPriceCents.Quote = "We will apply a 5% discount to the new $44 price for your 30 boxes."
	reading.DiscountPercent.Value = 5
	reading.PriceIncreaseDiscounted.Value = true
	reading.PriceIncreaseDiscounted.Quote = reading.UnitPriceCents.Quote
	email := "Seller:\n> Our price per box increases from $40 to $44 starting this week.\n" +
		"An older note said prices went from $30 to $35 last year.\n"
	res := check.CheckEmail(reading, sampleSeller(), email)
	if res.Outcome != check.OutcomeEvaluated {
		t.Fatalf("outcome %s, questions %v", res.Outcome, res.Questions)
	}
	if res.Deal.OldUnitPriceCents != 4000 {
		t.Errorf("old price %d, want 4000 (the increase that ends at the agreed $44, not the $30 one)", res.Deal.OldUnitPriceCents)
	}
	if res.Profit.DoubleDiscountCents != 600 {
		t.Errorf("double discount %d cents, want 600", res.Profit.DoubleDiscountCents)
	}
}
