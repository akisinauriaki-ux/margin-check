package profit

import (
	"errors"
)

var (
	// ErrMixedCurrency is returned when a deal contains multiple currencies or differs from the seller's currency.
	ErrMixedCurrency = errors.New("profit: mixed or mismatched currency")
)

// Money represents a monetary amount in whole cents and its currency code.
type Money struct {
	Cents    int64  `json:"cents"`
	Currency string `json:"currency"`
}

// Seller contains cost baselines and policy parameters for a seller.
type Seller struct {
	UnitCostCents             int64  `json:"unit_cost_cents"`
	ShippingCostCentsPerOrder int64  `json:"shipping_cost_cents_per_order"`
	ShippingCostCentsPerBox   int64  `json:"shipping_cost_cents_per_box"`
	PayPalFeePercentBP        int64  `json:"paypal_fee_percent_bp"`
	PayPalFixedFeeCents       int64  `json:"paypal_fixed_fee_cents"`
	YearlyCostOfMoneyBP       int64  `json:"yearly_cost_of_money_bp"`
	UnderlineMinGiveawayBP    int64  `json:"underline_min_giveaway_bp"`
	Currency                  string `json:"currency"`
}

// Deal represents the terms of an order to evaluate.
type Deal struct {
	Currency               string `json:"currency"`
	Quantity               int64  `json:"quantity"`
	UnitPriceCents         int64  `json:"unit_price_cents"`
	OrderSizeDiscountBP    int64  `json:"order_size_discount_bp"`
	OtherDiscountBP        int64  `json:"other_discount_bp"`
	RebateBP               int64  `json:"rebate_bp"`
	FreeShipping           bool   `json:"free_shipping"`
	PaymentDays            int64  `json:"payment_days"`
	EarlyPaymentDiscountBP int64  `json:"early_payment_discount_bp"`
	EarlyPaymentDays       int64  `json:"early_payment_days"`
	CoopAllowance          Money  `json:"coop_allowance"`
	OldUnitPriceCents      int64  `json:"old_unit_price_cents"`
}

// LeakLine represents one line in the pocket price waterfall.
type LeakLine struct {
	Name   string `json:"name"`
	Cents  int64  `json:"cents"`
	Reason string `json:"reason"`
}

// Result is the financial outcome of evaluating a deal against seller baselines.
type Result struct {
	ListValueCents        int64      `json:"list_value_cents"`
	RevenueCents          int64      `json:"revenue_cents"`
	CostsCents            int64      `json:"costs_cents"`
	ProfitCents           int64      `json:"profit_cents"`
	GiveawayCents         int64      `json:"giveaway_cents"`
	Leaks                 []LeakLine `json:"leaks"`
	DoubleDiscountFinding string     `json:"double_discount_finding,omitempty"`
	DoubleDiscountCents   int64      `json:"double_discount_cents,omitempty"`
	Level                 string     `json:"level"`
}

// EarlyPaymentResult is the outcome of comparing normal vs early payment terms.
type EarlyPaymentResult struct {
	WorthIt             bool  `json:"worth_it"`
	NormalProfitCents   int64 `json:"normal_profit_cents"`
	EarlyProfitCents    int64 `json:"early_profit_cents"`
	BreakEvenDiscountBP int64 `json:"break_even_discount_bp"`
}

// LoadSeller reads seller.json and validates all required fields without defaulting.
func LoadSeller(path string) (Seller, error) {
	return Seller{}, errors.New("not implemented")
}

// Evaluate computes profit, giveaway, pocket price waterfall leaks, and level.
func Evaluate(deal Deal, seller Seller) (Result, error) {
	return Result{}, errors.New("not implemented")
}

// EarlyPaymentWorthIt evaluates if an early-payment discount keeps at least normal profit.
func EarlyPaymentWorthIt(deal Deal, seller Seller, normalDays int64, earlyDays int64, discountBP int64) (EarlyPaymentResult, error) {
	return EarlyPaymentResult{}, errors.New("not implemented")
}
