package act

import (
	"context"
	"errors"
	"strings"

	"margincheck/internal/agenttools"
	"margincheck/internal/paypal"
)

// PayPalTools is the real Tools: create_invoice through PayPal's Agent Toolkit (decision 0021), the due date
// and the read-back through our REST client. Sandbox only (the REST client refuses any other base URL).
type PayPalTools struct {
	Client  *paypal.Client
	Factory agenttools.CommandFactory // nil = the pinned @paypal/mcp command
	Secrets []string                  // redacted from every error (the emails, for example)
}

// CreateInvoice finds the draft that already carries this invoice number, or creates it through the toolkit.
// Finding it first is what makes a repeat safe: PayPal ignores PayPal-Request-Id on invoice create (decision 0017).
func (t PayPalTools) CreateInvoice(ctx context.Context, a CreateInvoiceArgs) (string, bool, error) {
	if inv, err := t.Client.SearchInvoiceByNumber(ctx, "find-"+a.InvoiceNumber, a.InvoiceNumber); err == nil {
		if inv.Detail.InvoiceNumber == a.InvoiceNumber {
			return inv.ID, true, nil
		}
	} else if !errors.Is(err, paypal.ErrInvoiceNotFound) {
		return "", false, err
	}

	token, err := t.Client.AccessToken(ctx, "act-"+a.InvoiceNumber)
	if err != nil {
		return "", false, err
	}
	item := map[string]any{
		"name":            a.ItemName,
		"quantity":        itoa(a.Quantity),
		"unit_amount":     map[string]any{"currency_code": a.Currency, "value": centsString(a.UnitPriceCents)},
		"unit_of_measure": "QUANTITY",
	}
	if a.DiscountBP > 0 {
		item["discount"] = map[string]any{"percent": bpString(a.DiscountBP)}
	}
	args := map[string]any{
		"currency_code":          a.Currency,
		"invoice_number":         a.InvoiceNumber,
		"invoice_date":           a.InvoiceDate,
		"invoicer_email_address": a.SellerEmail,
		"primary_recipients":     []map[string]any{{"billing_info": map[string]any{"email_address": a.BuyerEmail}}},
		"items":                  []map[string]any{item},
	}
	if a.ShippingCents > 0 {
		args["shipping_cost"] = centsString(a.ShippingCents)
	}
	secrets := append([]string{a.SellerEmail, a.BuyerEmail}, t.Secrets...)
	id, err := agenttools.CallCreateInvoice(ctx, t.Factory, token, args, secrets...)
	if err != nil && strings.Contains(strings.ToUpper(err.Error()), "DUPLICATE") {
		// Created by an earlier, interrupted run: find it instead of failing.
		if inv, ferr := t.Client.SearchInvoiceByNumber(ctx, "find2-"+a.InvoiceNumber, a.InvoiceNumber); ferr == nil {
			return inv.ID, true, nil
		}
	}
	return id, false, err
}

// SetDueDate sets the draft's due date through our REST client (the toolkit can't, step-2 finding).
func (t PayPalTools) SetDueDate(ctx context.Context, invoiceID string, a SetDueDateArgs) error {
	return t.Client.SetDueDate(ctx, "due-"+a.InvoiceNumber+"-"+a.DueDate, invoiceID, a.DueDate)
}

// ReadBack reads the draft from PayPal.
func (t PayPalTools) ReadBack(ctx context.Context, invoiceID string) (*paypal.Invoice, error) {
	return t.Client.GetInvoice(ctx, invoiceID)
}
