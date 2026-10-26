package check

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"margincheck/internal/profit"
	"margincheck/internal/reader"
)

// Outcome represents the high-level evaluation status of a check.
type Outcome string

const (
	// OutcomeEvaluated indicates that only sure, agreed values were present and the profit engine ran.
	OutcomeEvaluated Outcome = "evaluated"
	// OutcomeAsk indicates that the check stopped before reaching the profit engine due to missing or unsure values.
	OutcomeAsk Outcome = "ask"
	// OutcomeRefused indicates that the check was refused by policy (e.g. mixed currency or negative values).
	OutcomeRefused Outcome = "refused"
)

// Action represents the recommended operational next step, especially for buyer reply threads.
type Action string

const (
	ActionDraft         Action = "draft"          // Standard, quantity, or early_payment option chosen
	ActionAsk           Action = "ask"            // Buyer reply UNCLEAR
	ActionSellerDecides Action = "seller_decides" // Buyer reply changed terms
	ActionNone          Action = ""               // Not a buyer reply thread
)

// Result is the financial and operational assessment of an email check.
// If Outcome != OutcomeEvaluated, Profit is nil and no money figures are computed or shown.
type Result struct {
	EmailID       string         `json:"email_id,omitempty"`
	Outcome       Outcome        `json:"outcome"`
	Action        Action         `json:"action,omitempty"`
	Questions     []string       `json:"questions,omitempty"`
	RefusalReason string         `json:"refusal_reason,omitempty"`
	Deal          *profit.Deal   `json:"deal,omitempty"`
	Profit        *profit.Result `json:"profit,omitempty"`
}

// ActionForAcceptedOption maps an accepted_option string into an Action:
// standard, quantity, early_payment -> draft
// UNCLEAR -> ask
// changed -> seller_decides
func ActionForAcceptedOption(opt string) Action {
	switch strings.ToLower(strings.TrimSpace(opt)) {
	case "standard", "quantity", "early_payment":
		return ActionDraft
	case "unclear":
		return ActionAsk
	case "changed":
		return ActionSellerDecides
	default:
		return ActionNone
	}
}

// ActionForReading determines the buyer-reply action from reader output.
func ActionForReading(res reader.CheckResult) Action {
	if res.AcceptedOption.Value != "" {
		return ActionForAcceptedOption(res.AcceptedOption.Value)
	}
	for _, q := range res.MustAsk {
		if strings.EqualFold(q, "accepted_option") {
			return ActionAsk
		}
	}
	return ActionNone
}

// ValidateSeller checks seller baseline configuration (M6).
// Missing unit cost, shipping cost, or cost of money are returned as questions.
func ValidateSeller(seller profit.Seller) []string {
	var questions []string
	if seller.UnitCostCents <= 0 {
		questions = append(questions, "seller: missing unit_cost_cents")
	}
	if seller.ShippingCostCentsPerOrder < 0 || seller.ShippingCostCentsPerBox < 0 {
		questions = append(questions, "seller: missing shipping_cost_cents")
	}
	if seller.YearlyCostOfMoneyBP <= 0 {
		questions = append(questions, "seller: missing yearly_cost_of_money (cost of money)")
	}
	return questions
}

// CollectDealQuestions examines a reader.CheckResult and collects any questions
// that require seller input (M4) or system limits (OWASP LLM10).
func CollectDealQuestions(res reader.CheckResult) []string {
	var questions []string
	seen := make(map[string]bool)

	addQ := func(q string) {
		q = strings.TrimSpace(q)
		if q != "" && !seen[q] {
			seen[q] = true
			questions = append(questions, q)
		}
	}

	// 1. System and budget limits (OWASP LLM10)
	if res.BudgetExceeded {
		addQ("budget: check exceeded limits")
	}
	if res.ModelUnavailable {
		reason := res.UnavailableReason
		if reason == "" {
			reason = "model unavailable"
		}
		addQ("model_unavailable: " + reason)
	}
	if res.DailyLimitHit {
		reason := res.UnavailableReason
		if reason == "" {
			reason = "daily limit reached"
		}
		addQ("daily_limit: " + reason)
	}

	// 2. Explicit must_ask items identified during reading
	for _, ma := range res.MustAsk {
		addQ(ma)
	}

	// 3. Deal fields: must be sure and known
	if !res.Quantity.Sure || res.Quantity.Unknown {
		addQ("quantity")
	} else if res.Quantity.Value <= 0 {
		addQ("quantity: must be positive")
	}

	if !res.UnitPriceCents.Sure || res.UnitPriceCents.Unknown {
		addQ("unit_price_cents")
	} else if res.UnitPriceCents.Value < 0 {
		addQ("unit_price_cents: cannot be negative")
	}

	if !res.DiscountPercent.Sure || res.DiscountPercent.Unknown {
		addQ("discount_percent")
	}

	if !res.FreeShipping.Sure || res.FreeShipping.Unknown {
		addQ("free_shipping")
	}

	if !res.PaymentDays.Sure || res.PaymentDays.Unknown {
		addQ("payment_days")
	}

	if !res.RebatePercent.Sure || res.RebatePercent.Unknown {
		addQ("rebate_percent")
	}

	if !res.PriceIncreaseDiscounted.Sure || res.PriceIncreaseDiscounted.Unknown {
		addQ("price_increase_discounted")
	}

	// 4. Accepted option unclear check
	if strings.EqualFold(res.AcceptedOption.Value, "UNCLEAR") {
		addQ("accepted_option")
	}

	return questions
}

var priceIncreaseRegex = regexp.MustCompile(`(?i)(?:increase[ds]?|rose|from)\s+\$?([0-9]+(?:\.[0-9]{2})?)\s+to\s+\$?([0-9]+(?:\.[0-9]{2})?)`)

func parsePriceCents(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "$"))
	parts := strings.Split(s, ".")
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}
	cents := whole * 100
	if len(parts) > 1 {
		dec := parts[1]
		if len(dec) == 1 {
			d, err := strconv.ParseInt(dec, 10, 64)
			if err != nil {
				return 0, err
			}
			cents += d * 10
		} else if len(dec) >= 2 {
			d, err := strconv.ParseInt(dec[:2], 10, 64)
			if err != nil {
				return 0, err
			}
			cents += d
		}
	}
	return cents, nil
}

func extractOldUnitPriceCents(res reader.CheckResult) int64 {
	candidates := []string{
		res.PriceIncreaseDiscounted.Quote,
		res.UnitPriceCents.Quote,
	}
	for _, term := range res.Terms {
		if strings.EqualFold(term.Name, "unit_price_cents") && term.SourceSentence != "" {
			candidates = append(candidates, term.SourceSentence)
		}
	}

	for _, text := range candidates {
		m := priceIncreaseRegex.FindStringSubmatch(text)
		if len(m) >= 3 {
			oldCents, err := parsePriceCents(m[1])
			if err == nil && oldCents > 0 {
				return oldCents
			}
		}
	}
	return 0
}

// Check coordinates turning a reading into a profit.Deal and running profit.Evaluate.
//
// Hard rule: Only sure, agreed values reach the profit engine. Any "ask" stops
// the check before any money figure is shown, and lists the questions (M6 for the
// seller's numbers, M4 for the deal's numbers).
//
// A mixed-currency refusal becomes the outcome OutcomeRefused with the reason.
func Check(res reader.CheckResult, seller profit.Seller) Result {
	action := ActionForReading(res)

	// 1. Validate seller configuration (M6)
	sellerQuestions := ValidateSeller(seller)

	// 2. Collect deal questions (M4 and limits)
	dealQuestions := CollectDealQuestions(res)

	allQuestions := append(sellerQuestions, dealQuestions...)
	if len(allQuestions) > 0 {
		if action == ActionNone {
			for _, q := range allQuestions {
				if strings.Contains(q, "accepted_option") {
					action = ActionAsk
					break
				}
			}
		}
		// STOP before profit engine; no money figures computed.
		return Result{
			EmailID:   res.EmailID,
			Outcome:   OutcomeAsk,
			Action:    action,
			Questions: allQuestions,
		}
	}

	// 3. At this point, all values are SURE and AGREED.
	// Check terms for currency consistency.
	termCurrencies := make(map[string]bool)
	for _, term := range res.Terms {
		c := strings.ToUpper(strings.TrimSpace(term.Currency))
		if c != "" {
			termCurrencies[c] = true
		}
	}

	if len(termCurrencies) > 1 {
		var list []string
		for c := range termCurrencies {
			list = append(list, c)
		}
		return Result{
			EmailID:       res.EmailID,
			Outcome:       OutcomeRefused,
			Action:        action,
			RefusalReason: fmt.Sprintf("%v: multiple currencies in terms (%s)", profit.ErrMixedCurrency, strings.Join(list, ", ")),
		}
	}

	dealCurrency := seller.Currency
	for c := range termCurrencies {
		dealCurrency = c
	}

	// 4. Extract old unit price if price increase discounted applies
	var oldUnitPrice int64
	if res.PriceIncreaseDiscounted.Value {
		oldUnitPrice = extractOldUnitPriceCents(res)
	}

	// 5. Construct profit.Deal
	deal := profit.Deal{
		Currency:            dealCurrency,
		Quantity:            res.Quantity.Value,
		UnitPriceCents:      res.UnitPriceCents.Value,
		OrderSizeDiscountBP: res.DiscountPercent.Value * 100,
		RebateBP:            res.RebatePercent.Value * 100,
		FreeShipping:        res.FreeShipping.Value,
		PaymentDays:         res.PaymentDays.Value,
		OldUnitPriceCents:   oldUnitPrice,
	}

	// 6. Run profit.Evaluate
	profitRes, err := profit.Evaluate(deal, seller)
	if err != nil {
		return Result{
			EmailID:       res.EmailID,
			Outcome:       OutcomeRefused,
			Action:        action,
			RefusalReason: err.Error(),
			Deal:          &deal,
		}
	}

	return Result{
		EmailID: res.EmailID,
		Outcome: OutcomeEvaluated,
		Action:  action,
		Deal:    &deal,
		Profit:  &profitRes,
	}
}
