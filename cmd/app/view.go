package main

import (
	"fmt"
	"strings"

	"margincheck/internal/flow"
	"margincheck/internal/offer"
	"margincheck/internal/quote"
	"margincheck/internal/reader"
)

// Line is one line of the pasted thread, marked when it gives something away.
type Line struct {
	Text string
	Mark bool
	Why  string
}

type giveaway struct {
	quote string
	why   string
}

// highlightLines marks the thread's lines that carry a giveaway the reader found and quoted. The money effect
// comes from the profit engine's waterfall line, never from the AI.
func highlightLines(d *flow.Deal) []Line {
	r := d.Reading
	cur := "USD"
	if d.Check.Deal != nil {
		cur = d.Check.Deal.Currency
	}
	leak := func(name string) string {
		if d.Check.Profit == nil {
			return ""
		}
		for _, l := range d.Check.Profit.Leaks {
			if l.Name == name && l.Cents > 0 {
				return " (costs you " + offer.FormatMoney(l.Cents, cur) + ")"
			}
		}
		return ""
	}
	var gs []giveaway
	if r.DiscountPercent.Sure && r.DiscountPercent.Value > 0 {
		gs = append(gs, giveaway{r.DiscountPercent.Quote, fmt.Sprintf("a %d%% discount%s", r.DiscountPercent.Value, leak("order_size_discount"))})
	}
	if r.FreeShipping.Sure && r.FreeShipping.Value {
		gs = append(gs, giveaway{r.FreeShipping.Quote, "free shipping" + leak("free_shipping")})
	}
	if r.PaymentDays.Sure && r.PaymentDays.Value > 0 {
		gs = append(gs, giveaway{r.PaymentDays.Quote, fmt.Sprintf("%d days to pay%s", r.PaymentDays.Value, leak("payment_days"))})
	}
	if r.RebatePercent.Sure && r.RebatePercent.Value > 0 {
		gs = append(gs, giveaway{r.RebatePercent.Quote, fmt.Sprintf("a %d%% rebate%s", r.RebatePercent.Value, leak("rebate"))})
	}
	if r.PriceIncreaseDiscounted.Sure && r.PriceIncreaseDiscounted.Value {
		gs = append(gs, giveaway{r.PriceIncreaseDiscounted.Quote, "a discount on your price increase"})
	}

	var out []Line
	for _, raw := range strings.Split(strings.ReplaceAll(d.Thread, "\r\n", "\n"), "\n") {
		ln := Line{Text: raw}
		n := quote.NormalizeText(raw)
		for _, g := range gs {
			q := quote.NormalizeText(g.quote)
			if n == "" || q == "" {
				continue
			}
			if strings.Contains(n, q) || (len(n) > 15 && strings.Contains(q, n)) {
				ln.Mark = true
				if ln.Why != "" {
					ln.Why += "; "
				}
				ln.Why += g.why
			}
		}
		out = append(out, ln)
	}
	return out
}

// Row is one value in "how it decided".
type Row struct {
	Field, Value, Status, Reason, Quote string
}

func fieldRows(r reader.CheckResult) []Row {
	intRow := func(name string, f reader.IntFieldResult) Row {
		v := fmt.Sprint(f.Value)
		if name == "unit price" {
			v = fmt.Sprintf("%d.%02d", f.Value/100, f.Value%100)
		}
		return row(name, v, f.Sure, f.Unknown, f.Reason, f.Quote)
	}
	boolRow := func(name string, f reader.BoolFieldResult) Row {
		return row(name, fmt.Sprint(f.Value), f.Sure, f.Unknown, f.Reason, f.Quote)
	}
	rows := []Row{
		intRow("quantity", r.Quantity),
		intRow("unit price", r.UnitPriceCents),
		intRow("discount %", r.DiscountPercent),
		boolRow("free shipping", r.FreeShipping),
		intRow("days to pay", r.PaymentDays),
		intRow("rebate %", r.RebatePercent),
		boolRow("discount on a price increase", r.PriceIncreaseDiscounted),
	}
	if r.AcceptedOption.Value != "" || r.AcceptedOption.Unknown {
		a := r.AcceptedOption
		rows = append(rows, row("accepted choice", a.Value, a.Sure, a.Unknown, a.Reason, a.Quote))
	}
	return rows
}

func row(name, value string, sure, unknown bool, reason, q string) Row {
	status := "sure"
	if !sure || unknown {
		status, value = "ask", "?"
	}
	if q == "" {
		q = "(nothing said, so nothing given)"
	}
	return Row{Field: name, Value: value, Status: status, Reason: strings.ReplaceAll(reason, "_", " "), Quote: q}
}
