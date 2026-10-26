package profit

import (
	"fmt"
)

// roundDiv divides n by d and rounds to the nearest integer, halves away from zero.
// Whole cents and basis points only, zero floats.
func roundDiv(n, d int64) int64 {
	if d == 0 {
		panic("profit: division by zero in roundDiv")
	}
	if d < 0 {
		n = -n
		d = -d
	}
	if n >= 0 {
		return (2*n + d) / (2 * d)
	}
	return (2*n - d) / (2 * d)
}

// formatBP formats basis points as a percentage string (e.g. 349 -> "3.49%").
func formatBP(bp int64) string {
	return fmt.Sprintf("%d.%02d%%", bp/100, bp%100)
}

// formatCents formats cents as a monetary dollar string (e.g. 4237 -> "$42.37").
func formatCents(cents int64) string {
	if cents < 0 {
		abs := -cents
		return fmt.Sprintf("-$%d.%02d", abs/100, abs%100)
	}
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

// Evaluate computes the deal's list value, revenue, profit, giveaway,
// pocket price waterfall leak lines, double discounting finding, and alert level.
func Evaluate(deal Deal, seller Seller) (Result, error) {
	// Refuse mixed currencies
	if deal.Currency != "" && seller.Currency != "" && deal.Currency != seller.Currency {
		return Result{}, ErrMixedCurrency
	}
	if deal.CoopAllowance.Currency != "" && deal.Currency != "" && deal.CoopAllowance.Currency != deal.Currency {
		return Result{}, ErrMixedCurrency
	}

	// Refuse invalid quantity or price
	if deal.Quantity <= 0 {
		return Result{}, fmt.Errorf("profit: deal quantity must be positive, got %d", deal.Quantity)
	}
	if deal.UnitPriceCents < 0 {
		return Result{}, fmt.Errorf("profit: deal unit price cannot be negative, got %d", deal.UnitPriceCents)
	}
	if deal.OldUnitPriceCents < 0 {
		return Result{}, fmt.Errorf("profit: old unit price cannot be negative, got %d", deal.OldUnitPriceCents)
	}
	if deal.OrderSizeDiscountBP < 0 || deal.OtherDiscountBP < 0 || deal.EarlyPaymentDiscountBP < 0 || deal.RebateBP < 0 {
		return Result{}, fmt.Errorf("profit: discounts and rebates cannot be negative")
	}
	if deal.PaymentDays < 0 {
		return Result{}, fmt.Errorf("profit: payment days cannot be negative, got %d", deal.PaymentDays)
	}

	listValue := deal.Quantity * deal.UnitPriceCents

	// Waterfall On-Invoice and Off-Invoice Revenue Leaks
	// 1. Order-size discount
	rem := listValue
	leak1 := roundDiv(rem*deal.OrderSizeDiscountBP, 10000)
	rem -= leak1

	// 2. Other discount
	leak2 := roundDiv(rem*deal.OtherDiscountBP, 10000)
	rem -= leak2

	// 6. Early-payment discount (if agreed as part of the deal terms)
	leak6 := roundDiv(rem*deal.EarlyPaymentDiscountBP, 10000)
	rem -= leak6

	// 3. Rebate or volume bonus (off the invoice)
	leak3 := roundDiv(rem*deal.RebateBP, 10000)
	revenue := rem - leak3

	// Base cost of goods sold (incremental unit cost only)
	baseUnitCost := deal.Quantity * seller.UnitCostCents

	// 4. Free shipping freight cost absorbed by seller
	leak4 := int64(0)
	if deal.FreeShipping {
		leak4 = seller.ShippingCostCentsPerOrder + deal.Quantity*seller.ShippingCostCentsPerBox
	}

	// 7. Co-op or marketing allowance
	leak7 := deal.CoopAllowance.Cents

	// 5. Payment days (cost of waiting: revenue * yearly rate * payment days / 365)
	leak5 := int64(0)
	if deal.PaymentDays > 0 {
		leak5 = roundDiv(revenue*seller.YearlyCostOfMoneyBP*deal.PaymentDays, 10000*365)
	}

	// 8. PayPal transaction fee (percent of revenue + fixed fee)
	leak8 := roundDiv(revenue*seller.PayPalFeePercentBP, 10000) + seller.PayPalFixedFeeCents

	totalCosts := baseUnitCost + leak4 + leak7 + leak5 + leak8
	profit := revenue - totalCosts

	// List Price Order baseline (no discounts, no free shipping, no rebate, no allowance, paid on receipt)
	listFee := roundDiv(listValue*seller.PayPalFeePercentBP, 10000) + seller.PayPalFixedFeeCents
	listCosts := baseUnitCost + listFee
	listProfit := listValue - listCosts
	giveaway := listProfit - profit

	// Alert Level determination:
	// - panel: profit < 0
	// - underline: giveaway >= threshold * listValue
	// - silent: otherwise
	var level string
	if profit < 0 {
		level = "panel"
	} else if giveaway*10000 >= seller.UnderlineMinGiveawayBP*listValue {
		level = "underline"
	} else {
		level = "silent"
	}

	// Double Discounting check
	var doubleFinding string
	var doubleCents int64
	if deal.OldUnitPriceCents > 0 && deal.UnitPriceCents > deal.OldUnitPriceCents {
		increase := deal.UnitPriceCents - deal.OldUnitPriceCents
		totalDiscBP := deal.OrderSizeDiscountBP + deal.OtherDiscountBP + deal.EarlyPaymentDiscountBP
		if totalDiscBP > 0 {
			doubleCents = roundDiv(deal.Quantity*increase*totalDiscBP, 10000)
			doubleFinding = fmt.Sprintf("double discount: giving away %s on the price increase alone (%s increase × %s discount × %d units)",
				formatCents(doubleCents), formatCents(increase), formatBP(totalDiscBP), deal.Quantity)
		}
	}

	// 8 Pocket Price Waterfall lines
	leaks := []LeakLine{
		{
			Name:   "order_size_discount",
			Cents:  leak1,
			Reason: fmt.Sprintf("order-size discount of %s on invoice", formatBP(deal.OrderSizeDiscountBP)),
		},
		{
			Name:   "other_discount",
			Cents:  leak2,
			Reason: fmt.Sprintf("promotional or negotiated discount of %s on invoice", formatBP(deal.OtherDiscountBP)),
		},
		{
			Name:   "rebate",
			Cents:  leak3,
			Reason: fmt.Sprintf("volume rebate or bonus of %s off invoice", formatBP(deal.RebateBP)),
		},
		{
			Name:   "free_shipping",
			Cents:  leak4,
			Reason: fmt.Sprintf("freight absorbed by seller (%s per order + %s per box)", formatCents(seller.ShippingCostCentsPerOrder), formatCents(seller.ShippingCostCentsPerBox)),
		},
		{
			Name:   "payment_days",
			Cents:  leak5,
			Reason: fmt.Sprintf("cost of waiting for %d days at %s yearly rate of money", deal.PaymentDays, formatBP(seller.YearlyCostOfMoneyBP)),
		},
		{
			Name:   "early_payment_discount",
			Cents:  leak6,
			Reason: fmt.Sprintf("early-payment discount of %s for payment within %d days", formatBP(deal.EarlyPaymentDiscountBP), deal.EarlyPaymentDays),
		},
		{
			Name:   "coop_allowance",
			Cents:  leak7,
			Reason: fmt.Sprintf("co-op marketing or advertising allowance of %s", formatCents(leak7)),
		},
		{
			Name:   "paypal_fee",
			Cents:  leak8,
			Reason: fmt.Sprintf("PayPal fee at %s + %s fixed", formatBP(seller.PayPalFeePercentBP), formatCents(seller.PayPalFixedFeeCents)),
		},
	}

	return Result{
		ListValueCents:        listValue,
		RevenueCents:          revenue,
		CostsCents:            totalCosts,
		ProfitCents:           profit,
		GiveawayCents:         giveaway,
		Leaks:                 leaks,
		DoubleDiscountFinding: doubleFinding,
		DoubleDiscountCents:   doubleCents,
		Level:                 level,
	}, nil
}

// EarlyPaymentWorthIt compares the profit when paid in normalDays with no discount
// against the profit when paid in earlyDays with discountBP. It returns whether
// the discount is worth it, both profits, and the break-even discount (largest discount in steps of 10 BP keeping at least normal profit).
func EarlyPaymentWorthIt(deal Deal, seller Seller, normalDays int64, earlyDays int64, discountBP int64) (EarlyPaymentResult, error) {
	// Normal terms: paid in normalDays, no discount
	normalDeal := deal
	normalDeal.PaymentDays = normalDays
	normalDeal.OrderSizeDiscountBP = 0
	normalDeal.OtherDiscountBP = 0
	normalDeal.EarlyPaymentDiscountBP = 0
	normalDeal.EarlyPaymentDays = 0
	normalDeal.RebateBP = 0

	resNormal, err := Evaluate(normalDeal, seller)
	if err != nil {
		return EarlyPaymentResult{}, fmt.Errorf("failed to evaluate normal terms: %w", err)
	}

	// Early terms: paid in earlyDays with discountBP
	earlyDeal := deal
	earlyDeal.PaymentDays = earlyDays
	earlyDeal.OrderSizeDiscountBP = 0
	earlyDeal.OtherDiscountBP = discountBP
	earlyDeal.EarlyPaymentDiscountBP = 0
	earlyDeal.EarlyPaymentDays = earlyDays
	earlyDeal.RebateBP = 0

	resEarly, err := Evaluate(earlyDeal, seller)
	if err != nil {
		return EarlyPaymentResult{}, fmt.Errorf("failed to evaluate early terms: %w", err)
	}

	worthIt := resEarly.ProfitCents >= resNormal.ProfitCents

	// Find break-even discount in steps of 10 basis points
	var breakEvenBP int64
	for bp := int64(0); bp <= 10000; bp += 10 {
		testDeal := deal
		testDeal.PaymentDays = earlyDays
		testDeal.OrderSizeDiscountBP = 0
		testDeal.OtherDiscountBP = bp
		testDeal.EarlyPaymentDiscountBP = 0
		testDeal.EarlyPaymentDays = earlyDays
		testDeal.RebateBP = 0

		testRes, err := Evaluate(testDeal, seller)
		if err != nil {
			break
		}
		if testRes.ProfitCents >= resNormal.ProfitCents {
			breakEvenBP = bp
		} else {
			break
		}
	}

	return EarlyPaymentResult{
		WorthIt:             worthIt,
		NormalProfitCents:   resNormal.ProfitCents,
		EarlyProfitCents:    resEarly.ProfitCents,
		BreakEvenDiscountBP: breakEvenBP,
	}, nil
}
