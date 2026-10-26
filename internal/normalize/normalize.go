// Package normalize converts natural-language number words, k-notation,
// payment terms, percentages, and currency-tagged amounts into exact int64 values.
// No float64 is used anywhere in money or percentage arithmetic.
package normalize

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// ErrCantParse is returned whenever an input cannot be deterministically parsed.
var ErrCantParse = errors.New("can't parse")

// Kind identifies what domain value is being parsed.
type Kind string

const (
	KindQuantity    Kind = "quantity"
	KindMoney       Kind = "money"
	KindPercent     Kind = "percent"
	KindPaymentDays Kind = "payment_days"
)

// Value holds a parsed integer value (cents for money, whole percent for percent,
// count for quantity, days for payment_days) and its ISO-4217 currency code for money.
type Value struct {
	Kind     Kind
	Int      int64
	Currency string
}

var smallWords = map[string]int64{
	"zero":      0,
	"one":       1,
	"two":       2,
	"three":     3,
	"four":      4,
	"five":      5,
	"six":       6,
	"seven":     7,
	"eight":     8,
	"nine":      9,
	"ten":       10,
	"eleven":    11,
	"twelve":    12,
	"thirteen":  13,
	"fourteen":  14,
	"fifteen":   15,
	"sixteen":   16,
	"seventeen": 17,
	"eighteen":  18,
	"nineteen":  19,
}

var tensWords = map[string]int64{
	"twenty":  20,
	"thirty":  30,
	"forty":   40,
	"fifty":   50,
	"sixty":   60,
	"seventy": 70,
	"eighty":  80,
	"ninety":  90,
}

// Parse parses s according to kind and returns ErrCantParse if it cannot be deterministically parsed.
func Parse(kind Kind, s string) (Value, error) {
	switch kind {
	case KindQuantity:
		q, err := ParseQuantity(s)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KindQuantity, Int: q}, nil
	case KindMoney:
		cents, curr, err := ParseMoney(s)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KindMoney, Int: cents, Currency: curr}, nil
	case KindPercent:
		p, err := ParsePercent(s)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KindPercent, Int: p}, nil
	case KindPaymentDays:
		d, err := ParsePaymentDays(s)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KindPaymentDays, Int: d}, nil
	default:
		return Value{}, ErrCantParse
	}
}

// DetectCurrencies returns all distinct ISO-4217 currency codes ("USD", "EUR", "GBP") mentioned in s.
func DetectCurrencies(s string) []string {
	var found []string
	add := func(c string) {
		for _, existing := range found {
			if existing == c {
				return
			}
		}
		found = append(found, c)
	}
	if strings.Contains(s, "$") {
		add("USD")
	}
	if strings.Contains(s, "€") {
		add("EUR")
	}
	if strings.Contains(s, "£") {
		add("GBP")
	}
	lower := strings.ToLower(s)
	tokens := tokenizeWords(lower)
	for _, tok := range tokens {
		switch tok {
		case "usd", "dollar", "dollars":
			add("USD")
		case "eur", "euro", "euros":
			add("EUR")
		case "gbp", "pound", "pounds", "sterling":
			add("GBP")
		}
	}
	return found
}

var (
	reMoneySymbolPrefix = regexp.MustCompile(`(?i)([$€£])\s*([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.([0-9]{1,2}))?\s*(k)?\b`)
	reMoneyCodePrefix   = regexp.MustCompile(`(?i)\b(USD|EUR|GBP)\s*([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.([0-9]{1,2}))?\s*(k)?\b`)
	reMoneyCodeSuffix   = regexp.MustCompile(`(?i)\b([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.([0-9]{1,2}))?\s*(k)?\s*(USD|EUR|GBP|dollars?|euros?|pounds?)\b`)
)

// ParseMoney parses a single money amount and its currency in whole cents.
func ParseMoney(s string) (int64, string, error) {
	vals := ExtractMoney(s)
	if len(vals) != 1 {
		return 0, "", ErrCantParse
	}
	currs := DetectCurrencies(s)
	if len(currs) > 1 {
		return 0, "", ErrCantParse
	}
	return vals[0].Int, vals[0].Currency, nil
}

// ExtractMoney extracts all currency-tagged monetary amounts (in cents) from s.
func ExtractMoney(s string) []Value {
	var out []Value
	addUnique := func(cents int64, curr string) {
		for _, v := range out {
			if v.Int == cents && v.Currency == curr {
				return
			}
		}
		out = append(out, Value{Kind: KindMoney, Int: cents, Currency: curr})
	}

	for _, m := range reMoneySymbolPrefix.FindAllStringSubmatch(s, -1) {
		curr := symbolToCurrency(m[1])
		cents, err := partsToCents(m[2], m[3], m[4] != "")
		if err == nil && curr != "" {
			addUnique(cents, curr)
		}
	}
	for _, m := range reMoneyCodePrefix.FindAllStringSubmatch(s, -1) {
		curr := wordToCurrency(m[1])
		cents, err := partsToCents(m[2], m[3], m[4] != "")
		if err == nil && curr != "" {
			addUnique(cents, curr)
		}
	}
	for _, m := range reMoneyCodeSuffix.FindAllStringSubmatch(s, -1) {
		curr := wordToCurrency(m[4])
		cents, err := partsToCents(m[1], m[2], m[3] != "")
		if err == nil && curr != "" {
			addUnique(cents, curr)
		}
	}

	// Also check number words followed by currency words, e.g. "forty-two dollars", "one hundred twenty euros"
	tokens := tokenizeWords(strings.ToLower(s))
	for i := 0; i < len(tokens); i++ {
		curr := wordToCurrency(tokens[i])
		if curr == "" {
			continue
		}
		// Look backwards for number words immediately preceding the currency word
		if i > 0 {
			if n, ok := parseTrailingNumberWords(tokens[:i]); ok && n > 0 {
				addUnique(n*100, curr)
			}
		}
	}

	return out
}

func symbolToCurrency(sym string) string {
	switch sym {
	case "$":
		return "USD"
	case "€":
		return "EUR"
	case "£":
		return "GBP"
	default:
		return ""
	}
}

func wordToCurrency(w string) string {
	switch strings.ToLower(w) {
	case "usd", "dollar", "dollars":
		return "USD"
	case "eur", "euro", "euros":
		return "EUR"
	case "gbp", "pound", "pounds", "sterling":
		return "GBP"
	default:
		return ""
	}
}

// partsToCents converts whole digits, optional 1-2 fractional digits, and optional "k" (x1000)
// into integer cents without float64.
func partsToCents(wholeStr, fracStr string, hasK bool) (int64, error) {
	cleanWhole := strings.ReplaceAll(wholeStr, ",", "")
	whole, err := strconv.ParseInt(cleanWhole, 10, 64)
	if err != nil || whole < 0 {
		return 0, ErrCantParse
	}
	if !hasK {
		var frac int64
		if len(fracStr) == 1 {
			d, err := strconv.ParseInt(fracStr, 10, 64)
			if err != nil {
				return 0, ErrCantParse
			}
			frac = d * 10
		} else if len(fracStr) == 2 {
			d, err := strconv.ParseInt(fracStr, 10, 64)
			if err != nil {
				return 0, ErrCantParse
			}
			frac = d
		} else if len(fracStr) > 2 {
			return 0, ErrCantParse
		}
		return whole*100 + frac, nil
	}
	// With "k": whole * 100,000 cents + frac
	cents := whole * 100000
	if len(fracStr) == 1 {
		d, err := strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, ErrCantParse
		}
		cents += d * 10000
	} else if len(fracStr) == 2 {
		d, err := strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, ErrCantParse
		}
		cents += d * 1000
	} else if len(fracStr) > 2 {
		return 0, ErrCantParse
	}
	return cents, nil
}

var (
	rePercentDigit = regexp.MustCompile(`(?i)\b([0-9]{1,3})\s*(?:%|(?:percent|per\s+cent)\b)`)
	reHalfOff      = regexp.MustCompile(`(?i)\bhalf\s+off\b`)
)

// ParsePercent parses a single percentage (0..100) from s.
func ParsePercent(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, ErrCantParse
	}
	vals := ExtractPercents(trimmed)
	if len(vals) == 1 {
		return vals[0], nil
	}
	return 0, ErrCantParse
}

// ExtractPercents extracts all percentage integers (0..100) from s.
func ExtractPercents(s string) []int64 {
	var out []int64
	addUnique := func(v int64) {
		if v < 0 || v > 100 {
			return
		}
		for _, existing := range out {
			if existing == v {
				return
			}
		}
		out = append(out, v)
	}

	if reHalfOff.MatchString(s) {
		addUnique(50)
	}
	for _, m := range rePercentDigit.FindAllStringSubmatch(s, -1) {
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err == nil {
			addUnique(v)
		}
	}

	tokens := tokenizeWords(strings.ToLower(s))
	for i := 0; i < len(tokens); i++ {
		isPctWord := tokens[i] == "percent" || tokens[i] == "off" ||
			(tokens[i] == "per" && i+1 < len(tokens) && tokens[i+1] == "cent")
		if !isPctWord || i == 0 {
			continue
		}
		if n, ok := parseTrailingNumberWords(tokens[:i]); ok && n >= 0 && n <= 100 {
			addUnique(n)
		}
	}

	return out
}

var (
	reNetDays       = regexp.MustCompile(`(?i)\bnet\s+([0-9]{1,3})\b`)
	reWithinDays    = regexp.MustCompile(`(?i)\b(?:within|in|net)\s+([0-9]{1,3})\s+days?\b`)
	reNumberDays    = regexp.MustCompile(`(?i)\b([0-9]{1,3})\s+days?\b`)
	reDueOnReceipt  = regexp.MustCompile(`(?i)\b(due\s+on\s+receipt|upon\s+receipt|on\s+receipt|paid\s+on\s+receipt|payment\s+is\s+made\s+upon\s+receipt)\b`)
	reAWeek         = regexp.MustCompile(`(?i)\b(a\s+week|one\s+week|this\s+week)\b`)
	reTwoWeeks      = regexp.MustCompile(`(?i)\b(two\s+weeks|a\s+fortnight)\b`)
	reAMonth        = regexp.MustCompile(`(?i)\b(a\s+month|one\s+month)\b`)
	reTwoMonths     = regexp.MustCompile(`(?i)\b(two\s+months)\b`)
	reThreeMonths   = regexp.MustCompile(`(?i)\b(three\s+months)\b`)
)

// ParsePaymentDays parses payment terms in days (e.g. "net 60" -> 60, "two months" -> 60,
// "a week" -> 7, "due on receipt" -> 0).
func ParsePaymentDays(s string) (int64, error) {
	vals := ExtractPaymentDays(s)
	if len(vals) == 1 {
		return vals[0], nil
	}
	return 0, ErrCantParse
}

// ExtractPaymentDays extracts all payment-day values mentioned in s.
func ExtractPaymentDays(s string) []int64 {
	var out []int64
	addUnique := func(v int64) {
		if v < 0 {
			return
		}
		for _, existing := range out {
			if existing == v {
				return
			}
		}
		out = append(out, v)
	}

	if reDueOnReceipt.MatchString(s) {
		addUnique(0)
	}
	for _, m := range reNetDays.FindAllStringSubmatch(s, -1) {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			addUnique(v)
		}
	}
	for _, m := range reWithinDays.FindAllStringSubmatch(s, -1) {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			addUnique(v)
		}
	}
	for _, m := range reNumberDays.FindAllStringSubmatch(s, -1) {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			addUnique(v)
		}
	}
	if reAWeek.MatchString(s) {
		addUnique(7)
	}
	if reTwoWeeks.MatchString(s) {
		addUnique(14)
	}
	if reAMonth.MatchString(s) {
		addUnique(30)
	}
	if reTwoMonths.MatchString(s) {
		addUnique(60)
	}
	if reThreeMonths.MatchString(s) {
		addUnique(90)
	}

	// Also handle number words before "days", "weeks", "months", or after "net"
	tokens := tokenizeWords(strings.ToLower(s))
	for i := 0; i < len(tokens); i++ {
		switch tokens[i] {
		case "day", "days":
			if i > 0 {
				if n, ok := parseTrailingNumberWords(tokens[:i]); ok {
					addUnique(n)
				}
			}
		case "week", "weeks":
			if i > 0 {
				if n, ok := parseTrailingNumberWords(tokens[:i]); ok {
					addUnique(n * 7)
				}
			}
		case "month", "months":
			if i > 0 {
				if n, ok := parseTrailingNumberWords(tokens[:i]); ok {
					addUnique(n * 30)
				}
			}
		case "net":
			if i+1 < len(tokens) {
				if n, consumed := parseLeadingNumberWords(tokens[i+1:]); consumed > 0 {
					addUnique(n)
				}
			}
		}
	}

	return out
}

var (
	reKNumber   = regexp.MustCompile(`(?i)^([0-9]+)(?:\.([0-9]{1,3}))?k$`)
	reDozen     = regexp.MustCompile(`(?i)\b(a|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|[0-9]+)\s+dozen\b`)
	rePlainInt  = regexp.MustCompile(`^[0-9]{1,9}(?:,[0-9]{3})*$`)
	reUnitQuant = regexp.MustCompile(`(?i)\b([0-9]{1,6}(?:\.[0-9]{1,3})?k|[0-9]{1,3}(?:,[0-9]{3})+|[0-9]{1,6})\s+(?:boxes|box|units|unit|tins|jars|bottles|bowls|mugs|candles|planters|blankets|teapots|napkins|plates|sets|cases|packs|items|pieces|cartons)\b`)
)

// ParseQuantity parses a quantity count from s (e.g. "fifty boxes" -> 50, "a dozen" -> 12, "1.2k" -> 1200).
func ParseQuantity(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, ErrCantParse
	}
	// If the string has a currency symbol/code, it's money, not a bare quantity
	if len(DetectCurrencies(trimmed)) > 0 {
		return 0, ErrCantParse
	}
	// Check exact k-number first (e.g. "1.2k")
	if m := reKNumber.FindStringSubmatch(trimmed); m != nil {
		return kPartsToInt(m[1], m[2])
	}
	// Check exact integer (e.g. "40" or "1,200")
	if rePlainInt.MatchString(trimmed) {
		v, err := strconv.ParseInt(strings.ReplaceAll(trimmed, ",", ""), 10, 64)
		if err != nil || v <= 0 {
			return 0, ErrCantParse
		}
		return v, nil
	}
	vals := ExtractQuantities(trimmed)
	if len(vals) == 1 {
		return vals[0], nil
	}
	// Check if the entire string is number words
	tokens := tokenizeWords(strings.ToLower(trimmed))
	if len(tokens) > 0 {
		if n, consumed := parseLeadingNumberWords(tokens); consumed == len(tokens) && n > 0 {
			return n, nil
		}
	}
	return 0, ErrCantParse
}

// ExtractQuantities extracts all candidate quantity integers from s.
func ExtractQuantities(s string) []int64 {
	var out []int64
	addUnique := func(v int64) {
		if v <= 0 {
			return
		}
		for _, existing := range out {
			if existing == v {
				return
			}
		}
		out = append(out, v)
	}

	// Dozen phrases ("a dozen", "two dozen")
	for _, m := range reDozen.FindAllStringSubmatch(s, -1) {
		multWord := strings.ToLower(m[1])
		var mult int64
		if multWord == "a" || multWord == "an" {
			mult = 1
		} else if v, ok := smallWords[multWord]; ok {
			mult = v
		} else if v, err := strconv.ParseInt(multWord, 10, 64); err == nil {
			mult = v
		}
		if mult > 0 {
			addUnique(mult * 12)
		}
	}

	// Digit / k quantities followed by unit nouns ("40 boxes", "2.5k boxes", "60 units")
	for _, m := range reUnitQuant.FindAllStringSubmatch(s, -1) {
		raw := m[1]
		if km := reKNumber.FindStringSubmatch(raw); km != nil {
			if v, err := kPartsToInt(km[1], km[2]); err == nil {
				addUnique(v)
			}
		} else {
			clean := strings.ReplaceAll(raw, ",", "")
			if v, err := strconv.ParseInt(clean, 10, 64); err == nil {
				addUnique(v)
			}
		}
	}

	// Number words followed by unit nouns ("fifty boxes", "forty units", "ten boxes")
	tokens := tokenizeWords(strings.ToLower(s))
	for i := 0; i < len(tokens); i++ {
		if isQuantityNoun(tokens[i]) && i > 0 {
			if n, ok := parseTrailingNumberWords(tokens[:i]); ok && n > 0 {
				addUnique(n)
			}
		}
	}

	return out
}

func isQuantityNoun(w string) bool {
	switch w {
	case "box", "boxes", "unit", "units", "tin", "tins", "jar", "jars",
		"bottle", "bottles", "bowl", "bowls", "mug", "mugs", "candle", "candles",
		"planter", "planters", "blanket", "blankets", "teapot", "teapots",
		"napkin", "napkins", "plate", "plates", "set", "sets", "case", "cases",
		"pack", "packs", "item", "items", "piece", "pieces", "carton", "cartons":
		return true
	default:
		return false
	}
}

func kPartsToInt(wholeStr, fracStr string) (int64, error) {
	whole, err := strconv.ParseInt(wholeStr, 10, 64)
	if err != nil || whole < 0 {
		return 0, ErrCantParse
	}
	val := whole * 1000
	switch len(fracStr) {
	case 0:
	case 1:
		d, err := strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, ErrCantParse
		}
		val += d * 100
	case 2:
		d, err := strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, ErrCantParse
		}
		val += d * 10
	case 3:
		d, err := strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, ErrCantParse
		}
		val += d
	default:
		return 0, ErrCantParse
	}
	if val <= 0 {
		return 0, ErrCantParse
	}
	return val, nil
}

// tokenizeWords splits lowercased text into alphabetic/alphanumeric word tokens,
// splitting hyphens so "forty-two" becomes ["forty", "two"].
func tokenizeWords(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseTrailingNumberWords parses a contiguous run of number words at the end of tokens.
func parseTrailingNumberWords(tokens []string) (int64, bool) {
	start := len(tokens)
	for start > 0 {
		w := tokens[start-1]
		if _, ok := smallWords[w]; ok {
			start--
		} else if _, ok := tensWords[w]; ok {
			start--
		} else if w == "hundred" || w == "thousand" || w == "and" {
			start--
		} else if (w == "a" || w == "an") && start < len(tokens) && (tokens[start] == "hundred" || tokens[start] == "thousand") {
			start--
		} else {
			break
		}
	}
	// Trim leading "and"
	for start < len(tokens) && tokens[start] == "and" {
		start++
	}
	if start >= len(tokens) {
		return 0, false
	}
	val, consumed := parseLeadingNumberWords(tokens[start:])
	if consumed != len(tokens)-start {
		return 0, false
	}
	return val, true
}

// parseLeadingNumberWords parses number words starting at tokens[0].
func parseLeadingNumberWords(tokens []string) (int64, int) {
	var total int64
	var current int64
	consumed := 0
	for i := 0; i < len(tokens); i++ {
		w := tokens[i]
		if w == "and" && consumed > 0 {
			consumed++
			continue
		}
		if (w == "a" || w == "an") && i+1 < len(tokens) && (tokens[i+1] == "hundred" || tokens[i+1] == "thousand") {
			current += 1
			consumed++
			continue
		}
		if v, ok := smallWords[w]; ok {
			current += v
			consumed++
		} else if v, ok := tensWords[w]; ok {
			current += v
			consumed++
		} else if w == "hundred" {
			if current == 0 {
				current = 1
			}
			current *= 100
			consumed++
		} else if w == "thousand" {
			if current == 0 {
				current = 1
			}
			total += current * 1000
			current = 0
			consumed++
		} else {
			break
		}
	}
	if consumed == 0 {
		return 0, 0
	}
	return total + current, consumed
}
