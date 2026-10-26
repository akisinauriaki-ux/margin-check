package check

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"margincheck/internal/normalize"
	"margincheck/internal/profit"
	"margincheck/internal/quote"
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

func extractOldUnitPriceCents(res reader.CheckResult, emailText string) int64 {
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

	// The quotes may name only the new price. Then look in the email itself, by code, for a
	// "from $X to $Y" sentence whose new price Y is exactly the agreed unit price.
	for _, m := range priceIncreaseRegex.FindAllStringSubmatch(quote.NormalizeText(emailText), -1) {
		oldCents, errOld := parsePriceCents(m[1])
		newCents, errNew := parsePriceCents(m[2])
		if errOld == nil && errNew == nil && newCents == res.UnitPriceCents.Value && oldCents > 0 && oldCents < newCents {
			return oldCents
		}
	}
	return 0
}

// DealCurrency returns the deal's one currency, taken from its own money, or false when it has none or several.
func DealCurrency(res reader.CheckResult) (string, bool) {
	if c := dealMoneyCurrencies(res); len(c) == 1 {
		return c[0], true
	}
	return "", false
}

// dealMoneyCurrencies returns the sorted, distinct currencies of the deal's own money: offered or agreed
// money terms, plus the currency written beside the unit price in its quote. Requested terms are left out.
func dealMoneyCurrencies(res reader.CheckResult) []string {
	set := make(map[string]bool)
	for _, term := range res.Terms {
		status := strings.ToLower(strings.TrimSpace(term.Status))
		if status != "agreed" && status != "offered" {
			continue
		}
		if c := strings.ToUpper(strings.TrimSpace(term.Currency)); c != "" {
			set[c] = true
		}
	}
	for _, m := range normalize.ExtractMoney(res.UnitPriceCents.Quote) {
		if m.Int == res.UnitPriceCents.Value && m.Currency != "" {
			set[strings.ToUpper(m.Currency)] = true
		}
	}
	list := make([]string, 0, len(set))
	for c := range set {
		list = append(list, c)
	}
	sort.Strings(list)
	return list
}

// Check coordinates turning a reading into a profit.Deal and running profit.Evaluate.
//
// Hard rule: Only sure, agreed values reach the profit engine. Any "ask" stops
// the check before any money figure is shown, and lists the questions (M6 for the
// seller's numbers, M4 for the deal's numbers).
//
// A mixed-currency refusal becomes the outcome OutcomeRefused with the reason.
func Check(res reader.CheckResult, seller profit.Seller) Result {
	return CheckEmail(res, seller, "")
}

// CheckEmail is Check with the email text available, so that code (never the AI) can find the old price of a
// discounted price increase when the reader's quotes name only the new price.
func CheckEmail(res reader.CheckResult, seller profit.Seller, emailText string) Result {
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
	// The deal's currency comes from its own money: offered or agreed money terms, and the currency written
	// in the unit price's quote. A buyer's request in another currency is not part of the deal. Never assume
	// the seller's currency (step-3 spec, item 0).
	dealCurrencies := dealMoneyCurrencies(res)
	if len(dealCurrencies) > 1 {
		return Result{
			EmailID:       res.EmailID,
			Outcome:       OutcomeRefused,
			Action:        action,
			RefusalReason: fmt.Sprintf("%v: multiple currencies in the deal (%s)", profit.ErrMixedCurrency, strings.Join(dealCurrencies, ", ")),
		}
	}
	if len(dealCurrencies) == 0 {
		return Result{
			EmailID:   res.EmailID,
			Outcome:   OutcomeAsk,
			Action:    action,
			Questions: []string{"currency"},
		}
	}
	dealCurrency := dealCurrencies[0]

	// 4. Extract old unit price if price increase discounted applies. If it can't be found, ask: the
	// double-discount warning must never silently disappear.
	var oldUnitPrice int64
	if res.PriceIncreaseDiscounted.Value {
		oldUnitPrice = extractOldUnitPriceCents(res, emailText)
		if oldUnitPrice <= 0 {
			return Result{
				EmailID:   res.EmailID,
				Outcome:   OutcomeAsk,
				Action:    action,
				Questions: []string{"old_unit_price"},
			}
		}
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
