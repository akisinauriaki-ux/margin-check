package offer

import (
	"errors"
	"fmt"
	"strings"

	"margincheck/internal/profit"
)

// Kind names one of the three choices.
type Kind string

const (
	Standard     Kind = "standard"
	Quantity     Kind = "quantity"
	EarlyPayment Kind = "early_payment"
)

// Request is what the buyer asked for, from sure values of the checked deal.
type Request struct {
	Currency             string
	Quantity             int64
	UnitPriceCents       int64
	RequestedPaymentDays int64 // the payment terms the choices answer: the buyer's ask, or the terms already settled
	RequestedDiscountBP  int64 // the discount the buyer asked for; recorded, not used to build the choices
	PaymentDaysSettled   bool  // the thread already shows the payment terms on the table (not only a request)
}

// Config holds the few fixed rules of the offer builder. Zero values take the defaults.
type Config struct {
	QuantityStep int64  // the larger order is rounded up to this step; default 10
	EarlyDays    int64  // the early-payment window; default 7 (decision 0020)
	UnitNoun     string // default "boxes"
}

func (c Config) withDefaults() Config {
	if c.QuantityStep <= 0 {
		c.QuantityStep = 10
	}
	if c.EarlyDays <= 0 {
		c.EarlyDays = 7
	}
	if strings.TrimSpace(c.UnitNoun) == "" {
		c.UnitNoun = "boxes"
	}
	return c
}

// Option is one choice put to the buyer. Every money figure in it comes from profit.Evaluate.
type Option struct {
	Kind                Kind          `json:"kind"`
	Currency            string        `json:"currency"`
	Quantity            int64         `json:"quantity"`
	UnitPriceCents      int64         `json:"unit_price_cents"`
	FreeShipping        bool          `json:"free_shipping"`
	ShippingChargeCents int64         `json:"shipping_charge_cents"` // charged to the buyer at the seller's cost; 0 when free
	DiscountBP          int64         `json:"discount_bp"`           // the early-payment discount (plan B: an item discount)
	DueDays             int64         `json:"due_days"`              // 0 = on receipt
	Profit              profit.Result `json:"profit"`
	BaselineProfitCents int64         `json:"baseline_profit_cents"` // what this choice is measured against
	Reason              string        `json:"reason"`                // the fair, outside reason; never a number
	Question            string        `json:"question"`              // a "how" or "what" question for the buyer
	Summary             string        `json:"summary"`               // the terms in one line, with numbers, written by code
}

// Set is the result of Build: the choices, and the choices left out with their reason.
type Set struct {
	Options []Option `json:"options"`
	Omitted []string `json:"omitted,omitempty"`
}

// Build makes up to three choices: standard, quantity (free shipping earned by a larger order) and early
// payment (plan B, decision 0020). It never makes a choice halfway between the buyer's ask and the seller's
// price: each choice rests on a cost-based reason, and each number comes from the profit engine.
func Build(req Request, seller profit.Seller, cfg Config) (Set, error) {
	cfg = cfg.withDefaults()
	cur := strings.ToUpper(strings.TrimSpace(req.Currency))
	switch {
	case cur == "":
		return Set{}, errors.New("offer: the deal has no currency")
	case cur != strings.ToUpper(seller.Currency):
		return Set{}, fmt.Errorf("offer: the deal is in %s but the seller's numbers are in %s", cur, seller.Currency)
	case req.Quantity <= 0:
		return Set{}, errors.New("offer: the deal has no quantity")
	case req.UnitPriceCents <= 0:
		return Set{}, errors.New("offer: the deal has no unit price")
	}

	base := profit.Deal{Currency: cur, Quantity: req.Quantity, UnitPriceCents: req.UnitPriceCents}
	shipping := func(q int64) int64 { return seller.ShippingCostCentsPerOrder + q*seller.ShippingCostCentsPerBox }

	var set Set

	// 1. Standard: list price, shipping at cost, paid on receipt.
	stdRes, err := profit.Evaluate(base, seller)
	if err != nil {
		return Set{}, err
	}
	std := Option{
		Kind: Standard, Currency: cur, Quantity: req.Quantity, UnitPriceCents: req.UnitPriceCents,
		ShippingChargeCents: shipping(req.Quantity), Profit: stdRes, BaselineProfitCents: stdRes.ProfitCents,
		Reason: "Our list price, with shipping charged at exactly what it costs us, and payment on receipt.",
	}
	std.Summary = fmt.Sprintf("%d %s at %s each, shipping %s at cost, payment on receipt",
		std.Quantity, cfg.UnitNoun, FormatMoney(std.UnitPriceCents, cur), FormatMoney(std.ShippingChargeCents, cur))
	std.Question = fmt.Sprintf("How does %s work for you?", std.Summary)
	set.addUnlessLoss(std)

	// 2. Quantity: free shipping, earned by the smallest larger order (in steps) that keeps at least as much.
	found := false
	for q := (req.Quantity/cfg.QuantityStep + 1) * cfg.QuantityStep; q <= req.Quantity*10+cfg.QuantityStep; q += cfg.QuantityStep {
		d := base
		d.Quantity = q
		d.FreeShipping = true
		res, err := profit.Evaluate(d, seller)
		if err != nil {
			return Set{}, err
		}
		if res.ProfitCents >= stdRes.ProfitCents {
			o := Option{
				Kind: Quantity, Currency: cur, Quantity: q, UnitPriceCents: req.UnitPriceCents, FreeShipping: true,
				Profit: res, BaselineProfitCents: stdRes.ProfitCents,
				Reason: "A larger order covers the cost of shipping, so the shipping is on us.",
			}
			o.Summary = fmt.Sprintf("%d %s at %s each with free shipping, payment on receipt",
				q, cfg.UnitNoun, FormatMoney(o.UnitPriceCents, cur))
			o.Question = fmt.Sprintf("How would %s work for you?", o.Summary)
			set.addUnlessLoss(o)
			found = true
			break
		}
	}
	if !found {
		set.Omitted = append(set.Omitted, "quantity: no larger order up to ten times the asked quantity covers free shipping")
	}

	// 3. Early payment (plan B): the break-even discount for paying within EarlyDays instead of the asked terms.
	if req.RequestedPaymentDays <= cfg.EarlyDays {
		if req.PaymentDaysSettled {
			set.Omitted = append(set.Omitted, fmt.Sprintf("early_payment: the payment terms already on the table are within %d days", cfg.EarlyDays))
		} else {
			set.Omitted = append(set.Omitted, fmt.Sprintf("early_payment: the buyer did not ask for more than %d days to pay", cfg.EarlyDays))
		}
		return set, nil
	}
	be, err := profit.EarlyPaymentWorthIt(base, seller, req.RequestedPaymentDays, cfg.EarlyDays, 0)
	if err != nil {
		return Set{}, err
	}
	if be.BreakEvenDiscountBP < 10 {
		set.Omitted = append(set.Omitted, "early_payment: being paid sooner saves too little to fund any discount")
		return set, nil
	}
	d := base
	d.PaymentDays = cfg.EarlyDays
	d.EarlyPaymentDiscountBP = be.BreakEvenDiscountBP
	d.EarlyPaymentDays = cfg.EarlyDays
	res, err := profit.Evaluate(d, seller)
	if err != nil {
		return Set{}, err
	}
	e := Option{
		Kind: EarlyPayment, Currency: cur, Quantity: req.Quantity, UnitPriceCents: req.UnitPriceCents,
		ShippingChargeCents: shipping(req.Quantity), DiscountBP: be.BreakEvenDiscountBP, DueDays: cfg.EarlyDays,
		Profit: res, BaselineProfitCents: be.NormalProfitCents,
		Reason: "Being paid sooner saves us the cost of waiting, and we pass that saving on to you.",
	}
	e.Summary = fmt.Sprintf("%d %s at %s each with %s off, shipping %s at cost, payment within %d days",
		e.Quantity, cfg.UnitNoun, FormatMoney(e.UnitPriceCents, cur), FormatPercent(e.DiscountBP),
		FormatMoney(e.ShippingChargeCents, cur), e.DueDays)
	e.Question = fmt.Sprintf("What if you paid within %d days and took %s off the %d %s?",
		e.DueDays, FormatPercent(e.DiscountBP), e.Quantity, cfg.UnitNoun)
	set.addUnlessLoss(e)
	return set, nil
}

// addUnlessLoss offers a choice only if the seller keeps money on it; otherwise it records why it is missing.
func (s *Set) addUnlessLoss(o Option) {
	if o.Profit.ProfitCents < 0 {
		s.Omitted = append(s.Omitted, fmt.Sprintf("%s: at this price the order loses money", o.Kind))
		return
	}
	s.Options = append(s.Options, o)
}

// FormatMoney writes whole cents as a price, e.g. 4000 USD -> "$40.00".
func FormatMoney(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	num := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	switch strings.ToUpper(currency) {
	case "USD":
		return sign + "$" + num
	case "EUR":
		return sign + "€" + num
	case "GBP":
		return sign + "£" + num
	default:
		return sign + strings.ToUpper(currency) + " " + num
	}
}

// FormatPercent writes basis points as a percentage without trailing zeros, e.g. 150 -> "1.5%".
func FormatPercent(bp int64) string {
	s := fmt.Sprintf("%d.%02d", bp/100, bp%100)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}
