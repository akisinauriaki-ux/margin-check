// Package quote verifies that a quoted sentence appears in the email thread
// after normalising reply-chain markers, wrapped lines, curly quotes, and dashes,
// and that the claimed value can be read from the quote via package normalize.
package quote

import (
	"errors"
	"fmt"
	"strings"

	"margincheck/internal/normalize"
)

var (
	// ErrQuoteEmpty is returned when the quote string is empty or whitespace.
	ErrQuoteEmpty = errors.New("quote is empty")
	// ErrQuoteNotInEmail is returned when the normalised quote is not found in the normalised email.
	ErrQuoteNotInEmail = errors.New("quote not found in email")
	// ErrValueNotInQuote is returned when the quote does not contain or back the claimed value.
	ErrValueNotInQuote = errors.New("value cannot be read from quote")
)

// Result holds both the original quote (kept for display) and its normalised form.
type Result struct {
	Original   string
	Normalized string
}

// NormalizeText strips leading '>' reply-chain markers on each line, joins wrapped lines,
// straightens curly quotes and dashes, and collapses whitespace to single spaces.
func NormalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for strings.HasPrefix(trimmed, ">") {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
		}
		lines[i] = trimmed
	}
	joined := strings.Join(lines, " ")

	replacer := strings.NewReplacer(
		"‘", "'",
		"’", "'",
		"“", `"`,
		"”", `"`,
		"–", "-",
		"—", "-",
		"\u00a0", " ",
	)
	joined = replacer.Replace(joined)
	return strings.Join(strings.Fields(joined), " ")
}

// ContainsQuote checks that sentence (after normalisation) appears in emailText (after normalisation).
// The original sentence is preserved in Result.Original for display.
func ContainsQuote(emailText, sentence string) (Result, error) {
	normQuote := NormalizeText(sentence)
	if normQuote == "" {
		return Result{Original: sentence}, ErrQuoteEmpty
	}
	normEmail := NormalizeText(emailText)
	if !strings.Contains(strings.ToLower(normEmail), strings.ToLower(normQuote)) {
		return Result{Original: sentence, Normalized: normQuote}, fmt.Errorf("%w: %q", ErrQuoteNotInEmail, sentence)
	}
	return Result{Original: sentence, Normalized: normQuote}, nil
}

// Verify checks that sentence is in emailText and that the claimed value for field can be read from sentence.
func Verify(emailText, sentence, field string, intVal int64, boolVal bool, strVal string) (Result, error) {
	res, err := ContainsQuote(emailText, sentence)
	if err != nil {
		return res, err
	}

	// If the value is explicitly UNKNOWN or UNCLEAR, any verbatim sentence from the email is valid evidence.
	if strings.EqualFold(strVal, "UNKNOWN") || strings.EqualFold(strVal, "UNCLEAR") {
		return res, nil
	}

	normSent := res.Normalized
	lowerSent := strings.ToLower(normSent)

	switch field {
	case "quantity":
		qs := normalize.ExtractQuantities(normSent)
		for _, q := range qs {
			if q == intVal {
				return res, nil
			}
		}
		return res, fmt.Errorf("%w: quantity %d not found in %q", ErrValueNotInQuote, intVal, sentence)

	case "unit_price_cents", "shipping_cents", "coop_allowance_cents":
		ms := normalize.ExtractMoney(normSent)
		for _, m := range ms {
			if m.Int == intVal {
				return res, nil
			}
		}
		return res, fmt.Errorf("%w: amount %d cents not found in %q", ErrValueNotInQuote, intVal, sentence)

	case "discount_percent", "rebate_percent":
		ps := normalize.ExtractPercents(normSent)
		for _, p := range ps {
			if p == intVal {
				return res, nil
			}
		}
		if intVal == 0 {
			return res, nil
		}
		return res, fmt.Errorf("%w: percent %d%% not found in %q", ErrValueNotInQuote, intVal, sentence)

	case "payment_days":
		ds := normalize.ExtractPaymentDays(normSent)
		for _, d := range ds {
			if d == intVal {
				return res, nil
			}
		}
		return res, fmt.Errorf("%w: payment_days %d not found in %q", ErrValueNotInQuote, intVal, sentence)

	case "free_shipping":
		if boolVal {
			if strings.Contains(lowerSent, "free shipping") ||
				strings.Contains(lowerSent, "free delivery") ||
				strings.Contains(lowerSent, "shipping included") ||
				strings.Contains(lowerSent, "no shipping") {
				return res, nil
			}
			return res, fmt.Errorf("%w: free_shipping=true not backed by %q", ErrValueNotInQuote, sentence)
		}
		return res, nil

	case "price_increase_discounted":
		if boolVal {
			hasPct := len(normalize.ExtractPercents(normSent)) > 0 || strings.Contains(lowerSent, "discount")
			hasNewOrInc := strings.Contains(lowerSent, "new") ||
				strings.Contains(lowerSent, "increase") ||
				strings.Contains(lowerSent, "hike") ||
				len(normalize.ExtractMoney(normSent)) > 0
			if hasPct && hasNewOrInc {
				return res, nil
			}
			return res, fmt.Errorf("%w: price_increase_discounted=true not backed by %q", ErrValueNotInQuote, sentence)
		}
		return res, nil

	case "accepted_option":
		switch strVal {
		case "standard", "quantity", "early_payment", "changed":
			return res, nil
		default:
			return res, fmt.Errorf("%w: invalid accepted_option %q", ErrValueNotInQuote, strVal)
		}
	}

	return res, nil
}
