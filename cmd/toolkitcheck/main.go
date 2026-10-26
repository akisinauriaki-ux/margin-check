package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"margincheck/internal/agenttools"
	"margincheck/internal/paypal"
)

const (
	toolkitQuantityBoxes  int64 = 40
	toolkitUnitPriceCents int64 = 1500 // $15.00 per box in whole cents
	defaultToolkitReport        = "reports/step2-toolkit.txt"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	code := runToolkitProof(ctx, ".env", defaultToolkitReport, nil, nil, os.Stdout)
	if code != 0 {
		os.Exit(code)
	}
}

func runToolkitProof(
	ctx context.Context,
	dotenvPath, reportPath string,
	httpClient *http.Client,
	factory agenttools.CommandFactory,
	out io.Writer,
) int {
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
	saveReport := func(extra string) {
		if strings.TrimSpace(reportPath) == "" {
			return
		}
		_ = os.MkdirAll(filepath.Dir(reportPath), 0o755)
		content := buf.Bytes()
		if extra != "" {
			content = append(content, []byte("\n"+extra)...)
		}
		_ = os.WriteFile(reportPath, content, 0o644)
	}

	ppClient, err := paypal.NewClient(cfg.APIBase, cfg.ClientID, cfg.ClientSecret, httpClient)
	if err != nil {
		emit("FAIL toolkit step 0 (client init): " + err.Error())
		saveReport("")
		return 1
	}

	key, err := paypal.NewRequestID("mcp-inv")
	if err != nil {
		emit("FAIL toolkit step 1 (idempotency key): " + err.Error())
		saveReport("")
		return 1
	}

	tok, err := ppClient.AccessToken(ctx, "inspect-"+key)
	if err != nil {
		emit("FAIL toolkit step 1 (access token): " + err.Error())
		saveReport("")
		return 1
	}

	// 1. Inspect schema from pinned MCP server (@paypal/mcp@1.8.1)
	finding, err := agenttools.InspectCreateInvoiceTool(ctx, factory, tok)
	if err != nil {
		emit("FAIL toolkit step 1 (inspect schema): " + formatToolkitError(err))
		saveReport("")
		return 1
	}

	schemaJSON, _ := json.MarshalIndent(finding.CreateInvoiceSchema, "", "  ")

	emit(fmt.Sprintf("PASS toolkit step 1 (mcp server start & list_tools): connected to %s via Go SDK (github.com/modelcontextprotocol/go-sdk/mcp v1.8.0) with %s, exposed_tools=%v",
		agenttools.PinnedPayPalMCPPackage, agenttools.AllowedToolsArg, finding.ExposedTools))

	// 2. Run draft proof and verify PayPal REST read-back field by field (status DRAFT, currency, quantity, unit price, MC- invoice number)
	res, err := agenttools.RunDraftProof(ctx, ppClient, agenttools.DraftProofInput{
		IdempotencyKey: key,
		CurrencyCode:   "USD",
		SellerEmail:    cfg.SellerEmail,
		BuyerEmail:     cfg.BuyerEmail,
		ItemName:       "Wholesale Boxes",
		Quantity:       toolkitQuantityBoxes,
		UnitPriceCents: toolkitUnitPriceCents,
	}, factory)
	if err != nil {
		emit("FAIL toolkit step 2 (draft proof & read-back): " + formatToolkitError(err))
		saveReport(string(schemaJSON))
		return 1
	}

	// Print verified only for fields that match in the REST read-back
	var verifiedParts []string
	if res.ReadBack != nil {
		if res.ReadBack.StatusVerified {
			verifiedParts = append(verifiedParts, fmt.Sprintf("status=%s (verified)", res.ReadBack.VerifiedStatus))
		}
		if res.ReadBack.CurrencyVerified {
			verifiedParts = append(verifiedParts, fmt.Sprintf("currency=%s (verified)", res.ReadBack.VerifiedCurrency))
		}
		if res.ReadBack.InvoiceNumberVerified {
			verifiedParts = append(verifiedParts, fmt.Sprintf("invoice_number=%s (verified)", paypal.MaskID(res.ReadBack.VerifiedInvNum)))
		}
		if res.ReadBack.QuantityVerified {
			verifiedParts = append(verifiedParts, fmt.Sprintf("quantity=%d (verified)", res.ReadBack.VerifiedQuantity))
		}
		if res.ReadBack.UnitPriceVerified {
			verifiedParts = append(verifiedParts, fmt.Sprintf("unit_price_cents=%d (verified)", res.ReadBack.VerifiedUnitCents))
		}
	}

	emit(fmt.Sprintf("PASS toolkit step 2 (call create_invoice & verify REST read-back): invoice %s verified via GET /v2/invoicing/invoices/{id}: %s (never sent)",
		paypal.MaskID(res.CreatedInvoiceID), strings.Join(verifiedParts, ", ")))

	// 3. Schema capability finding for item discount and payment terms
	if finding.HasItemDiscount && !finding.HasPaymentTerms {
		emit("FINDING toolkit step 3 (schema inspection): item discount is SUPPORTED (items[].discount.percent / items[].discount.amount); payment terms is MISSING (no payment_term, due_date, or term_type in create_invoice input schema)")
		emit("STOP toolkit step 4 (brief Part C.4): create_invoice cannot set payment terms or due_date; stopping run cleanly with no workaround")
	} else if finding.HasItemDiscount && finding.HasPaymentTerms {
		emit("PASS toolkit step 3 (schema inspection): both item discount and payment terms are supported in create_invoice schema")
	} else {
		emit(fmt.Sprintf("FINDING toolkit step 3 (schema inspection): missing required field %s; stopping cleanly", finding.MissingRequiredField))
	}

	extraReport := fmt.Sprintf("=== Pinned MCP Server: %s ===\n=== Tool: %s Input Schema ===\n%s\n",
		agenttools.PinnedPayPalMCPPackage, agenttools.CreateInvoiceToolName, string(schemaJSON))
	saveReport(extraReport)
	return 0
}

func formatToolkitError(err error) string {
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
