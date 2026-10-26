package check_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"margincheck/internal/check"
	"margincheck/internal/profit"
	"margincheck/internal/reader"
)

func sampleSeller() profit.Seller {
	return profit.Seller{
		UnitCostCents:             2300,
		ShippingCostCentsPerOrder: 600,
		ShippingCostCentsPerBox:   150,
		PayPalFeePercentBP:        349,
		PayPalFixedFeeCents:       49,
		YearlyCostOfMoneyBP:       1000, // 10.0%
		UnderlineMinGiveawayBP:    200,  // 2.0%
		Currency:                  "USD",
	}
}

func baseSureResult() reader.CheckResult {
	return reader.CheckResult{
		EmailID: "test-01",
		Quantity: reader.IntFieldResult{
			Value:   40,
			Sure:    true,
			Unknown: false,
			Quote:   "We want 40 boxes.",
		},
		UnitPriceCents: reader.IntFieldResult{
			Value:   4000,
			Sure:    true,
			Unknown: false,
			Quote:   "$40 per box.",
		},
		DiscountPercent: reader.IntFieldResult{
			Value:   0,
			Sure:    true,
			Unknown: false,
		},
		FreeShipping: reader.BoolFieldResult{
			Value:   false,
			Sure:    true,
			Unknown: false,
		},
		PaymentDays: reader.IntFieldResult{
			Value:   0,
			Sure:    true,
			Unknown: false,
		},
		RebatePercent: reader.IntFieldResult{
			Value:   0,
			Sure:    true,
			Unknown: false,
		},
		PriceIncreaseDiscounted: reader.BoolFieldResult{
			Value:   false,
			Sure:    true,
			Unknown: false,
		},
		Terms: []reader.TermResult{
			{Name: "quantity", Value: 40, By: "buyer", Status: "agreed"},
			{Name: "unit_price_cents", Value: 4000, Currency: "USD", By: "seller", Status: "agreed"},
		},
	}
}

func TestCheckEvaluatedSureValues(t *testing.T) {
	seller := sampleSeller()
	reading := baseSureResult()

	res := check.Check(reading, seller)
	if res.Outcome != check.OutcomeEvaluated {
		t.Fatalf("expected outcome %s, got %s (questions: %v, refusal: %s)",
			check.OutcomeEvaluated, res.Outcome, res.Questions, res.RefusalReason)
	}
	if res.Profit == nil {
		t.Fatal("expected profit result to be non-nil on evaluated deal")
	}
	if res.Deal == nil {
		t.Fatal("expected deal to be non-nil on evaluated deal")
	}
	if res.Profit.ProfitCents <= 0 {
		t.Errorf("expected positive profit, got %d", res.Profit.ProfitCents)
	}
	if res.Profit.Level != "silent" {
		t.Errorf("expected level silent, got %s", res.Profit.Level)
	}
	if len(res.Questions) != 0 {
		t.Errorf("expected no questions on evaluated deal, got %v", res.Questions)
	}
}

func TestCheckStopsOnUnsureDealFields(t *testing.T) {
	seller := sampleSeller()

	cases := []struct {
		name     string
		modify   func(*reader.CheckResult)
		wantQ    string
	}{
		{
			name: "unsure quantity",
			modify: func(r *reader.CheckResult) {
				r.Quantity.Sure = false
			},
			wantQ: "quantity",
		},
		{
			name: "unknown quantity",
			modify: func(r *reader.CheckResult) {
				r.Quantity.Unknown = true
			},
			wantQ: "quantity",
		},
		{
			name: "unsure unit price",
			modify: func(r *reader.CheckResult) {
				r.UnitPriceCents.Sure = false
			},
			wantQ: "unit_price_cents",
		},
		{
			name: "unsure discount percent",
			modify: func(r *reader.CheckResult) {
				r.DiscountPercent.Sure = false
			},
			wantQ: "discount_percent",
		},
		{
			name: "unsure free shipping",
			modify: func(r *reader.CheckResult) {
				r.FreeShipping.Sure = false
			},
			wantQ: "free_shipping",
		},
		{
			name: "unsure payment days",
			modify: func(r *reader.CheckResult) {
				r.PaymentDays.Sure = false
			},
			wantQ: "payment_days",
		},
		{
			name: "unsure rebate percent",
			modify: func(r *reader.CheckResult) {
				r.RebatePercent.Sure = false
			},
			wantQ: "rebate_percent",
		},
		{
			name: "unsure price increase discounted",
			modify: func(r *reader.CheckResult) {
				r.PriceIncreaseDiscounted.Sure = false
			},
			wantQ: "price_increase_discounted",
		},
		{
			name: "explicit must_ask item",
			modify: func(r *reader.CheckResult) {
				r.MustAsk = []string{"free_shipping"}
			},
			wantQ: "free_shipping",
		},
		{
			name: "budget exceeded",
			modify: func(r *reader.CheckResult) {
				r.BudgetExceeded = true
			},
			wantQ: "budget",
		},
		{
			name: "model unavailable",
			modify: func(r *reader.CheckResult) {
				r.ModelUnavailable = true
				r.UnavailableReason = "503 service unavailable"
			},
			wantQ: "model_unavailable",
		},
		{
			name: "daily limit reached",
			modify: func(r *reader.CheckResult) {
				r.DailyLimitHit = true
				r.UnavailableReason = "daily quota exhausted"
			},
			wantQ: "daily_limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reading := baseSureResult()
			tc.modify(&reading)

			res := check.Check(reading, seller)
			if res.Outcome != check.OutcomeAsk {
				t.Fatalf("expected outcome %s, got %s", check.OutcomeAsk, res.Outcome)
			}
			// Hard rule: No money figures may exist if check stopped on ask
			if res.Profit != nil {
				t.Fatalf("expected Profit to be nil when check stops on ask, got %+v", res.Profit)
			}
			if len(res.Questions) == 0 {
				t.Fatal("expected questions list to be non-empty")
			}
			found := false
			for _, q := range res.Questions {
				if strings.Contains(q, tc.wantQ) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected question containing %q in %v", tc.wantQ, res.Questions)
			}
		})
	}
}

func TestCheckStopsOnMissingSellerConfiguration(t *testing.T) {
	reading := baseSureResult()

	t.Run("missing unit cost", func(t *testing.T) {
		seller := sampleSeller()
		seller.UnitCostCents = 0

		res := check.Check(reading, seller)
		if res.Outcome != check.OutcomeAsk {
			t.Fatalf("expected outcome %s, got %s", check.OutcomeAsk, res.Outcome)
		}
		if res.Profit != nil {
			t.Fatal("expected Profit to be nil when seller configuration is incomplete")
		}
		found := false
		for _, q := range res.Questions {
			if strings.Contains(q, "unit_cost_cents") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected question about unit_cost_cents, got %v", res.Questions)
		}
	})

	t.Run("missing cost of money", func(t *testing.T) {
		seller := sampleSeller()
		seller.YearlyCostOfMoneyBP = 0

		res := check.Check(reading, seller)
		if res.Outcome != check.OutcomeAsk {
			t.Fatalf("expected outcome %s, got %s", check.OutcomeAsk, res.Outcome)
		}
		if res.Profit != nil {
			t.Fatal("expected Profit to be nil when seller configuration is incomplete")
		}
		found := false
		for _, q := range res.Questions {
			if strings.Contains(q, "yearly_cost_of_money") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected question about yearly_cost_of_money, got %v", res.Questions)
		}
	})
}

func TestCheckRefusedMixedCurrency(t *testing.T) {
	seller := sampleSeller() // USD

	t.Run("terms specify different currency from seller", func(t *testing.T) {
		reading := baseSureResult()
		reading.Terms = []reader.TermResult{
			{Name: "unit_price_cents", Value: 4000, Currency: "EUR", By: "seller", Status: "agreed"},
		}

		res := check.Check(reading, seller)
		if res.Outcome != check.OutcomeRefused {
			t.Fatalf("expected outcome %s, got %s", check.OutcomeRefused, res.Outcome)
		}
		if res.Profit != nil {
			t.Fatal("expected Profit to be nil on refused check")
		}
		if !strings.Contains(strings.ToLower(res.RefusalReason), "currency") {
			t.Errorf("expected refusal reason to mention currency, got %s", res.RefusalReason)
		}
	})

	t.Run("terms contain multiple conflicting currencies", func(t *testing.T) {
		reading := baseSureResult()
		reading.Terms = []reader.TermResult{
			{Name: "unit_price_cents", Value: 4000, Currency: "USD", By: "seller", Status: "agreed"},
			{Name: "shipping_cents", Value: 2400, Currency: "EUR", By: "seller", Status: "agreed"},
		}

		res := check.Check(reading, seller)
		if res.Outcome != check.OutcomeRefused {
			t.Fatalf("expected outcome %s, got %s", check.OutcomeRefused, res.Outcome)
		}
		if res.Profit != nil {
			t.Fatal("expected Profit to be nil on refused check")
		}
		if !strings.Contains(strings.ToLower(res.RefusalReason), "currency") {
			t.Errorf("expected refusal reason to mention currency, got %s", res.RefusalReason)
		}
	})
}

func TestCheckDoubleDiscountFinding(t *testing.T) {
	seller := sampleSeller()
	reading := baseSureResult()
	reading.EmailID = "08"
	reading.Quantity.Value = 30
	reading.UnitPriceCents.Value = 4400
	reading.DiscountPercent.Value = 5
	reading.PriceIncreaseDiscounted.Value = true
	reading.UnitPriceCents.Quote = "Due to higher packaging material costs, our baseline price per box increases from $40 to $44 starting this week."
	reading.PriceIncreaseDiscounted.Quote = "Due to higher packaging material costs, our baseline price per box increases from $40 to $44 starting this week."

	res := check.Check(reading, seller)
	if res.Outcome != check.OutcomeEvaluated {
		t.Fatalf("expected outcome %s, got %s (refusal: %s, questions: %v)",
			check.OutcomeEvaluated, res.Outcome, res.RefusalReason, res.Questions)
	}
	if res.Profit == nil {
		t.Fatal("expected profit to be non-nil")
	}
	// For exam email 08 ($40 -> $44, 5% off, 30 boxes), the finding is $6.00 (600 cents)
	if res.Profit.DoubleDiscountCents != 600 {
		t.Errorf("expected DoubleDiscountCents 600 ($6.00), got %d", res.Profit.DoubleDiscountCents)
	}
	if !strings.Contains(res.Profit.DoubleDiscountFinding, "double discount") {
		t.Errorf("expected double discount finding text, got %q", res.Profit.DoubleDiscountFinding)
	}
}

func TestBuyerReplyActions(t *testing.T) {
	cases := []struct {
		opt        string
		wantAction check.Action
	}{
		{"standard", check.ActionDraft},
		{"quantity", check.ActionDraft},
		{"early_payment", check.ActionDraft},
		{"UNCLEAR", check.ActionAsk},
		{"changed", check.ActionSellerDecides},
		{"", check.ActionNone},
	}

	for _, tc := range cases {
		t.Run("opt_"+tc.opt, func(t *testing.T) {
			act := check.ActionForAcceptedOption(tc.opt)
			if act != tc.wantAction {
				t.Errorf("ActionForAcceptedOption(%q) = %q, want %q", tc.opt, act, tc.wantAction)
			}
		})
	}
}

type examAnswerKeyItem struct {
	EmailID               string `json:"email_id"`
	ExpectedLevel         string `json:"expected_level"`
	ExpectedProfitCents   int64  `json:"expected_profit_cents"`
	ExpectedGiveawayCents int64  `json:"expected_giveaway_cents"`
	ExpectedAction        string `json:"expected_action"`
	MustAsk               []string `json:"must_ask"`
}

type examReportFile struct {
	EmailScores []struct {
		EmailID string `json:"email_id"`
	} `json:"email_scores"`
}

func TestAll20ExamEmailsIntegration(t *testing.T) {
	answersPath := filepath.Join("..", "..", "testdata", "answers.json")
	ansBytes, err := os.ReadFile(answersPath)
	if err != nil {
		t.Fatalf("read answers.json: %v", err)
	}
	var answers []examAnswerKeyItem
	if err := json.Unmarshal(ansBytes, &answers); err != nil {
		t.Fatalf("parse answers.json: %v", err)
	}

	sellerPath := filepath.Join("..", "..", "testdata", "seller.json")
	seller, err := profit.LoadSeller(sellerPath)
	if err != nil {
		t.Fatalf("LoadSeller(%s): %v", sellerPath, err)
	}

	// Locate cached pair traces in reports/traces/
	tracesDir := filepath.Join("..", "..", "reports", "traces")
	if _, err := os.Stat(tracesDir); err != nil {
		t.Skipf("traces dir %s not available: %v", tracesDir, err)
	}

	traces, err := os.ReadDir(tracesDir)
	if err != nil {
		t.Fatalf("read traces dir: %v", err)
	}

	traceByEmail := make(map[string]reader.CheckResult)
	for _, tr := range traces {
		name := tr.Name()
		if !strings.HasPrefix(name, "trace-") || !strings.Contains(name, "gemini-3.5-flash-lite_gemini-3.1-flash-lite") {
			continue
		}
		parts := strings.Split(name, "-")
		if len(parts) < 2 {
			continue
		}
		emailID := parts[1]
		if len(emailID) != 2 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(tracesDir, name))
		if err != nil {
			continue
		}
		var tf reader.TraceFile
		if err := json.Unmarshal(data, &tf); err == nil && tf.FinalResult.Quantity.Sure != false || tf.FinalResult.UnitPriceCents.Sure != false {
			traceByEmail[emailID] = tf.FinalResult
		}
	}

	if len(traceByEmail) < 20 {
		t.Logf("found %d pair traces in %s; will evaluate those present", len(traceByEmail), tracesDir)
	}

	falseAlarmsHealthyDeals := 0
	actionsScored := 0
	actionsMatched := 0

	for _, a := range answers {
		t.Run("Email_"+a.EmailID, func(t *testing.T) {
			res, ok := traceByEmail[a.EmailID]
			if !ok {
				t.Skipf("no trace found for email %s", a.EmailID)
				return
			}

			chk := check.Check(res, seller)

			// If answer key has must_ask items (04, 09, 14, 19):
			if len(a.MustAsk) > 0 {
				if chk.Outcome != check.OutcomeAsk {
					t.Errorf("email %s: expected outcome ask, got %s", a.EmailID, chk.Outcome)
				}
				if chk.Profit != nil {
					t.Errorf("email %s: profit must be nil when check stops on ask, got %+v", a.EmailID, chk.Profit)
				}
				if len(chk.Questions) == 0 {
					t.Errorf("email %s: expected questions to be non-empty", a.EmailID)
				}
			} else {
				// 16 emails where deal is closed and sure
				if chk.Outcome != check.OutcomeEvaluated {
					t.Errorf("email %s: expected outcome evaluated, got %s (questions: %v, refusal: %s)",
						a.EmailID, chk.Outcome, chk.Questions, chk.RefusalReason)
				}
				if chk.Profit == nil {
					t.Fatalf("email %s: expected profit to be non-nil", a.EmailID)
				}

				diffProf := chk.Profit.ProfitCents - a.ExpectedProfitCents
				if diffProf < -1 || diffProf > 1 {
					t.Errorf("email %s profit mismatch: got %d, expected %d (diff %d cents)",
						a.EmailID, chk.Profit.ProfitCents, a.ExpectedProfitCents, diffProf)
				}

				diffGive := chk.Profit.GiveawayCents - a.ExpectedGiveawayCents
				if diffGive < -1 || diffGive > 1 {
					t.Errorf("email %s giveaway mismatch: got %d, expected %d (diff %d cents)",
						a.EmailID, chk.Profit.GiveawayCents, a.ExpectedGiveawayCents, diffGive)
				}

				if chk.Profit.Level != a.ExpectedLevel {
					t.Errorf("email %s level mismatch: got %s, expected %s",
						a.EmailID, chk.Profit.Level, a.ExpectedLevel)
				}
			}

			// False alarm check: key says silent, but tool spoke (underline or panel)
			if a.ExpectedLevel == "silent" {
				if chk.Outcome == check.OutcomeEvaluated && chk.Profit != nil && chk.Profit.Level != "silent" {
					falseAlarmsHealthyDeals++
					t.Errorf("false alarm on healthy email %s: expected silent, tool spoke %s",
						a.EmailID, chk.Profit.Level)
				}
			}

			// Action accuracy check
			if a.ExpectedAction != "" {
				actionsScored++
				if string(chk.Action) == a.ExpectedAction {
					actionsMatched++
				} else {
					t.Errorf("email %s action mismatch: got %s, expected %s",
						a.EmailID, chk.Action, a.ExpectedAction)
				}
			}
		})
	}

	if falseAlarmsHealthyDeals > 0 {
		t.Errorf("expected 0 false alarms on healthy deals, got %d", falseAlarmsHealthyDeals)
	}
	if actionsScored > 0 && actionsMatched != actionsScored {
		t.Errorf("expected %d/%d actions matched, got %d/%d", actionsScored, actionsScored, actionsMatched, actionsScored)
	}
}
