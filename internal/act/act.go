package act

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"margincheck/internal/offer"
	"margincheck/internal/paypal"
)

// The only two tools the agent has (decision 0021). Nothing else can be planned or run.
const (
	ToolCreateInvoice = "paypal.create_invoice" // PayPal's Agent Toolkit (@paypal/mcp create_invoice)
	ToolSetDueDate    = "paypal.set_due_date"   // our REST client: full update of the draft's payment term
)

var (
	// ErrFingerprintMismatch means the plan about to run is not the plan the seller approved.
	ErrFingerprintMismatch = errors.New("act: the plan differs from the approved plan; nothing was run")
	// ErrBlocked means the plan differs from what the chosen offer requires (the interceptor).
	ErrBlocked = errors.New("act: the plan does not match the offer; nothing was run")
)

// Parties are the non-money facts of the draft, from the seller's settings (never from the email or the AI).
type Parties struct {
	SellerEmail string `json:"seller_email"`
	BuyerEmail  string `json:"buyer_email"`
	ItemName    string `json:"item_name"`
}

// CreateInvoiceArgs are the exact arguments of one create_invoice call.
type CreateInvoiceArgs struct {
	InvoiceNumber  string `json:"invoice_number"`
	Currency       string `json:"currency_code"`
	SellerEmail    string `json:"invoicer_email"`
	BuyerEmail     string `json:"recipient_email"`
	ItemName       string `json:"item_name"`
	Quantity       int64  `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	DiscountBP     int64  `json:"discount_bp"`
	ShippingCents  int64  `json:"shipping_cents"`
	InvoiceDate    string `json:"invoice_date"`
}

// SetDueDateArgs are the exact arguments of one set_due_date call.
type SetDueDateArgs struct {
	InvoiceNumber string `json:"invoice_number"`
	DueDate       string `json:"due_date"`
}

// Call is one planned tool call. Exactly one of Create and DueDate is set, matching Tool.
type Call struct {
	Tool    string             `json:"tool"`
	Create  *CreateInvoiceArgs `json:"create,omitempty"`
	DueDate *SetDueDateArgs    `json:"due_date,omitempty"`
}

// Plan is what the seller sees and approves. Only DealID, Option and Calls are executed and fingerprinted.
type Plan struct {
	DealID      string     `json:"deal_id"`
	Option      offer.Kind `json:"option"`
	Calls       []Call     `json:"calls"`
	Explanation string     `json:"explanation,omitempty"` // shown to the seller, never executed
	By          string     `json:"by,omitempty"`          // "ai" or "code"
}

// ExpectedPlan is the plan the chosen offer requires, built by code. It is the reference the interceptor
// compares every proposed plan against.
func ExpectedPlan(dealID string, o offer.Option, p Parties, invoiceDay time.Time) Plan {
	invNum, _ := paypal.DeriveInvoiceNumber(InvoiceKey(dealID, o.Kind))
	create := &CreateInvoiceArgs{
		InvoiceNumber:  invNum,
		Currency:       strings.ToUpper(o.Currency),
		SellerEmail:    p.SellerEmail,
		BuyerEmail:     p.BuyerEmail,
		ItemName:       p.ItemName,
		Quantity:       o.Quantity,
		UnitPriceCents: o.UnitPriceCents,
		DiscountBP:     o.DiscountBP,
		ShippingCents:  o.ShippingChargeCents,
		InvoiceDate:    invoiceDay.Format("2006-01-02"),
	}
	plan := Plan{DealID: dealID, Option: o.Kind, Calls: []Call{{Tool: ToolCreateInvoice, Create: create}}, By: "code"}
	if o.DueDays > 0 {
		plan.Calls = append(plan.Calls, Call{Tool: ToolSetDueDate, DueDate: &SetDueDateArgs{
			InvoiceNumber: invNum,
			DueDate:       invoiceDay.AddDate(0, 0, int(o.DueDays)).Format("2006-01-02"),
		}})
	}
	return plan
}

// InvoiceKey is the idempotency key of the draft for one deal and one choice (decision 0017).
func InvoiceKey(dealID string, k offer.Kind) string { return "mc-" + dealID + "-" + string(k) }

// Fingerprint is the SHA-256 of the canonical JSON of what will run: the deal, the choice and the exact calls
// in order. The explanation is left out because it is never executed.
func Fingerprint(p Plan) string {
	canon, _ := json.Marshal(struct {
		DealID string     `json:"deal_id"`
		Option offer.Kind `json:"option"`
		Calls  []Call     `json:"calls"`
	}{p.DealID, p.Option, p.Calls})
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// Intercept lists every difference between a proposed plan and the plan the offer requires. Any difference
// blocks the plan.
func Intercept(got, want Plan) []string {
	var diffs []string
	if got.DealID != want.DealID {
		diffs = append(diffs, fmt.Sprintf("deal %q, want %q", got.DealID, want.DealID))
	}
	if got.Option != want.Option {
		diffs = append(diffs, fmt.Sprintf("choice %q, want %q", got.Option, want.Option))
	}
	if len(got.Calls) != len(want.Calls) {
		diffs = append(diffs, fmt.Sprintf("%d calls, want %d", len(got.Calls), len(want.Calls)))
	}
	for i, g := range got.Calls {
		if g.Tool != ToolCreateInvoice && g.Tool != ToolSetDueDate {
			diffs = append(diffs, fmt.Sprintf("call %d: tool %q is not allowed", i+1, g.Tool))
			continue
		}
		if i >= len(want.Calls) {
			diffs = append(diffs, fmt.Sprintf("call %d (%s) is extra", i+1, g.Tool))
			continue
		}
		w := want.Calls[i]
		if g.Tool != w.Tool {
			diffs = append(diffs, fmt.Sprintf("call %d: tool %s, want %s", i+1, g.Tool, w.Tool))
			continue
		}
		gj, _ := json.Marshal(g)
		wj, _ := json.Marshal(w)
		if string(gj) != string(wj) {
			diffs = append(diffs, fieldDiffs(i+1, g, w)...)
		}
	}
	return diffs
}

func isEmailField(k string) bool {
	return strings.HasSuffix(k, "_email") || k == "email"
}

func maskEmail(e string) string {
	at := strings.Index(e, "@")
	if at < 1 {
		return "***"
	}
	return e[:1] + "***" + e[at:]
}

func fieldDiffs(n int, g, w Call) []string {
	gm, wm := map[string]any{}, map[string]any{}
	gj, _ := json.Marshal(g)
	wj, _ := json.Marshal(w)
	_ = json.Unmarshal(gj, &gm)
	_ = json.Unmarshal(wj, &wm)
	key := "create"
	if g.Tool == ToolSetDueDate {
		key = "due_date"
	}
	ga, _ := gm[key].(map[string]any)
	wa, _ := wm[key].(map[string]any)
	var diffs []string
	for k, wv := range wa {
		if fmt.Sprint(ga[k]) != fmt.Sprint(wv) {
			gvStr := fmt.Sprint(ga[k])
			wvStr := fmt.Sprint(wv)
			if isEmailField(k) {
				gvStr = maskEmail(gvStr)
				wvStr = maskEmail(wvStr)
			}
			diffs = append(diffs, fmt.Sprintf("call %d %s: %s is %s, the offer requires %s", n, g.Tool, k, gvStr, wvStr))
		}
	}
	for k := range ga {
		if _, ok := wa[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("call %d %s: unexpected argument %s", n, g.Tool, k))
		}
	}
	if len(diffs) == 0 {
		diffs = append(diffs, fmt.Sprintf("call %d %s differs from the offer", n, g.Tool))
	}
	return diffs
}

// Approval binds the seller's yes to one exact plan.
type Approval struct {
	Fingerprint string    `json:"fingerprint"`
	Plan        Plan      `json:"plan"`
	ApprovedAt  time.Time `json:"approved_at"`
}

// Approve records the seller's approval of exactly this plan.
func Approve(p Plan) Approval {
	return Approval{Fingerprint: Fingerprint(p), Plan: clonePlan(p), ApprovedAt: time.Now().UTC()}
}

// Tools is the agent's whole reach into PayPal: two tools, plus the read-back used to verify them.
type Tools interface {
	// CreateInvoice creates a DRAFT, or finds the draft that already carries this invoice number.
	CreateInvoice(ctx context.Context, a CreateInvoiceArgs) (invoiceID string, alreadyExisted bool, err error)
	// SetDueDate sets the draft's due date. It must be safe to repeat and must never send anything.
	SetDueDate(ctx context.Context, invoiceID string, a SetDueDateArgs) error
	ReadBack(ctx context.Context, invoiceID string) (*paypal.Invoice, error)
}

// ReadCheck is one field compared between the plan and PayPal's read-back.
type ReadCheck struct {
	Field string `json:"field"`
	Want  string `json:"want"`
	Got   string `json:"got"`
	OK    bool   `json:"ok"`
}

// Result is what happened, including a half-done run.
type Result struct {
	InvoiceID      string      `json:"invoice_id"`
	InvoiceNumber  string      `json:"invoice_number"`
	AlreadyExisted bool        `json:"already_existed"`
	DueDateSet     bool        `json:"due_date_set"`
	Status         string      `json:"status"`
	ReadBackOK     bool        `json:"read_back_ok"`
	Checks         []ReadCheck `json:"checks,omitempty"`
}

// Execute runs an approved plan. It refuses unless the plan's fingerprint equals the approval's, and unless the
// plan equals the one the offer requires. It runs the calls in order, then reads the draft back and compares.
func Execute(ctx context.Context, tools Tools, appr Approval, plan Plan, o offer.Option, p Parties, invoiceDay time.Time) (Result, error) {
	if Fingerprint(plan) != appr.Fingerprint {
		return Result{}, ErrFingerprintMismatch
	}
	if diffs := Intercept(plan, ExpectedPlan(plan.DealID, o, p, invoiceDay)); len(diffs) > 0 {
		return Result{}, fmt.Errorf("%w: %s", ErrBlocked, strings.Join(diffs, "; "))
	}

	var res Result
	var create *CreateInvoiceArgs
	var due *SetDueDateArgs
	for _, c := range plan.Calls {
		switch c.Tool {
		case ToolCreateInvoice:
			create = c.Create
			id, existed, err := tools.CreateInvoice(ctx, *c.Create)
			if err != nil {
				return res, fmt.Errorf("act: create_invoice: %w", err)
			}
			res.InvoiceID, res.InvoiceNumber, res.AlreadyExisted = id, c.Create.InvoiceNumber, existed
		case ToolSetDueDate:
			due = c.DueDate
			if res.InvoiceID == "" {
				return res, errors.New("act: set_due_date before a draft exists")
			}
			if err := tools.SetDueDate(ctx, res.InvoiceID, *c.DueDate); err != nil {
				return res, fmt.Errorf("act: the draft was created but its due date isn't set yet: %w", err)
			}
			res.DueDateSet = true
		}
	}

	inv, err := tools.ReadBack(ctx, res.InvoiceID)
	if err != nil {
		return res, fmt.Errorf("act: read-back: %w", err)
	}
	res.Status = strings.ToUpper(inv.Status)
	res.Checks = compare(inv, create, due)
	res.ReadBackOK = true
	for _, c := range res.Checks {
		if !c.OK {
			res.ReadBackOK = false
		}
	}
	if !res.ReadBackOK {
		return res, errors.New("act: the draft PayPal holds differs from the approved plan")
	}
	return res, nil
}

func compare(inv *paypal.Invoice, c *CreateInvoiceArgs, due *SetDueDateArgs) []ReadCheck {
	add := func(out []ReadCheck, field, want, got string) []ReadCheck {
		return append(out, ReadCheck{Field: field, Want: want, Got: got, OK: want == got})
	}
	var out []ReadCheck
	out = add(out, "status", "DRAFT", strings.ToUpper(inv.Status))
	out = add(out, "currency", c.Currency, strings.ToUpper(inv.Detail.CurrencyCode))
	out = add(out, "invoice_number", c.InvoiceNumber, inv.Detail.InvoiceNumber)
	var item paypal.InvoiceLineItem
	if len(inv.Items) == 1 {
		item = inv.Items[0]
	}
	out = add(out, "items", "1", itoa(int64(len(inv.Items))))
	out = add(out, "quantity", itoa(c.Quantity), trimDecimal(item.Quantity))
	out = add(out, "unit_price_cents", itoa(c.UnitPriceCents), readCents(item.UnitAmount.Value))
	gotBP := "0"
	if item.Discount != nil {
		gotBP = readBP(item.Discount.Percent)
	}
	out = add(out, "discount_bp", itoa(c.DiscountBP), gotBP)
	gotShip := "0"
	if inv.Amount != nil && inv.Amount.Breakdown != nil && inv.Amount.Breakdown.Shipping != nil && inv.Amount.Breakdown.Shipping.Amount != nil {
		gotShip = readCents(inv.Amount.Breakdown.Shipping.Amount.Value)
	}
	out = add(out, "shipping_cents", itoa(c.ShippingCents), gotShip)
	if due != nil {
		got := ""
		if inv.Detail.PaymentTerm != nil {
			got = inv.Detail.PaymentTerm.DueDate
		}
		out = add(out, "due_date", due.DueDate, got)
	}
	return out
}

func clonePlan(p Plan) Plan {
	raw, _ := json.Marshal(p)
	var c Plan
	_ = json.Unmarshal(raw, &c)
	return c
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// centsString writes cents as PayPal's decimal string, e.g. 4000 -> "40.00".
func centsString(c int64) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

// bpString writes basis points as PayPal's percent string, e.g. 150 -> "1.5".
func bpString(bp int64) string {
	return strings.TrimSuffix(offer.FormatPercent(bp), "%")
}

func trimDecimal(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// readCents reads a decimal string such as "40", "40.5" or "40.00" as whole cents, without floats. Anything
// else (a sign, extra non-zero decimals, any other character) reads as "unreadable", which matches no plan.
func readCents(s string) string { return readScaled(s, 2) }

// readBP reads a percent string such as "1.5" or "10.000" as basis points, the same way.
func readBP(s string) string { return readScaled(s, 2) }

func readScaled(s string, places int) string {
	if v, ok := parseScaled(s, places); ok {
		return itoa(v)
	}
	return "unreadable " + strconv.Quote(s)
}

// parseScaled reads digits with an optional decimal part as a whole number of 1/10^places units. It refuses
// signs, non-digits, non-zero digits beyond places, and values that would not fit in an int64.
func parseScaled(s string, places int) (int64, bool) {
	whole, frac, hasDot := strings.Cut(strings.TrimSpace(s), ".")
	if !allDigits(whole) || (hasDot && !allDigits(frac)) {
		return 0, false
	}
	if len(frac) > places {
		if strings.Trim(frac[places:], "0") != "" {
			return 0, false
		}
		frac = frac[:places]
	}
	for len(frac) < places {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	var f int64
	if places > 0 {
		if f, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return 0, false
		}
	}
	scale := int64(1)
	for i := 0; i < places; i++ {
		scale *= 10
	}
	if w > (math.MaxInt64-f)/scale {
		return 0, false
	}
	return w*scale + f, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
