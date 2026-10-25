package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"margincheck/internal/paypal"
)

const (
	sampleQuantityBoxes  int64 = 40
	sampleUnitPriceCents int64 = 1500 // $15.00 per box in whole cents
	sampleDiscountPct    int64 = 10
	defaultReportPath          = "reports/day1-sandbox.txt"
)

func main() {
	code := run(context.Background(), ".env", defaultReportPath, nil, os.Stdout)
	if code != 0 {
		os.Exit(code)
	}
}

func run(ctx context.Context, dotenvPath, reportPath string, httpClient *http.Client, out io.Writer) int {
	cfg, ready, err := paypal.LoadSandboxConfig(dotenvPath)
	if err != nil {
		fmt.Fprintln(out, "FAIL config:", err.Error())
		return 1
	}
	if !ready {
		fmt.Fprintln(out, paypal.MissingEnvMessage)
		return 0
	}

	var buf bytes.Buffer
	emit := func(line string) {
		safe := scrubConfigValues(line, cfg)
		fmt.Fprintln(out, safe)
		fmt.Fprintln(&buf, safe)
	}
	saveReport := func() {
		if strings.TrimSpace(reportPath) == "" {
			return
		}
		_ = os.MkdirAll(filepath.Dir(reportPath), 0o755)
		_ = os.WriteFile(reportPath, buf.Bytes(), 0o644)
	}

	client, err := paypal.NewClient(cfg.APIBase, cfg.ClientID, cfg.ClientSecret, httpClient)
	if err != nil {
		emit("FAIL step 0 (client init): " + err.Error())
		saveReport()
		return 1
	}

	tokReqID, err := paypal.NewRequestID("sbx-tok")
	if err != nil {
		emit("FAIL step 1 (oauth token): " + err.Error())
		saveReport()
		return 1
	}
	if err := client.EnsureToken(ctx, tokReqID); err != nil {
		emit(fmt.Sprintf("FAIL step 1 (oauth token): %s", formatStepError(err)))
		saveReport()
		return 1
	}
	emit(fmt.Sprintf("PASS step 1 (oauth token): token cached in memory (req %s)", paypal.MaskID(tokReqID)))

	createReqID, err := paypal.NewRequestID("sbx-inv")
	if err != nil {
		emit("FAIL step 2 (create draft invoice): " + err.Error())
		saveReport()
		return 1
	}

	draftInput := paypal.DraftInvoiceInput{
		CurrencyCode:    "USD",
		SellerEmail:     cfg.SellerEmail,
		BuyerEmail:      cfg.BuyerEmail,
		PaymentTermType: paypal.PaymentTermNet30,
		Items: []paypal.InvoiceItemInput{
			{
				Name:           "Wholesale Boxes",
				Quantity:       sampleQuantityBoxes,
				UnitPriceCents: sampleUnitPriceCents,
			},
		},
	}

	inv, err := client.CreateDraftInvoice(ctx, createReqID, draftInput)
	if err != nil {
		emit(fmt.Sprintf("FAIL step 2 (create draft invoice): %s", formatStepError(err)))
		saveReport()
		return 1
	}
	if inv.Status != "" && !strings.EqualFold(inv.Status, "DRAFT") {
		emit(fmt.Sprintf("FAIL step 2 (create draft invoice): expected DRAFT status for %s, got %s", paypal.MaskID(inv.ID), inv.Status))
		saveReport()
		return 1
	}
	emit(fmt.Sprintf("PASS step 2 (create draft invoice): invoice %s status=DRAFT qty=%d unit_cents=%d currency=USD (req %s)",
		paypal.MaskID(inv.ID), sampleQuantityBoxes, sampleUnitPriceCents, paypal.MaskID(createReqID)))

	ruleReqID, err := paypal.NewRequestID("sbx-rule")
	if err != nil {
		emit("FAIL step 3 (early_payment_discount rule): " + err.Error())
		saveReport()
		return 1
	}
	endDate := computeSevenDayEndDate(inv.Detail.InvoiceDate)
	_, err = client.CreateEarlyPaymentDiscountRule(ctx, ruleReqID, paypal.EarlyPaymentDiscountInput{
		InvoiceID:       inv.ID,
		Percent:         sampleDiscountPct,
		ExpiryCondition: paypal.ExpirySevenDaysAfterIssueDate,
		EndDate:         endDate,
	})
	if err != nil {
		emit(fmt.Sprintf("FAIL step 3 (early_payment_discount rule): %s", formatStepError(err)))
		saveReport()
		return 1
	}
	emit(fmt.Sprintf("PASS step 3 (early_payment_discount rule): %d%% within 7 days attached to invoice %s (req %s)",
		sampleDiscountPct, paypal.MaskID(inv.ID), paypal.MaskID(ruleReqID)))

	gotInv, err := client.GetInvoice(ctx, inv.ID)
	if err != nil {
		emit(fmt.Sprintf("FAIL step 4 (get invoice & confirm rule): %s", formatStepError(err)))
		saveReport()
		return 1
	}
	if !gotInv.HasEarlyPaymentDiscount() {
		emit(fmt.Sprintf("FAIL step 4 (get invoice & confirm rule): invoice %s missing early_payment_discount rule", paypal.MaskID(inv.ID)))
		saveReport()
		return 1
	}
	emit(fmt.Sprintf("PASS step 4 (get invoice & confirm rule): invoice %s status=%s early_payment_discount present",
		paypal.MaskID(gotInv.ID), strings.ToUpper(gotInv.Status)))

	// Capture baseline count right before repeating Step 2 with the same PayPal-Request-Id.
	beforeList, err := client.ListInvoices(ctx, 1, 100, true)
	if err != nil {
		emit(fmt.Sprintf("FAIL step 5 (pre-repeat list baseline): %s", formatStepError(err)))
		saveReport()
		return 1
	}

	repeatInv, err := client.CreateDraftInvoice(ctx, createReqID, draftInput)
	if err != nil {
		emit(fmt.Sprintf("FAIL step 5 (repeat create with same PayPal-Request-Id): %s", formatStepError(err)))
		saveReport()
		return 1
	}
	if repeatInv.ID != inv.ID {
		emit(fmt.Sprintf("FAIL step 5 (repeat create with same PayPal-Request-Id): duplicate created (%s != %s)",
			paypal.MaskID(repeatInv.ID), paypal.MaskID(inv.ID)))
		saveReport()
		return 1
	}
	emit(fmt.Sprintf("PASS step 5 (repeat create with same PayPal-Request-Id): same invoice %s returned (req %s)",
		paypal.MaskID(repeatInv.ID), paypal.MaskID(createReqID)))

	afterList, err := client.ListInvoices(ctx, 1, 100, true)
	if err != nil {
		emit(fmt.Sprintf("FAIL step 6 (list invoices count check): %s", formatStepError(err)))
		saveReport()
		return 1
	}
	if afterList.TotalItems != beforeList.TotalItems {
		emit(fmt.Sprintf("FAIL step 6 (list invoices count check): total_items grew from %d to %d",
			beforeList.TotalItems, afterList.TotalItems))
		saveReport()
		return 1
	}
	emit(fmt.Sprintf("PASS step 6 (list invoices count check): total_items=%d unchanged after repeat",
		afterList.TotalItems))

	saveReport()
	return 0
}

func computeSevenDayEndDate(invoiceDate string) string {
	base := time.Now().UTC()
	if trimmed := strings.TrimSpace(invoiceDate); trimmed != "" {
		if parsed, err := time.Parse("2006-01-02", trimmed); err == nil {
			base = parsed
		}
	}
	return base.AddDate(0, 0, 7).Format("2006-01-02")
}

func formatStepError(err error) string {
	var apiErr *paypal.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Issue != "" {
			return fmt.Sprintf("status=%d error_name=%s issue=%s", apiErr.StatusCode, apiErr.Name, apiErr.Issue)
		}
		return fmt.Sprintf("status=%d error_name=%s", apiErr.StatusCode, apiErr.Name)
	}
	return err.Error()
}

func scrubConfigValues(s string, cfg *paypal.SandboxConfig) string {
	if cfg == nil {
		return s
	}
	out := s
	for _, secret := range []string{cfg.ClientSecret, cfg.ClientID, cfg.SellerEmail, cfg.BuyerEmail} {
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "[REDACTED]")
		}
	}
	return out
}
