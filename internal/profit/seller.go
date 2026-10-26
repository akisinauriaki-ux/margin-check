package profit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type rawSeller struct {
	UnitCostCents               *json.Number `json:"unit_cost_cents"`
	ShippingCostCentsPerOrder   *json.Number `json:"shipping_cost_cents_per_order"`
	ShippingCostCentsPerBox     *json.Number `json:"shipping_cost_cents_per_box"`
	PayPalFeePercent            *json.Number `json:"paypal_fee_percent"`
	PayPalFixedFeeCents         *json.Number `json:"paypal_fixed_fee_cents"`
	YearlyCostOfMoneyPercent    *json.Number `json:"yearly_cost_of_money_percent"`
	UnderlineMinGiveawayPercent *json.Number `json:"underline_min_giveaway_percent"`
	Currency                    *string      `json:"currency"`
}

// parsePercentBP converts a decimal percentage string (e.g. "3.49", "10.0") to basis points without floats.
func parsePercentBP(num json.Number) (int64, error) {
	s := strings.TrimSpace(num.String())
	if s == "" {
		return 0, fmt.Errorf("empty percentage string")
	}

	parts := strings.Split(s, ".")
	intPart, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer part in percentage %q: %w", s, err)
	}

	bp := intPart * 100
	if len(parts) > 1 {
		decPart := parts[1]
		switch len(decPart) {
		case 0:
			// no fractional digits
		case 1:
			d, err := strconv.ParseInt(decPart, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid fractional part in %q: %w", s, err)
			}
			bp += d * 10
		case 2:
			d, err := strconv.ParseInt(decPart, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid fractional part in %q: %w", s, err)
			}
			bp += d
		default:
			twoDigits, err := strconv.ParseInt(decPart[:2], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid fractional part in %q: %w", s, err)
			}
			bp += twoDigits
			if decPart[2] >= '5' {
				bp++
			}
		}
	}

	return bp, nil
}

// parseIntCents parses an integer number string into int64 cents without floats.
func parseIntCents(num json.Number) (int64, error) {
	s := strings.TrimSpace(num.String())
	if s == "" {
		return 0, fmt.Errorf("empty integer string")
	}
	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", s, err)
	}
	return val, nil
}

// LoadSeller reads seller.json and validates all required baseline parameters.
// Missing unit cost, shipping cost, or cost of money returns an error naming each missing value.
// PayPal fees have sourced defaults from evidence E-501 (3.49%) and E-502 (49 cents).
func LoadSeller(path string) (Seller, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Seller{}, fmt.Errorf("failed to read seller file %s: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var raw rawSeller
	if err := dec.Decode(&raw); err != nil {
		return Seller{}, fmt.Errorf("failed to parse seller json %s: %w", path, err)
	}

	var missing []string
	if raw.UnitCostCents == nil {
		missing = append(missing, "unit_cost_cents")
	}
	if raw.ShippingCostCentsPerOrder == nil {
		missing = append(missing, "shipping_cost_cents_per_order")
	}
	if raw.ShippingCostCentsPerBox == nil {
		missing = append(missing, "shipping_cost_cents_per_box")
	}
	if raw.YearlyCostOfMoneyPercent == nil {
		missing = append(missing, "yearly_cost_of_money_percent (cost of money)")
	}

	if len(missing) > 0 {
		return Seller{}, fmt.Errorf("seller configuration missing required values: %s", strings.Join(missing, ", "))
	}

	unitCost, err := parseIntCents(*raw.UnitCostCents)
	if err != nil {
		return Seller{}, fmt.Errorf("failed to parse unit_cost_cents: %w", err)
	}

	shipOrder, err := parseIntCents(*raw.ShippingCostCentsPerOrder)
	if err != nil {
		return Seller{}, fmt.Errorf("failed to parse shipping_cost_cents_per_order: %w", err)
	}

	shipBox, err := parseIntCents(*raw.ShippingCostCentsPerBox)
	if err != nil {
		return Seller{}, fmt.Errorf("failed to parse shipping_cost_cents_per_box: %w", err)
	}

	costOfMoneyBP, err := parsePercentBP(*raw.YearlyCostOfMoneyPercent)
	if err != nil {
		return Seller{}, fmt.Errorf("failed to parse yearly_cost_of_money_percent: %w", err)
	}

	// Sourced defaults for PayPal fee (E-501: 3.49%, E-502: 49 cents)
	paypalFeeBP := int64(349)
	if raw.PayPalFeePercent != nil {
		parsedBP, err := parsePercentBP(*raw.PayPalFeePercent)
		if err != nil {
			return Seller{}, fmt.Errorf("failed to parse paypal_fee_percent: %w", err)
		}
		paypalFeeBP = parsedBP
	}

	paypalFixedFee := int64(49)
	if raw.PayPalFixedFeeCents != nil {
		parsedFixed, err := parseIntCents(*raw.PayPalFixedFeeCents)
		if err != nil {
			return Seller{}, fmt.Errorf("failed to parse paypal_fixed_fee_cents: %w", err)
		}
		paypalFixedFee = parsedFixed
	}

	underlineBP := int64(200) // 2.0%
	if raw.UnderlineMinGiveawayPercent != nil {
		parsedUnderline, err := parsePercentBP(*raw.UnderlineMinGiveawayPercent)
		if err != nil {
			return Seller{}, fmt.Errorf("failed to parse underline_min_giveaway_percent: %w", err)
		}
		underlineBP = parsedUnderline
	}

	currency := "USD"
	if raw.Currency != nil && strings.TrimSpace(*raw.Currency) != "" {
		currency = strings.TrimSpace(*raw.Currency)
	}

	return Seller{
		UnitCostCents:             unitCost,
		ShippingCostCentsPerOrder: shipOrder,
		ShippingCostCentsPerBox:   shipBox,
		PayPalFeePercentBP:        paypalFeeBP,
		PayPalFixedFeeCents:       paypalFixedFee,
		YearlyCostOfMoneyBP:       costOfMoneyBP,
		UnderlineMinGiveawayBP:    underlineBP,
		Currency:                  currency,
	}, nil
}
