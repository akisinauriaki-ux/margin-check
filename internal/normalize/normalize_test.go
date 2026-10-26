package normalize

import (
	"errors"
	"testing"
)

func TestNormalizeTable(t *testing.T) {
	tests := []struct {
		name     string
		kind     Kind
		input    string
		wantInt  int64
		wantCurr string
		wantErr  bool
	}{
		// 1-10: Number words (percent, money, boxes, dozen)
		{name: "ten percent word", kind: KindPercent, input: "ten percent", wantInt: 10},
		{name: "fifteen percent word", kind: KindPercent, input: "fifteen percent", wantInt: 15},
		{name: "twenty percent word", kind: KindPercent, input: "twenty percent discount", wantInt: 20},
		{name: "half off phrase", kind: KindPercent, input: "half off", wantInt: 50},
		{name: "forty-two dollars word", kind: KindMoney, input: "forty-two dollars", wantInt: 4200, wantCurr: "USD"},
		{name: "thirty five dollars space", kind: KindMoney, input: "thirty five dollars", wantInt: 3500, wantCurr: "USD"},
		{name: "one hundred twenty euros", kind: KindMoney, input: "one hundred twenty euros", wantInt: 12000, wantCurr: "EUR"},
		{name: "fifty boxes word", kind: KindQuantity, input: "fifty boxes", wantInt: 50},
		{name: "forty units word", kind: KindQuantity, input: "forty units", wantInt: 40},
		{name: "a dozen phrase", kind: KindQuantity, input: "a dozen", wantInt: 12},
		{name: "two dozen boxes", kind: KindQuantity, input: "two dozen boxes", wantInt: 24},
		{name: "three dozen units", kind: KindQuantity, input: "three dozen", wantInt: 36},

		// 13-22: Payment days ("net 60", "two months", "a week", "due on receipt", etc.)
		{name: "net 60", kind: KindPaymentDays, input: "net 60", wantInt: 60},
		{name: "net 60 days", kind: KindPaymentDays, input: "net 60 days", wantInt: 60},
		{name: "two months", kind: KindPaymentDays, input: "two months", wantInt: 60},
		{name: "a month", kind: KindPaymentDays, input: "a month", wantInt: 30},
		{name: "net 30", kind: KindPaymentDays, input: "net 30", wantInt: 30},
		{name: "net 15 days", kind: KindPaymentDays, input: "net 15 days", wantInt: 15},
		{name: "net 14 days", kind: KindPaymentDays, input: "net 14 days", wantInt: 14},
		{name: "two weeks", kind: KindPaymentDays, input: "two weeks", wantInt: 14},
		{name: "a week", kind: KindPaymentDays, input: "a week", wantInt: 7},
		{name: "within 7 days", kind: KindPaymentDays, input: "paid within 7 days", wantInt: 7},
		{name: "net 7", kind: KindPaymentDays, input: "net 7", wantInt: 7},
		{name: "due on receipt", kind: KindPaymentDays, input: "due on receipt", wantInt: 0},
		{name: "upon receipt", kind: KindPaymentDays, input: "prompt payment upon receipt", wantInt: 0},

		// 26-34: k notation, comma thousands, cents, symbols and currency codes ($, USD, €, EUR, £, GBP)
		{name: "1.2k quantity", kind: KindQuantity, input: "1.2k", wantInt: 1200},
		{name: "2.5k boxes", kind: KindQuantity, input: "2.5k boxes", wantInt: 2500},
		{name: "$1,200.00 cents", kind: KindMoney, input: "$1,200.00", wantInt: 120000, wantCurr: "USD"},
		{name: "$40 per box", kind: KindMoney, input: "$40 per box", wantInt: 4000, wantCurr: "USD"},
		{name: "USD code prefix", kind: KindMoney, input: "USD 38.50", wantInt: 3850, wantCurr: "USD"},
		{name: "USD code suffix", kind: KindMoney, input: "45 USD", wantInt: 4500, wantCurr: "USD"},
		{name: "euro symbol", kind: KindMoney, input: "€1,200.00", wantInt: 120000, wantCurr: "EUR"},
		{name: "EUR code", kind: KindMoney, input: "85.25 EUR", wantInt: 8525, wantCurr: "EUR"},
		{name: "pound symbol", kind: KindMoney, input: "£40", wantInt: 4000, wantCurr: "GBP"},
		{name: "GBP code", kind: KindMoney, input: "GBP 120.50", wantInt: 12050, wantCurr: "GBP"},
		{name: "$1.2k money", kind: KindMoney, input: "$1.2k", wantInt: 120000, wantCurr: "USD"},
		{name: "5% discount", kind: KindPercent, input: "5% discount", wantInt: 5},
		{name: "35% off", kind: KindPercent, input: "35% off", wantInt: 35},

		// 39-45: Unparseable inputs must return ErrCantParse ("can't parse"), never a guess
		{name: "unparseable quantity", kind: KindQuantity, input: "several boxes", wantErr: true},
		{name: "unparseable money without currency", kind: KindMoney, input: "40 per box", wantErr: true},
		{name: "unparseable money text", kind: KindMoney, input: "market rate", wantErr: true},
		{name: "unparseable percent", kind: KindPercent, input: "a fair discount", wantErr: true},
		{name: "unparseable payment days", kind: KindPaymentDays, input: "confirm payment terms next week", wantErr: true},
		{name: "empty input", kind: KindQuantity, input: "   ", wantErr: true},
	}

	if len(tests) < 30 {
		t.Fatalf("expected at least 30 rows in test table, got %d", len(tests))
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.kind, tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q, %q) = %+v, want ErrCantParse", tc.kind, tc.input, got)
				}
				if !errors.Is(err, ErrCantParse) {
					t.Fatalf("Parse(%q, %q) error = %v, want ErrCantParse (%v)", tc.kind, tc.input, err, ErrCantParse)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q, %q) unexpected error: %v", tc.kind, tc.input, err)
			}
			if got.Int != tc.wantInt {
				t.Errorf("Parse(%q, %q).Int = %d, want %d", tc.kind, tc.input, got.Int, tc.wantInt)
			}
			if got.Currency != tc.wantCurr {
				t.Errorf("Parse(%q, %q).Currency = %q, want %q", tc.kind, tc.input, got.Currency, tc.wantCurr)
			}
		})
	}
}

func TestFreeShippingPhrases(t *testing.T) {
	tests := []struct {
		phrase    string
		wantAgree bool
	}{
		// Required positive rows
		{"include the shipping", true},
		{"shipping at no charge", true},
		{"shipping is on us", true},
		{"we'll cover shipping", true},
		{"shipping is free", true},
		{"no charge for shipping", true},

		// Additional positive variants
		{"I'll include the shipping at no charge", true},
		{"we will cover shipping", true},
		{"we'll cover the shipping", true},
		{"free shipping", true},
		{"free delivery", true},
		{"shipping included", true},
		{"shipping is included", true},
		{"no shipping fee", true},
		{"no shipping charge", true},

		// Required negative rows
		{"free shipping over $500", false},
		{"shipping is $20", false},
		{"can you do free shipping?", false},

		// Additional negative rows (conditional, questions, charges)
		{"free shipping on orders over $100", false},
		{"can you provide free shipping?", false},
		{"could you include free shipping?", false},
		{"can we get free shipping?", false},
		{"shipping will be $15", false},
		{"shipping cost is $25", false},
		{"standard shipping applies", false},
		{"shipping is not included", false},
	}

	if len(tests) < 12 {
		t.Fatalf("expected at least 12 rows, got %d", len(tests))
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.phrase, func(t *testing.T) {
			got := IsFreeShipping(tc.phrase)
			if got != tc.wantAgree {
				t.Errorf("IsFreeShipping(%q) = %v, want %v", tc.phrase, got, tc.wantAgree)
			}
		})
	}
}
