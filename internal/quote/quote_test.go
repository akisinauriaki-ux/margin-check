package quote

import (
	"testing"
)

func TestQuoteTable(t *testing.T) {
	emailThread := `Buyer:
> Hello,
> We are checking whether we can reorder 60 units of organic
> honey jars.
> Can you provide a “ten percent” discount — and ‘free shipping’ on this order?

Seller:
> Yes, fifty boxes of herbal tea tins will be $45 per box.
> We can approve payment terms of net 15 days for your account.
> Shipping is $20 for the order, and payment is due on receipt for smaller orders.
> For large orders, we'll include the shipping at no charge.`

	tests := []struct {
		name      string
		email     string
		sentence  string
		field     string
		intVal    int64
		boolVal   bool
		strVal    string
		wantMatch bool
		wantErr   bool
	}{
		{
			name:      "reply chain and wrapped lines match quantity",
			email:     emailThread,
			sentence:  "We are checking whether we can reorder 60 units of organic honey jars.",
			field:     "quantity",
			intVal:    60,
			wantMatch: true,
			wantErr:   false,
		},
		{
			name:      "curly quotes and em-dash normalised and number word ten percent backs 10",
			email:     emailThread,
			sentence:  `Can you provide a "ten percent" discount - and 'free shipping' on this order?`,
			field:     "discount_percent",
			intVal:    10,
			wantMatch: true,
			wantErr:   false,
		},
		{
			name:      "number word fifty boxes backs 50 and $45 backs 4500 cents",
			email:     emailThread,
			sentence:  "Yes, fifty boxes of herbal tea tins will be $45 per box.",
			field:     "unit_price_cents",
			intVal:    4500,
			wantMatch: true,
			wantErr:   false,
		},
		{
			name:      "net 15 days backs payment_days 15",
			email:     emailThread,
			sentence:  "We can approve payment terms of net 15 days for your account.",
			field:     "payment_days",
			intVal:    15,
			wantMatch: true,
			wantErr:   false,
		},
		{
			name:      "buyer asking for free shipping does not back free_shipping=true",
			email:     emailThread,
			sentence:  `Can you provide a "ten percent" discount - and 'free shipping' on this order?`,
			field:     "free_shipping",
			boolVal:   true,
			wantMatch: false,
			wantErr:   true,
		},
		{
			name:      "agreed free shipping at no charge backs free_shipping=true",
			email:     emailThread,
			sentence:  "For large orders, we'll include the shipping at no charge.",
			field:     "free_shipping",
			boolVal:   true,
			wantMatch: true,
			wantErr:   false,
		},
		{
			name:      "free shipping false backed by paid shipping sentence",
			email:     emailThread,
			sentence:  "Shipping is $20 for the order, and payment is due on receipt for smaller orders.",
			field:     "free_shipping",
			boolVal:   false,
			wantMatch: true,
			wantErr:   false,
		},
		{
			name:      "quote not in email is rejected",
			email:     emailThread,
			sentence:  "We can offer a 25% discount on 100 boxes.",
			field:     "discount_percent",
			intVal:    25,
			wantMatch: false,
			wantErr:   true,
		},
		{
			name:      "quote is in email but does not contain claimed discount value",
			email:     emailThread,
			sentence:  "Yes, fifty boxes of herbal tea tins will be $45 per box.",
			field:     "discount_percent",
			intVal:    20,
			wantMatch: false,
			wantErr:   true,
		},
		{
			name:      "quote is in email but has wrong quantity",
			email:     emailThread,
			sentence:  "Yes, fifty boxes of herbal tea tins will be $45 per box.",
			field:     "quantity",
			intVal:    40,
			wantMatch: false,
			wantErr:   true,
		},
		{
			name:      "empty quote is rejected",
			email:     emailThread,
			sentence:  "   ",
			field:     "quantity",
			intVal:    60,
			wantMatch: false,
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Verify(tc.email, tc.sentence, tc.field, tc.intVal, tc.boolVal, tc.strVal)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Verify(%q, %q) succeeded (%+v), want error", tc.sentence, tc.field, res)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify(%q, %q) unexpected error: %v", tc.sentence, tc.field, err)
			}
			if res.Original != tc.sentence {
				t.Errorf("Original = %q, want %q", res.Original, tc.sentence)
			}
			if res.Normalized == "" {
				t.Errorf("Normalized should not be empty")
			}
		})
	}
}
