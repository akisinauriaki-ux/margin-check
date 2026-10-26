package profit

import (
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func defaultTestSeller() Seller {
	return Seller{
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

// -----------------------------------------------------------------------------
// Exam 20 Emails Test
// -----------------------------------------------------------------------------

type examEmailAnswer struct {
	EmailID                string          `json:"email_id"`
	ExpectedLevel          string          `json:"expected_level"`
	ExpectedProfitCents    int64           `json:"expected_profit_cents"`
	ExpectedGiveawayCents  int64           `json:"expected_giveaway_cents"`
	Quantity               json.RawMessage `json:"quantity"`
	UnitPriceCents         json.RawMessage `json:"unit_price_cents"`
	DiscountPercent        json.RawMessage `json:"discount_percent"`
	RebatePercent          json.RawMessage `json:"rebate_percent"`
	FreeShipping           json.RawMessage `json:"free_shipping"`
	PaymentDays            json.RawMessage `json:"payment_days"`
	PriceIncreaseDisc      json.RawMessage `json:"price_increase_discounted"`
}

func parseExamFieldValue(raw json.RawMessage) (interface{}, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var obj struct {
		Value interface{} `json:"value"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Value != nil {
		if s, ok := obj.Value.(string); ok && s == "UNKNOWN" {
			return nil, false
		}
		return obj.Value, true
	}
	var direct interface{}
	if err := json.Unmarshal(raw, &direct); err == nil && direct != nil {
		if s, ok := direct.(string); ok && s == "UNKNOWN" {
			return nil, false
		}
		return direct, true
	}
	return nil, false
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

func TestAll20ExamEmails(t *testing.T) {
	answersPath := filepath.Join("..", "..", "testdata", "answers.json")
	answersData, err := os.ReadFile(answersPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", answersPath, err)
	}

	var answers []examEmailAnswer
	if err := json.Unmarshal(answersData, &answers); err != nil {
		t.Fatalf("failed to parse %s: %v", answersPath, err)
	}

	sellerPath := filepath.Join("..", "..", "testdata", "seller.json")
	seller, err := LoadSeller(sellerPath)
	if err != nil {
		t.Fatalf("LoadSeller(%s) failed: %v", sellerPath, err)
	}

	for _, a := range answers {
		t.Run("Email_"+a.EmailID, func(t *testing.T) {
			qVal, qOK := parseExamFieldValue(a.Quantity)
			pVal, pOK := parseExamFieldValue(a.UnitPriceCents)
			if !qOK || !pOK {
				t.Skipf("Email %s has unknown quantity or unit price", a.EmailID)
				return
			}

			q := toInt64(qVal)
			p := toInt64(pVal)

			var discBP int64
			if dVal, ok := parseExamFieldValue(a.DiscountPercent); ok {
				discBP = toInt64(dVal) * 100
			}

			var rebateBP int64
			if rVal, ok := parseExamFieldValue(a.RebatePercent); ok {
				rebateBP = toInt64(rVal) * 100
			}

			var freeShip bool
			if fsVal, ok := parseExamFieldValue(a.FreeShipping); ok {
				if b, ok := fsVal.(bool); ok {
					freeShip = b
				}
			}

			var payDays int64
			if pdVal, ok := parseExamFieldValue(a.PaymentDays); ok {
				payDays = toInt64(pdVal)
			}

			var oldPrice int64
			if piVal, ok := parseExamFieldValue(a.PriceIncreaseDisc); ok {
				if b, ok := piVal.(bool); ok && b {
					// Email 08 has price hike $40 -> $44
					if a.EmailID == "08" {
						oldPrice = 4000
					}
				}
			}

			deal := Deal{
				Currency:            "USD",
				Quantity:            q,
				UnitPriceCents:      p,
				OrderSizeDiscountBP: discBP,
				RebateBP:            rebateBP,
				FreeShipping:        freeShip,
				PaymentDays:         payDays,
				OldUnitPriceCents:   oldPrice,
			}

			res, err := Evaluate(deal, seller)
			if err != nil {
				t.Fatalf("Evaluate failed on email %s: %v", a.EmailID, err)
			}

			diffProf := res.ProfitCents - a.ExpectedProfitCents
			if diffProf < -1 || diffProf > 1 {
				t.Errorf("profit mismatch on %s: got %d, expected %d (diff %d cents)", a.EmailID, res.ProfitCents, a.ExpectedProfitCents, diffProf)
			}

			diffGive := res.GiveawayCents - a.ExpectedGiveawayCents
			if diffGive < -1 || diffGive > 1 {
				t.Errorf("giveaway mismatch on %s: got %d, expected %d (diff %d cents)", a.EmailID, res.GiveawayCents, a.ExpectedGiveawayCents, diffGive)
			}

			if res.Level != a.ExpectedLevel {
				t.Errorf("level mismatch on %s: got %s, expected %s", a.EmailID, res.Level, a.ExpectedLevel)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Hand Cases for What the Exam Doesn't Cover
// -----------------------------------------------------------------------------

func TestEarlyPaymentAnchorCase(t *testing.T) {
	seller := defaultTestSeller()
	deal := Deal{
		Currency:       "USD",
		Quantity:       30,
		UnitPriceCents: 4000,
		FreeShipping:   false,
	}

	normalDays := int64(60)
	earlyDays := int64(7)
	discountBP := int64(200) // 2.0%

	res, err := EarlyPaymentWorthIt(deal, seller, normalDays, earlyDays, discountBP)
	if err != nil {
		t.Fatalf("EarlyPaymentWorthIt failed: %v", err)
	}

	// Normal profit must equal $447.90 (44790 cents)
	if res.NormalProfitCents != 44790 {
		t.Errorf("normal profit: got %d, want 44790 ($447.90)", res.NormalProfitCents)
	}

	// Early profit with 2% discount must equal $442.21 (44221 cents)
	if res.EarlyProfitCents != 44221 {
		t.Errorf("early profit at 2%%: got %d, want 44221 ($442.21)", res.EarlyProfitCents)
	}

	// 2% is not worth it
	if res.WorthIt {
		t.Errorf("expected 2%% discount to NOT be worth it, got true")
	}

	// Break-even discount must equal 150 BP (1.5%)
	if res.BreakEvenDiscountBP != 150 {
		t.Errorf("break-even discount: got %d BP, want 150 BP (1.5%%)", res.BreakEvenDiscountBP)
	}

	// Verify profit at break-even 1.5% (150 BP) >= normal profit
	deal15 := deal
	deal15.EarlyPaymentDiscountBP = 150
	deal15.EarlyPaymentDays = earlyDays
	deal15.PaymentDays = earlyDays
	res15, err := Evaluate(deal15, seller)
	if err != nil {
		t.Fatalf("Evaluate at 1.5%% failed: %v", err)
	}
	if res15.ProfitCents != 44799 {
		t.Errorf("profit at 1.5%% break-even: got %d, want 44799 ($447.99)", res15.ProfitCents)
	}

	// Verify profit at 1.6% (160 BP) < normal profit
	deal16 := deal
	deal16.EarlyPaymentDiscountBP = 160
	deal16.EarlyPaymentDays = earlyDays
	deal16.PaymentDays = earlyDays
	res16, err := Evaluate(deal16, seller)
	if err != nil {
		t.Fatalf("Evaluate at 1.6%% failed: %v", err)
	}
	if res16.ProfitCents != 44684 {
		t.Errorf("profit at 1.6%%: got %d, want 44684 ($446.84)", res16.ProfitCents)
	}
}

func TestEarlyPaymentAnchorPlusOrderSizeDiscount(t *testing.T) {
	seller := defaultTestSeller()
	// Anchor deal plus a 5% order-size discount (500 BP)
	deal := Deal{
		Currency:            "USD",
		Quantity:            30,
		UnitPriceCents:      4000,
		OrderSizeDiscountBP: 500,
		FreeShipping:        false,
	}

	normalDays := int64(60)
	earlyDays := int64(7)
	discountBP := int64(200) // 2.0%

	res, err := EarlyPaymentWorthIt(deal, seller, normalDays, earlyDays, discountBP)
	if err != nil {
		t.Fatalf("EarlyPaymentWorthIt failed: %v", err)
	}

	// Normal profit: 30 boxes * $40 = $1,200 list value.
	// 5% order-size discount = $60, Net revenue = $1,140 (114,000 cents).
	// Base unit cost = 30 * $23 = $690 (69,000 cents).
	// Waiting cost (60 days) = roundDiv(114000 * 1000 * 60, 3650000) = 1,874 cents ($18.74).
	// PayPal fee = roundDiv(114000 * 349, 10000) + 49 = 3,979 + 49 = 4,028 cents ($40.28).
	// Total costs = 69000 + 1874 + 4028 = 74,902 cents ($749.02).
	// Normal profit must equal $390.98 (39098 cents).
	if res.NormalProfitCents != 39098 {
		t.Errorf("normal profit with 5%% order-size discount: got %d, want 39098 ($390.98)", res.NormalProfitCents)
	}

	// Early profit with 2% early-payment discount:
	// Early discount on $1,140 = roundDiv(114000 * 200, 10000) = $22.80 (2,280 cents).
	// Net revenue = 114,000 - 2,280 = $1,117.20 (111,720 cents).
	// Base unit cost = $690 (69,000 cents).
	// Waiting cost (7 days) = roundDiv(111720 * 1000 * 7, 3650000) = 214 cents ($2.14).
	// PayPal fee = roundDiv(111720 * 349, 10000) + 49 = 3,899 + 49 = 3,948 cents ($39.48).
	// Total costs = 69000 + 214 + 3948 = 73,162 cents ($731.62).
	// Early profit must equal $385.58 (38558 cents).
	if res.EarlyProfitCents != 38558 {
		t.Errorf("early profit with 2%% early discount: got %d, want 38558 ($385.58)", res.EarlyProfitCents)
	}

	// 2% is not worth it ($385.58 < $390.98)
	if res.WorthIt {
		t.Errorf("expected 2%% early discount to NOT be worth it, got true")
	}

	// Break-even discount must equal 150 BP (1.5%)
	if res.BreakEvenDiscountBP != 150 {
		t.Errorf("break-even discount: got %d BP, want 150 BP (1.5%%)", res.BreakEvenDiscountBP)
	}

	// Verify profit at break-even 1.5% (150 BP) >= normal profit:
	// Early discount = roundDiv(114000 * 150, 10000) = 1,710 cents ($17.10).
	// Net revenue = 112,290 cents ($1,122.90).
	// Waiting cost (7 days) = 215 cents ($2.15).
	// PayPal fee = 3,919 + 49 = 3,968 cents ($39.68).
	// Total costs = 69000 + 215 + 3968 = 73,183 cents ($731.83).
	// Profit at 150 BP = 112,290 - 73,183 = 39,107 cents ($391.07) >= 39,098 cents.
	deal15 := deal
	deal15.EarlyPaymentDiscountBP = 150
	deal15.EarlyPaymentDays = earlyDays
	deal15.PaymentDays = earlyDays
	res15, err := Evaluate(deal15, seller)
	if err != nil {
		t.Fatalf("Evaluate at 1.5%% failed: %v", err)
	}
	if res15.ProfitCents != 39107 {
		t.Errorf("profit at 1.5%% break-even: got %d, want 39107 ($391.07)", res15.ProfitCents)
	}

	// Verify profit at 1.6% (160 BP) < normal profit:
	// Early discount = roundDiv(114000 * 160, 10000) = 1,824 cents ($18.24).
	// Net revenue = 112,176 cents ($1,121.76).
	// Waiting cost (7 days) = 215 cents ($2.15).
	// PayPal fee = 3,915 + 49 = 3,964 cents ($39.64).
	// Total costs = 69000 + 215 + 3964 = 73,179 cents ($731.79).
	// Profit at 160 BP = 112,176 - 73,179 = 38,997 cents ($389.97) < 39,098 cents.
	deal16 := deal
	deal16.EarlyPaymentDiscountBP = 160
	deal16.EarlyPaymentDays = earlyDays
	deal16.PaymentDays = earlyDays
	res16, err := Evaluate(deal16, seller)
	if err != nil {
		t.Fatalf("Evaluate at 1.6%% failed: %v", err)
	}
	if res16.ProfitCents != 38997 {
		t.Errorf("profit at 1.6%%: got %d, want 38997 ($389.97)", res16.ProfitCents)
	}
}

func TestEachOfTheEightLeaksAlone(t *testing.T) {
	seller := defaultTestSeller()
	baseDeal := Deal{
		Currency:       "USD",
		Quantity:       40,
		UnitPriceCents: 4000, // List Value = 160,000 cents
	}

	// Reference list profit
	baseRes, err := Evaluate(baseDeal, seller)
	if err != nil {
		t.Fatalf("Evaluate base failed: %v", err)
	}
	if baseRes.ProfitCents != 62367 || baseRes.GiveawayCents != 0 {
		t.Fatalf("base order profit mismatch: got prof=%d give=%d, want 62367 and 0", baseRes.ProfitCents, baseRes.GiveawayCents)
	}

	// Leak 1 alone: Order-size discount 10% (1000 BP)
	t.Run("Leak1_OrderSizeDiscount", func(t *testing.T) {
		d := baseDeal
		d.OrderSizeDiscountBP = 1000
		r, err := Evaluate(d, seller)
		if err != nil {
			t.Fatal(err)
		}
		if r.RevenueCents != 144000 {
			t.Errorf("revenue: got %d, want 144000", r.RevenueCents)
		}
		if r.ProfitCents != 46925 {
			t.Errorf("profit: got %d, want 46925", r.ProfitCents)
		}
		if r.GiveawayCents != 15442 {
			t.Errorf("giveaway: got %d, want 15442", r.GiveawayCents)
		}
		if r.Level != "underline" {
			t.Errorf("level: got %s, want underline", r.Level)
		}
	})

	// Leak 2 alone: Other discount 5% (500 BP)
	t.Run("Leak2_OtherDiscount", func(t *testing.T) {
		d := baseDeal
		d.OtherDiscountBP = 500
		r, err := Evaluate(d, seller)
		if err != nil {
			t.Fatal(err)
		}
		if r.RevenueCents != 152000 {
			t.Errorf("revenue: got %d, want 152000", r.RevenueCents)
		}
		if r.ProfitCents != 54646 {
			t.Errorf("profit: got %d, want 54646", r.ProfitCents)
		}
		if r.GiveawayCents != 7721 {
			t.Errorf("giveaway: got %d, want 7721", r.GiveawayCents)
		}
	})

	// Leak 3 alone: Rebate 8% (800 BP)
	t.Run("Leak3_Rebate", func(t *testing.T) {
		d := baseDeal
		d.RebateBP = 800
		r, err := Evaluate(d, seller)
		if err != nil {
			t.Fatal(err)
		}
		if r.RevenueCents != 147200 {
			t.Errorf("revenue: got %d, want 147200", r.RevenueCents)
		}
		if r.ProfitCents != 50014 {
			t.Errorf("profit: got %d, want 50014", r.ProfitCents)
		}
		if r.GiveawayCents != 12353 {
			t.Errorf("giveaway: got %d, want 12353", r.GiveawayCents)
		}
	})

	// Leak 4 alone: Free shipping
	t.Run("Leak4_FreeShipping", func(t *testing.T) {
		d := baseDeal
		d.FreeShipping = true
		r, err := Evaluate(d, seller)
		if err != nil {
			t.Fatal(err)
		}
		if r.ProfitCents != 55767 {
			t.Errorf("profit: got %d, want 55767", r.ProfitCents)
		}
		if r.GiveawayCents != 6600 {
			t.Errorf("giveaway: got %d, want 6600", r.GiveawayCents)
		}
	})

	// Leak 5 alone: Payment days 30
	t.Run("Leak5_PaymentDays", func(t *testing.T) {
		d := baseDeal
		d.PaymentDays = 30
		r, err := Evaluate(d, seller)
		if err != nil {
			t.Fatal(err)
		}
		if r.ProfitCents != 61052 {
			t.Errorf("profit: got %d, want 61052", r.ProfitCents)
		}
		if r.GiveawayCents != 1315 {
			t.Errorf("giveaway: got %d, want 1315", r.GiveawayCents)
		}
	})

	// Leak 6 alone: Early-payment discount 2% (200 BP), 7 days
	t.Run("Leak6_EarlyPaymentDiscount", func(t *testing.T) {
		d := baseDeal
		d.EarlyPaymentDiscountBP = 200
		d.PaymentDays = 7
		r, err := Evaluate(d, seller)
		if err != nil {
			t.Fatal(err)
		}
		if r.RevenueCents != 156800 {
			t.Errorf("revenue: got %d, want 156800", r.RevenueCents)
		}
		if r.ProfitCents != 58978 {
			t.Errorf("profit: got %d, want 58978", r.ProfitCents)
		}
		if r.GiveawayCents != 3389 {
			t.Errorf("giveaway: got %d, want 3389", r.GiveawayCents)
		}
	})

	// Leak 7 alone: Co-op allowance $50 (5000 cents)
	t.Run("Leak7_CoopAllowance", func(t *testing.T) {
		d := baseDeal
		d.CoopAllowance = Money{Cents: 5000, Currency: "USD"}
		r, err := Evaluate(d, seller)
		if err != nil {
			t.Fatal(err)
		}
		if r.ProfitCents != 57367 {
			t.Errorf("profit: got %d, want 57367", r.ProfitCents)
		}
		if r.GiveawayCents != 5000 {
			t.Errorf("giveaway: got %d, want 5000", r.GiveawayCents)
		}
	})

	// Leak 8 alone: PayPal fee (baseline)
	t.Run("Leak8_PayPalFee", func(t *testing.T) {
		d := baseDeal
		r, err := Evaluate(d, seller)
		if err != nil {
			t.Fatal(err)
		}
		if r.ProfitCents != 62367 {
			t.Errorf("profit: got %d, want 62367", r.ProfitCents)
		}
		if r.GiveawayCents != 0 {
			t.Errorf("giveaway: got %d, want 0", r.GiveawayCents)
		}
	})
}

func TestAllEightLeaksTogether(t *testing.T) {
	seller := defaultTestSeller()
	deal := Deal{
		Currency:               "USD",
		Quantity:               40,
		UnitPriceCents:         4000,
		OrderSizeDiscountBP:    500, // 5%
		OtherDiscountBP:        300, // 3%
		EarlyPaymentDiscountBP: 200, // 2%
		RebateBP:               400, // 4%
		FreeShipping:           true,
		PaymentDays:            7,
		CoopAllowance:          Money{Cents: 2500, Currency: "USD"},
	}

	res, err := Evaluate(deal, seller)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}

	if res.RevenueCents != 138711 {
		t.Errorf("revenue: got %d, want 138711", res.RevenueCents)
	}
	if res.CostsCents != 106256 {
		t.Errorf("costs: got %d, want 106256", res.CostsCents)
	}
	if res.ProfitCents != 32455 {
		t.Errorf("profit: got %d, want 32455", res.ProfitCents)
	}
	if res.GiveawayCents != 29912 {
		t.Errorf("giveaway: got %d, want 29912", res.GiveawayCents)
	}
	if res.Level != "underline" {
		t.Errorf("level: got %s, want underline", res.Level)
	}

	// Exact line add-up check:
	// ListValue - BaseCost - sum(all 8 leaks) == Profit
	var leakSum int64
	for _, l := range res.Leaks {
		if l.Cents < 0 {
			t.Errorf("leak line %s has negative cents: %d", l.Name, l.Cents)
		}
		leakSum += l.Cents
	}

	baseCost := deal.Quantity * seller.UnitCostCents
	reconciledProfit := res.ListValueCents - baseCost - leakSum
	if reconciledProfit != res.ProfitCents {
		t.Errorf("leak lines sum reconciliation failed: %d vs profit %d", reconciledProfit, res.ProfitCents)
	}
}

func TestDoubleDiscounting(t *testing.T) {
	seller := defaultTestSeller()

	// Exam Email 08: $40 -> $44 (+400 cents), 5% off (500 BP), 30 boxes -> $6.00 (600 cents)
	t.Run("ExamEmail08", func(t *testing.T) {
		deal := Deal{
			Currency:          "USD",
			Quantity:          30,
			UnitPriceCents:    4400,
			OldUnitPriceCents: 4000,
			OtherDiscountBP:   500,
		}
		res, err := Evaluate(deal, seller)
		if err != nil {
			t.Fatal(err)
		}
		if res.DoubleDiscountCents != 600 {
			t.Errorf("double discount cents: got %d, want 600 ($6.00)", res.DoubleDiscountCents)
		}
		if !strings.Contains(res.DoubleDiscountFinding, "6.00") && !strings.Contains(res.DoubleDiscountFinding, "600") {
			t.Errorf("finding should mention $6.00, got: %s", res.DoubleDiscountFinding)
		}
	})

	// Book example: The Strategy and Tactics of Pricing p. 112
	// $100 list (10,000 cents), buyer pays $75 (25% = 2500 BP), 6% price increase ($6.00 = 600 cents increase)
	// Finding: $1.50 (150 cents) per unit
	t.Run("BookExample_Pricing_p112", func(t *testing.T) {
		deal := Deal{
			Currency:          "USD",
			Quantity:          1,
			UnitPriceCents:    10600,
			OldUnitPriceCents: 10000,
			OtherDiscountBP:   2500,
		}
		res, err := Evaluate(deal, seller)
		if err != nil {
			t.Fatal(err)
		}
		if res.DoubleDiscountCents != 150 {
			t.Errorf("double discount cents: got %d, want 150 ($1.50)", res.DoubleDiscountCents)
		}
	})
}

func TestRefusals(t *testing.T) {
	seller := defaultTestSeller()

	t.Run("MixedCurrency_DealVsSeller", func(t *testing.T) {
		deal := Deal{
			Currency:       "EUR",
			Quantity:       10,
			UnitPriceCents: 3000,
		}
		_, err := Evaluate(deal, seller)
		if !errors.Is(err, ErrMixedCurrency) {
			t.Errorf("expected ErrMixedCurrency, got: %v", err)
		}
	})

	t.Run("MixedCurrency_CoopVsDeal", func(t *testing.T) {
		deal := Deal{
			Currency:       "USD",
			Quantity:       10,
			UnitPriceCents: 3000,
			CoopAllowance:  Money{Cents: 500, Currency: "GBP"},
		}
		_, err := Evaluate(deal, seller)
		if !errors.Is(err, ErrMixedCurrency) {
			t.Errorf("expected ErrMixedCurrency, got: %v", err)
		}
	})

	t.Run("NoCurrency", func(t *testing.T) {
		deal := Deal{
			Currency:       "",
			Quantity:       10,
			UnitPriceCents: 3000,
		}
		_, err := Evaluate(deal, seller)
		if err == nil {
			t.Error("expected error for empty currency, got nil")
		}
		if !errors.Is(err, ErrNoCurrency) && !errors.Is(err, ErrMixedCurrency) {
			t.Errorf("expected ErrNoCurrency or ErrMixedCurrency, got: %v", err)
		}
	})

	t.Run("WhitespaceCurrency", func(t *testing.T) {
		deal := Deal{
			Currency:       "   ",
			Quantity:       10,
			UnitPriceCents: 3000,
		}
		_, err := Evaluate(deal, seller)
		if err == nil {
			t.Error("expected error for whitespace currency, got nil")
		}
		if !errors.Is(err, ErrNoCurrency) && !errors.Is(err, ErrMixedCurrency) {
			t.Errorf("expected ErrNoCurrency or ErrMixedCurrency, got: %v", err)
		}
	})

	t.Run("NegativeCoopAllowance", func(t *testing.T) {
		deal := Deal{
			Currency:       "USD",
			Quantity:       10,
			UnitPriceCents: 3000,
			CoopAllowance:  Money{Cents: -500, Currency: "USD"},
		}
		_, err := Evaluate(deal, seller)
		if err == nil {
			t.Error("expected error for negative co-op allowance, got nil")
		}
	})

	t.Run("InvalidQuantity", func(t *testing.T) {
		deal := Deal{
			Currency:       "USD",
			Quantity:       0,
			UnitPriceCents: 3000,
		}
		_, err := Evaluate(deal, seller)
		if err == nil {
			t.Error("expected error for zero quantity")
		}
		deal.Quantity = -5
		_, err = Evaluate(deal, seller)
		if err == nil {
			t.Error("expected error for negative quantity")
		}
	})

	t.Run("InvalidUnitPrice", func(t *testing.T) {
		deal := Deal{
			Currency:       "USD",
			Quantity:       10,
			UnitPriceCents: -100,
		}
		_, err := Evaluate(deal, seller)
		if err == nil {
			t.Error("expected error for negative unit price")
		}
	})

	t.Run("MissingSellerValuesNamed", func(t *testing.T) {
		// Missing unit_cost_cents
		badJSON1 := `{"shipping_cost_cents_per_order": 600, "shipping_cost_cents_per_box": 150, "yearly_cost_of_money_percent": 10.0}`
		tmp1 := filepath.Join(t.TempDir(), "seller1.json")
		if err := os.WriteFile(tmp1, []byte(badJSON1), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadSeller(tmp1)
		if err == nil || !strings.Contains(err.Error(), "unit_cost") {
			t.Errorf("expected error naming unit_cost, got %v", err)
		}

		// Missing shipping_cost_cents_per_order
		badJSON2 := `{"unit_cost_cents": 2300, "shipping_cost_cents_per_box": 150, "yearly_cost_of_money_percent": 10.0}`
		tmp2 := filepath.Join(t.TempDir(), "seller2.json")
		if err := os.WriteFile(tmp2, []byte(badJSON2), 0644); err != nil {
			t.Fatal(err)
		}
		_, err = LoadSeller(tmp2)
		if err == nil || !strings.Contains(err.Error(), "shipping") {
			t.Errorf("expected error naming shipping cost, got %v", err)
		}

		// Missing yearly_cost_of_money_percent
		badJSON3 := `{"unit_cost_cents": 2300, "shipping_cost_cents_per_order": 600, "shipping_cost_cents_per_box": 150}`
		tmp3 := filepath.Join(t.TempDir(), "seller3.json")
		if err := os.WriteFile(tmp3, []byte(badJSON3), 0644); err != nil {
			t.Fatal(err)
		}
		_, err = LoadSeller(tmp3)
		if err == nil || !strings.Contains(err.Error(), "yearly_cost_of_money") && !strings.Contains(err.Error(), "cost of money") {
			t.Errorf("expected error naming cost of money, got %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Property Tests (S4)
// -----------------------------------------------------------------------------

func TestPropertyTests(t *testing.T) {
	seller := defaultTestSeller()

	cfg := &quick.Config{
		MaxCount: 500,
		Rand:     rand.New(rand.NewSource(42)),
	}

	// 1. Adding any concession never increases profit
	t.Run("ConcessionNeverIncreasesProfit", func(t *testing.T) {
		f := func(qVal, pVal uint16, discBP, rebateBP, days uint16, freeShip bool, coopCents uint16) bool {
			q := int64(qVal%50) + 1
			p := int64(pVal%5000) + 500
			dBP := int64(discBP % 2500)
			rBP := int64(rebateBP % 1500)
			pDays := int64(days % 90)
			coop := int64(coopCents % 5000)

			base := Deal{
				Currency:       "USD",
				Quantity:       q,
				UnitPriceCents: p,
			}
			baseRes, err := Evaluate(base, seller)
			if err != nil {
				return false
			}

			conceded := base
			conceded.OrderSizeDiscountBP = dBP
			conceded.RebateBP = rBP
			conceded.FreeShipping = freeShip
			conceded.PaymentDays = pDays
			conceded.CoopAllowance = Money{Cents: coop, Currency: "USD"}

			concRes, err := Evaluate(conceded, seller)
			if err != nil {
				return false
			}

			return concRes.ProfitCents <= baseRes.ProfitCents
		}

		if err := quick.Check(f, cfg); err != nil {
			t.Errorf("Concession property failed: %v", err)
		}
	})

	// 2. No leak line is ever negative
	t.Run("NoLeakIsNegative", func(t *testing.T) {
		f := func(qVal, pVal uint16, disc1, disc2, rebate uint16, freeShip bool, days uint16, coop uint16) bool {
			deal := Deal{
				Currency:            "USD",
				Quantity:            int64(qVal%50) + 1,
				UnitPriceCents:      int64(pVal%5000) + 500,
				OrderSizeDiscountBP: int64(disc1 % 2000),
				OtherDiscountBP:     int64(disc2 % 2000),
				RebateBP:            int64(rebate % 1500),
				FreeShipping:        freeShip,
				PaymentDays:         int64(days % 90),
				CoopAllowance:       Money{Cents: int64(coop % 5000), Currency: "USD"},
			}
			res, err := Evaluate(deal, seller)
			if err != nil {
				return false
			}
			for _, l := range res.Leaks {
				if l.Cents < 0 {
					return false
				}
			}
			return true
		}

		if err := quick.Check(f, cfg); err != nil {
			t.Errorf("Negative leak property failed: %v", err)
		}
	})

	// 3. The lines add up exactly to the totals
	t.Run("LinesAddUpExactly", func(t *testing.T) {
		f := func(qVal, pVal uint16, disc1, disc2, earlyDisc, rebate uint16, freeShip bool, days uint16, coop uint16) bool {
			deal := Deal{
				Currency:               "USD",
				Quantity:               int64(qVal%50) + 1,
				UnitPriceCents:         int64(pVal%5000) + 500,
				OrderSizeDiscountBP:    int64(disc1 % 2000),
				OtherDiscountBP:        int64(disc2 % 2000),
				EarlyPaymentDiscountBP: int64(earlyDisc % 1000),
				RebateBP:               int64(rebate % 1500),
				FreeShipping:           freeShip,
				PaymentDays:            int64(days % 90),
				CoopAllowance:          Money{Cents: int64(coop % 5000), Currency: "USD"},
			}
			res, err := Evaluate(deal, seller)
			if err != nil {
				return false
			}

			// Revenue minus costs must equal profit exactly
			if res.RevenueCents-res.CostsCents != res.ProfitCents {
				return false
			}

			// List Value minus base cost minus all leaks must equal profit exactly
			var sumLeaks int64
			for _, l := range res.Leaks {
				sumLeaks += l.Cents
			}
			baseCost := deal.Quantity * seller.UnitCostCents
			if res.ListValueCents-baseCost-sumLeaks != res.ProfitCents {
				return false
			}

			return true
		}

		if err := quick.Check(f, cfg); err != nil {
			t.Errorf("Line add-up property failed: %v", err)
		}
	})

	// 4. Order in which terms are listed doesn't change any result
	t.Run("OrderInvariant", func(t *testing.T) {
		f := func(qVal, pVal uint16, discBP, rebateBP uint16, freeShip bool, days uint16) bool {
			dealA := Deal{
				Currency:            "USD",
				Quantity:            int64(qVal%50) + 1,
				UnitPriceCents:      int64(pVal%5000) + 500,
				OrderSizeDiscountBP: int64(discBP % 2500),
				RebateBP:            int64(rebateBP % 1500),
				FreeShipping:        freeShip,
				PaymentDays:         int64(days % 90),
			}
			dealB := Deal{
				PaymentDays:         dealA.PaymentDays,
				FreeShipping:        dealA.FreeShipping,
				RebateBP:            dealA.RebateBP,
				OrderSizeDiscountBP: dealA.OrderSizeDiscountBP,
				UnitPriceCents:      dealA.UnitPriceCents,
				Quantity:            dealA.Quantity,
				Currency:            dealA.Currency,
			}

			resA, errA := Evaluate(dealA, seller)
			resB, errB := Evaluate(dealB, seller)
			if errA != nil || errB != nil {
				return false
			}

			return reflect.DeepEqual(resA, resB)
		}

		if err := quick.Check(f, cfg); err != nil {
			t.Errorf("Order invariant property failed: %v", err)
		}
	})
}
