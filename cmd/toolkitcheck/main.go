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

	"margincheck/internal/agenttools"
	"margincheck/internal/paypal"
)

const (
	toolkitQuantityBoxes  int64 = 40
	toolkitUnitPriceCents int64 = 1500 // $15.00 per box in whole cents
	defaultToolkitReport        = "reports/day1-toolkit.txt"
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
	saveReport := func() {
		if strings.TrimSpace(reportPath) == "" {
			return
		}
		_ = os.MkdirAll(filepath.Dir(reportPath), 0o755)
		_ = os.WriteFile(reportPath, buf.Bytes(), 0o644)
	}

	ppClient, err := paypal.NewClient(cfg.APIBase, cfg.ClientID, cfg.ClientSecret, httpClient)
	if err != nil {
		emit("FAIL toolkit step 0 (client init): " + err.Error())
		saveReport()
		return 1
	}

	key, err := paypal.NewRequestID("mcp-inv")
	if err != nil {
		emit("FAIL toolkit step 1 (idempotency key): " + err.Error())
		saveReport()
		return 1
	}

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
		emit("FAIL toolkit proof: " + formatToolkitError(err))
		saveReport()
		return 1
	}

	emit(fmt.Sprintf("PASS toolkit step 1 (mcp server start & list_tools): connected via Go SDK (github.com/modelcontextprotocol/go-sdk/mcp v1.8.0) with %s, exposed_tools=%v",
		agenttools.AllowedToolsArg, res.ExposedTools))
	emit(fmt.Sprintf("PASS toolkit step 2 (call create_invoice): draft invoice %s (inv_num %s) created via MCP tool qty=%d unit_cents=%d currency=%s (key %s)",
		paypal.MaskID(res.CreatedInvoiceID), paypal.MaskID(res.InvoiceNumber), res.VerifiedQuantity, res.VerifiedUnitCents, res.VerifiedCurrency, paypal.MaskID(key)))
	emit(fmt.Sprintf("PASS toolkit step 3 (verify draft via REST client): invoice %s status=%s confirmed via GET /v2/invoicing/invoices/{id} (never sent)",
		paypal.MaskID(res.CreatedInvoiceID), res.VerifiedStatus))

	saveReport()
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
