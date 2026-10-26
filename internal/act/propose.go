package act

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"margincheck/internal/gemini"
	"margincheck/internal/offer"
	"margincheck/internal/writer"
)

// PlannerPromptVersion names the planner's instructions; bump it whenever they change (S5).
const PlannerPromptVersion = "planner-v1"

const plannerInstruction = `You plan the PayPal actions for a seller's accepted deal. You have exactly two tools:
- paypal.create_invoice: creates a DRAFT invoice with one line item (quantity, unit_price_cents, discount_bp) and a shipping charge (shipping_cents).
- paypal.set_due_date: sets the draft's due date, due_in_days after the invoice date. Use it only when the choice has a due date.

Rules:
- Use only the values given in the choice. Never change, round or invent a number.
- Never plan any other tool. Nothing is ever sent; the seller approves the plan first.
- The explanation is one plain sentence for the seller, with no numbers in it.

Return JSON: {"calls": [{"tool", "quantity", "unit_price_cents", "discount_bp", "shipping_cents", "due_in_days"}], "explanation"}.
Set fields a tool does not use to 0.`

var plannerSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"calls": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool":             map[string]any{"type": "string"},
					"quantity":         map[string]any{"type": "integer"},
					"unit_price_cents": map[string]any{"type": "integer"},
					"discount_bp":      map[string]any{"type": "integer"},
					"shipping_cents":   map[string]any{"type": "integer"},
					"due_in_days":      map[string]any{"type": "integer"},
				},
				"required": []any{"tool", "quantity", "unit_price_cents", "discount_bp", "shipping_cents", "due_in_days"},
			},
		},
		"explanation": map[string]any{"type": "string"},
	},
	"required": []any{"calls", "explanation"},
}

// Generator is the model client (internal/gemini satisfies it).
type Generator interface {
	Generate(ctx context.Context, req gemini.Request) (gemini.Response, error)
}

// Proposal is the plan offered to the seller, with what happened to the AI's own plan.
type Proposal struct {
	Plan    Plan     `json:"plan"`
	Blocked []string `json:"blocked,omitempty"` // the interceptor's findings on the AI's plan, if it was blocked
	Note    string   `json:"note,omitempty"`
	Tokens  int      `json:"tokens"`
	Mode    string   `json:"mode,omitempty"`
}

// Propose asks the AI to plan the PayPal actions for the accepted choice. The AI sees only the choice's verified
// values (never the email, the addresses or the invoice number). Code fills the identifiers and the dates, and
// the interceptor compares the result with the plan the offer requires. A differing plan is blocked and the code
// plan is offered instead; so is the case where the AI is unavailable.
func Propose(ctx context.Context, gen Generator, model, dealID string, o offer.Option, p Parties, invoiceDay time.Time) Proposal {
	want := ExpectedPlan(dealID, o, p, invoiceDay)
	codePlan := clonePlan(want)
	codePlan.Explanation = codeExplanation(o)

	choice, _ := json.Marshal(map[string]any{
		"kind": o.Kind, "currency": o.Currency, "quantity": o.Quantity, "unit_price_cents": o.UnitPriceCents,
		"discount_bp": o.DiscountBP, "shipping_cents": o.ShippingChargeCents, "due_in_days": o.DueDays,
	})
	if gen == nil {
		return Proposal{Plan: codePlan, Note: "no planner model configured; plan by code"}
	}
	resp, err := gen.Generate(ctx, gemini.Request{
		Model: model, InstructionVersion: PlannerPromptVersion, SystemInstruction: plannerInstruction,
		UserText: "The accepted choice:\n" + string(choice), ResponseSchema: plannerSchema, MaxOutputTokens: 4096,
	})
	if err != nil {
		return Proposal{Plan: codePlan, Note: "the planner model was unavailable; plan by code"}
	}
	var raw struct {
		Calls []struct {
			Tool           string `json:"tool"`
			Quantity       int64  `json:"quantity"`
			UnitPriceCents int64  `json:"unit_price_cents"`
			DiscountBP     int64  `json:"discount_bp"`
			ShippingCents  int64  `json:"shipping_cents"`
			DueInDays      int64  `json:"due_in_days"`
		} `json:"calls"`
		Explanation string `json:"explanation"`
	}
	pr := Proposal{Tokens: resp.TotalTokens, Mode: resp.Source}
	if err := json.Unmarshal([]byte(resp.Text), &raw); err != nil {
		pr.Plan, pr.Note = codePlan, "the planner's answer was not the requested JSON; plan by code"
		return pr
	}

	// Build the AI's plan: its tools and numbers, with identifiers and dates filled by code.
	ai := Plan{DealID: dealID, Option: o.Kind, By: "ai", Explanation: strings.TrimSpace(raw.Explanation)}
	create := want.Calls[0].Create
	for _, c := range raw.Calls {
		switch c.Tool {
		case ToolCreateInvoice:
			args := *create
			args.Quantity, args.UnitPriceCents, args.DiscountBP, args.ShippingCents = c.Quantity, c.UnitPriceCents, c.DiscountBP, c.ShippingCents
			ai.Calls = append(ai.Calls, Call{Tool: c.Tool, Create: &args})
		case ToolSetDueDate:
			ai.Calls = append(ai.Calls, Call{Tool: c.Tool, DueDate: &SetDueDateArgs{
				InvoiceNumber: create.InvoiceNumber,
				DueDate:       invoiceDay.AddDate(0, 0, int(c.DueInDays)).Format("2006-01-02"),
			}})
		default:
			ai.Calls = append(ai.Calls, Call{Tool: c.Tool})
		}
	}
	if diffs := Intercept(ai, want); len(diffs) > 0 {
		pr.Plan, pr.Blocked = codePlan, diffs
		pr.Note = "the AI's plan was blocked by the interceptor; the plan below was built by code"
		return pr
	}
	if ai.Explanation == "" || writer.HasNumber(ai.Explanation) || writer.HasCurrencySign(ai.Explanation) || writer.HasPercentSign(ai.Explanation) {
		ai.Explanation = codeExplanation(o)
		pr.Note = "the AI's explanation stated numbers, so the code's explanation is shown"
	}
	pr.Plan = ai
	return pr
}

func codeExplanation(o offer.Option) string {
	if o.DueDays > 0 {
		return "Create a draft invoice through PayPal's Agent Toolkit for the accepted choice, then set its due date."
	}
	return "Create a draft invoice through PayPal's Agent Toolkit for the accepted choice."
}
