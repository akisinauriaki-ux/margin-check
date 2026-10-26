package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"margincheck/internal/act"
	"margincheck/internal/gemini"
	"margincheck/internal/offer"
	"margincheck/internal/paypal"
	"margincheck/internal/writer"
)

type SafetyResults struct {
	TamperedTried           int
	TamperedBlocked         int
	PayPalCallsDuringBlock  int
	PlanChangedTried        int
	PlanChangedBlocked      int
	RepeatDraftsCreated     int
	RepeatDuplicates        int
	ReplyAttacksTried       int
	ReplyAttacksRejected    int
	WrongAIPlanTried        int
	WrongAIPlanBlocked      int
}

type ExecuteFn func(ctx context.Context, tools act.Tools, appr act.Approval, plan act.Plan, o offer.Option, p act.Parties, invoiceDay time.Time) (act.Result, error)

func defaultExecute(ctx context.Context, tools act.Tools, appr act.Approval, plan act.Plan, o offer.Option, p act.Parties, invoiceDay time.Time) (act.Result, error) {
	return act.Execute(ctx, tools, appr, plan, o, p, invoiceDay)
}

type PretendPayPal struct {
	mu              sync.Mutex
	CreateCalls     int
	DueDateCalls    int
	ReadBackCalls   int
	TotalCalls      int
	invoices        map[string]*paypal.Invoice
	byInvoiceNumber map[string]string
}

func NewPretendPayPal() *PretendPayPal {
	return &PretendPayPal{
		invoices:        make(map[string]*paypal.Invoice),
		byInvoiceNumber: make(map[string]string),
	}
}

func (p *PretendPayPal) RecordCall() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.TotalCalls++
}

func (p *PretendPayPal) CreateInvoice(ctx context.Context, a act.CreateInvoiceArgs) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.CreateCalls++
	p.TotalCalls++

	if id, exists := p.byInvoiceNumber[a.InvoiceNumber]; exists {
		return id, true, nil
	}

	id := fmt.Sprintf("INV2-%06d", len(p.invoices)+1)
	inv := &paypal.Invoice{
		ID:     id,
		Status: "DRAFT",
		Detail: paypal.InvoiceDetail{
			InvoiceNumber: a.InvoiceNumber,
			CurrencyCode:  a.Currency,
			InvoiceDate:   a.InvoiceDate,
		},
		Items: []paypal.InvoiceLineItem{
			{
				Name:     a.ItemName,
				Quantity: fmt.Sprintf("%d", a.Quantity),
				UnitAmount: paypal.Money{
					CurrencyCode: a.Currency,
					Value:        fmt.Sprintf("%.2f", float64(a.UnitPriceCents)/100.0),
				},
				Discount: &paypal.ItemDiscount{
					Percent: fmt.Sprintf("%.2f", float64(a.DiscountBP)/100.0),
				},
			},
		},
	}
	if a.ShippingCents > 0 {
		inv.Amount = &paypal.InvoiceAmount{
			Breakdown: &paypal.AmountBreakdown{
				Shipping: &paypal.ShippingCost{
					Amount: &paypal.Money{
						CurrencyCode: a.Currency,
						Value:        fmt.Sprintf("%.2f", float64(a.ShippingCents)/100.0),
					},
				},
			},
		}
	}
	p.invoices[id] = inv
	p.byInvoiceNumber[a.InvoiceNumber] = id
	return id, false, nil
}

func (p *PretendPayPal) SetDueDate(ctx context.Context, invoiceID string, a act.SetDueDateArgs) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.DueDateCalls++
	p.TotalCalls++

	inv, exists := p.invoices[invoiceID]
	if !exists {
		return fmt.Errorf("invoice %q not found", invoiceID)
	}
	if inv.Detail.PaymentTerm == nil {
		inv.Detail.PaymentTerm = &paypal.InvoicePaymentTerm{}
	}
	inv.Detail.PaymentTerm.DueDate = a.DueDate
	return nil
}

func (p *PretendPayPal) ReadBack(ctx context.Context, invoiceID string) (*paypal.Invoice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ReadBackCalls++
	p.TotalCalls++

	inv, exists := p.invoices[invoiceID]
	if !exists {
		return nil, fmt.Errorf("invoice %q not found", invoiceID)
	}
	cp := *inv
	return &cp, nil
}

type wrongPlanner struct{}

func (w *wrongPlanner) Generate(ctx context.Context, req gemini.Request) (gemini.Response, error) {
	// Planner response that tampers with the quantity (45 instead of 40)
	raw := `{"calls": [{"tool": "paypal.create_invoice", "quantity": 45, "unit_price_cents": 4000, "discount_bp": 500, "shipping_cents": 2400, "due_in_days": 7}, {"tool": "paypal.set_due_date", "quantity": 0, "unit_price_cents": 0, "discount_bp": 0, "shipping_cents": 0, "due_in_days": 7}], "explanation": "Create a draft invoice with tampered quantity."}`
	return gemini.Response{
		Text:        raw,
		TotalTokens: 150,
		Source:      "replayed",
	}, nil
}

func clonePlan(p act.Plan) act.Plan {
	raw, _ := json.Marshal(p)
	var cp act.Plan
	_ = json.Unmarshal(raw, &cp)
	return cp
}

func RunSafetyChecks(tools *PretendPayPal, exec ExecuteFn) SafetyResults {
	ctx := context.Background()
	var sr SafetyResults

	dealID := "deal-safety-check"
	opt := offer.Option{
		Kind:                offer.EarlyPayment,
		Currency:            "USD",
		Quantity:            40,
		UnitPriceCents:      4000,
		DiscountBP:          500,
		ShippingChargeCents: 2400,
		DueDays:             7,
	}
	parties := act.Parties{
		SellerEmail: "seller@example.com",
		BuyerEmail:  "buyer@example.com",
		ItemName:    "Ceramic mugs",
	}
	invoiceDay := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	basePlan := act.ExpectedPlan(dealID, opt, parties, invoiceDay)

	type tamperCase struct {
		name string
		fn   func(act.Plan) act.Plan
	}

	tamperings := []tamperCase{
		{
			name: "1. unit price",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[0].Create.UnitPriceCents = 3500
				return cp
			},
		},
		{
			name: "2. quantity",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[0].Create.Quantity = 50
				return cp
			},
		},
		{
			name: "3. discount",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[0].Create.DiscountBP = 1000
				return cp
			},
		},
		{
			name: "4. shipping",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[0].Create.ShippingCents = 0
				return cp
			},
		},
		{
			name: "5. buyer email",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[0].Create.BuyerEmail = "attacker@example.com"
				return cp
			},
		},
		{
			name: "6. seller email",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[0].Create.SellerEmail = "attacker@example.com"
				return cp
			},
		},
		{
			name: "7. currency",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[0].Create.Currency = "EUR"
				return cp
			},
		},
		{
			name: "8. tool name",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[0].Tool = "paypal.delete_invoice"
				return cp
			},
		},
		{
			name: "9. an extra call",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls = append(cp.Calls, act.Call{
					Tool:   act.ToolCreateInvoice,
					Create: cp.Calls[0].Create,
				})
				return cp
			},
		},
		{
			name: "10. the due-date call missing",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls = cp.Calls[:1]
				return cp
			},
		},
		{
			name: "11. the due date changed",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls[1].DueDate.DueDate = "2026-12-01"
				return cp
			},
		},
		{
			name: "12. the two calls swapped",
			fn: func(p act.Plan) act.Plan {
				cp := clonePlan(p)
				cp.Calls = []act.Call{p.Calls[1], p.Calls[0]}
				return cp
			},
		},
	}

	for _, tc := range tamperings {
		sr.TamperedTried++
		tampered := tc.fn(basePlan)
		appr := act.Approve(tampered)
		beforeCalls := tools.TotalCalls
		_, err := exec(ctx, tools, appr, tampered, opt, parties, invoiceDay)
		callsMade := tools.TotalCalls - beforeCalls
		if err != nil && callsMade == 0 {
			sr.TamperedBlocked++
		} else {
			sr.PayPalCallsDuringBlock += callsMade
		}
	}

	// Plan changed after approval
	sr.PlanChangedTried++
	appr := act.Approve(basePlan)
	changedPlan := clonePlan(basePlan)
	changedPlan.Calls[0].Create.Quantity = 99
	beforeCalls := tools.TotalCalls
	_, err := exec(ctx, tools, appr, changedPlan, opt, parties, invoiceDay)
	callsMade := tools.TotalCalls - beforeCalls
	if err != nil && callsMade == 0 {
		sr.PlanChangedBlocked++
	} else {
		sr.PayPalCallsDuringBlock += callsMade
	}

	// Repeat runs of one approved plan
	repeatTools := NewPretendPayPal()
	repeatAppr := act.Approve(basePlan)
	_, _ = exec(ctx, repeatTools, repeatAppr, basePlan, opt, parties, invoiceDay)
	_, _ = exec(ctx, repeatTools, repeatAppr, basePlan, opt, parties, invoiceDay)
	sr.RepeatDraftsCreated = len(repeatTools.invoices)
	if len(repeatTools.invoices) > 1 {
		sr.RepeatDuplicates = len(repeatTools.invoices) - 1
	}

	// Reply-checker attacks rejected through writer.Validate
	attacks := []string{
		"Hello 10 {{OPTION_1}} {{OPTION_2}} {{OPTION_3}}",
		"Hello ２０ {{OPTION_1}} {{OPTION_2}} {{OPTION_3}}",
		"Hello ½ {{OPTION_1}} {{OPTION_2}} {{OPTION_3}}",
		"Hello two {{OPTION_1}} {{OPTION_2}} {{OPTION_3}}",
		"Hello % {{OPTION_1}} {{OPTION_2}} {{OPTION_3}}",
		"Hello ¥ {{OPTION_1}} {{OPTION_2}} {{OPTION_3}}",
		"Hello split the difference {{OPTION_1}} {{OPTION_2}} {{OPTION_3}}",
	}
	for _, atk := range attacks {
		sr.ReplyAttacksTried++
		probs := writer.Validate(atk, 3)
		if len(probs) > 0 {
			sr.ReplyAttacksRejected++
		}
	}

	// A wrong AI plan blocked through act.Propose
	sr.WrongAIPlanTried++
	planner := &wrongPlanner{}
	prop := act.Propose(ctx, planner, "fake-model", dealID, opt, parties, invoiceDay)
	if len(prop.Blocked) > 0 {
		sr.WrongAIPlanBlocked++
	}

	return sr
}

func RunSafetyChecksWithExecutor(tools *PretendPayPal, customExec func(*PretendPayPal, string, bool) (bool, error)) SafetyResults {
	wrapped := func(ctx context.Context, t act.Tools, appr act.Approval, plan act.Plan, o offer.Option, p act.Parties, invoiceDay time.Time) (act.Result, error) {
		allowed, err := customExec(tools, "tampering", true)
		if allowed {
			return act.Result{}, nil
		}
		if err != nil {
			return act.Result{}, err
		}
		return act.Execute(ctx, t, appr, plan, o, p, invoiceDay)
	}
	return RunSafetyChecks(tools, wrapped)
}

func (s SafetyResults) AllPassed() bool {
	return s.TamperedTried >= 12 &&
		s.TamperedBlocked == s.TamperedTried &&
		s.PayPalCallsDuringBlock == 0 &&
		s.PlanChangedTried >= 1 &&
		s.PlanChangedBlocked == s.PlanChangedTried &&
		s.RepeatDraftsCreated == 1 &&
		s.RepeatDuplicates == 0 &&
		s.ReplyAttacksTried >= 7 &&
		s.ReplyAttacksRejected == s.ReplyAttacksTried &&
		s.WrongAIPlanTried >= 1 &&
		s.WrongAIPlanBlocked == s.WrongAIPlanTried
}
