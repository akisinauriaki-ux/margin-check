package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"margincheck/internal/agenttools"
	"margincheck/internal/paypal"
)

type helperCreateInvoiceInput struct {
	CurrencyCode         string           `json:"currency_code"`
	InvoiceNumber        string           `json:"invoice_number,omitempty"`
	InvoicerEmailAddress string           `json:"invoicer_email_address,omitempty"`
	PrimaryRecipients    []map[string]any `json:"primary_recipients,omitempty"`
	Items                []map[string]any `json:"items,omitempty"`
}

func TestHelperMCPServerProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_MCP_SERVER") != "1" {
		return
	}
	if os.Getenv("PAYPAL_ENVIRONMENT") != agenttools.SandboxEnvironment {
		os.Exit(2)
	}
	if strings.TrimSpace(os.Getenv("PAYPAL_ACCESS_TOKEN")) == "" {
		os.Exit(3)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "fake-paypal-mcp",
		Version: "1.8.1",
	}, nil)

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"currency_code":  map[string]any{"type": "string"},
			"invoice_number": map[string]any{"type": "string"},
			"invoice_date":   map[string]any{"type": "string"},
			"primary_recipients": map[string]any{
				"type": "array",
			},
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string"},
						"quantity":    map[string]any{"type": "string"},
						"unit_amount": map[string]any{"type": "object"},
						"discount": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"percent": map[string]any{"type": "string"},
								"amount":  map[string]any{"type": "object"},
							},
						},
					},
				},
			},
		},
		"required": []string{"currency_code", "primary_recipients", "items"},
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        agenttools.CreateInvoiceToolName,
		Description: "Create a draft invoice in PayPal Sandbox",
		InputSchema: schema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in helperCreateInvoiceInput) (*mcp.CallToolResult, any, error) {
		if in.CurrencyCode != "USD" || in.InvoiceNumber == "" || len(in.Items) != 1 {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid arguments"}},
			}, nil, nil
		}
		if invFile := os.Getenv("TEST_RECORD_INVNUM_FILE"); invFile != "" {
			_ = os.WriteFile(invFile, []byte(in.InvoiceNumber), 0o600)
		}
		respJSON, _ := json.Marshal(map[string]any{
			"rel":    "self",
			"href":   "https://api.sandbox.paypal.com/v2/invoicing/invoices/INV2-MCP1-MCP2-MCP3-6543",
			"method": "GET",
		})
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(respJSON)}},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestToolkitCheckMissingEnvPrintsExactMessageAndStops(t *testing.T) {
	for _, k := range []string{
		"PAYPAL_CLIENT_ID",
		"PAYPAL_CLIENT_SECRET",
		"PAYPAL_API_BASE",
		"PAYPAL_SANDBOX_SELLER_EMAIL",
		"PAYPAL_SANDBOX_BUYER_EMAIL",
	} {
		t.Setenv(k, "")
	}

	tmp := t.TempDir()
	reportPath := filepath.Join(tmp, "reports", "step2-toolkit.txt")

	var out bytes.Buffer
	code := runToolkitProof(context.Background(), filepath.Join(tmp, "missing.env"), reportPath, nil, nil, &out)
	if code != 0 {
		t.Fatalf("expected clean exit code 0 when .env is missing, got %d", code)
	}
	if strings.TrimSpace(out.String()) != paypal.MissingEnvMessage {
		t.Fatalf("got %q, want %q", strings.TrimSpace(out.String()), paypal.MissingEnvMessage)
	}
}

func TestRunToolkitProofWithFakeServersVerifiesReadBackAndDetectsMissingPaymentTerms(t *testing.T) {
	const (
		fakeClientID = "CID_TOOLKIT_TEST"
		fakeSecret   = "CSEC_TOOLKIT_TEST"
		fakeToken    = "TOK_TOOLKIT_TEST_SECRET"
		wantInvID    = "INV2-MCP1-MCP2-MCP3-6543"
	)

	tmp := t.TempDir()
	lastInvNumFile := filepath.Join(tmp, "last_invnum.txt")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"` + fakeToken + `","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/invoicing/invoices/"+wantInvID):
			invNumBytes, _ := os.ReadFile(lastInvNumFile)
			invNum := strings.TrimSpace(string(invNumBytes))
			if invNum == "" {
				invNum = "MC-UNKNOWN"
			}
			_, _ = w.Write([]byte(`{
				"id":"` + wantInvID + `",
				"status":"DRAFT",
				"detail":{"currency_code":"USD","invoice_number":"` + invNum + `","invoice_date":"2026-10-26"},
				"items":[{"name":"Wholesale Boxes","quantity":"40","unit_amount":{"currency_code":"USD","value":"15.00"}}]
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	t.Setenv("PAYPAL_CLIENT_ID", fakeClientID)
	t.Setenv("PAYPAL_CLIENT_SECRET", fakeSecret)
	t.Setenv("PAYPAL_API_BASE", srv.URL)
	t.Setenv("PAYPAL_SANDBOX_SELLER_EMAIL", "seller@example.com")
	t.Setenv("PAYPAL_SANDBOX_BUYER_EMAIL", "buyer@example.com")

	envPath := filepath.Join(tmp, ".env")
	envContent := "PAYPAL_CLIENT_ID=" + fakeClientID + "\nPAYPAL_CLIENT_SECRET=" + fakeSecret + "\nPAYPAL_API_BASE=" + srv.URL + "\nPAYPAL_SANDBOX_SELLER_EMAIL=seller@example.com\nPAYPAL_SANDBOX_BUYER_EMAIL=buyer@example.com\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		t.Fatalf("WriteFile .env: %v", err)
	}

	reportPath := filepath.Join(tmp, "reports", "step2-toolkit.txt")

	factory := func(ctx context.Context, accessToken string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperMCPServerProcess")
		cmd.Env = append(agenttools.BuildMinimalSandboxEnv(os.Environ(), accessToken),
			"GO_WANT_HELPER_MCP_SERVER=1",
			"TEST_RECORD_INVNUM_FILE="+lastInvNumFile,
		)
		return cmd, nil
	}

	var out bytes.Buffer
	code := runToolkitProof(context.Background(), envPath, reportPath, srv.Client(), factory, &out)
	if code != 0 {
		t.Fatalf("expected runToolkitProof to exit 0, got %d, output: %s", code, out.String())
	}

	outStr := out.String()
	if !strings.Contains(outStr, "PASS toolkit step 1") {
		t.Errorf("missing step 1 pass in output: %s", outStr)
	}
	if !strings.Contains(outStr, "(verified)") {
		t.Errorf("missing verified read-back in output: %s", outStr)
	}
	if !strings.Contains(outStr, "FINDING toolkit step 3") || !strings.Contains(outStr, "payment terms is MISSING") {
		t.Errorf("missing payment terms missing finding in output: %s", outStr)
	}
	if !strings.Contains(outStr, "STOP toolkit step 4") {
		t.Errorf("missing step 4 stop in output: %s", outStr)
	}

	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("failed to read report file: %v", err)
	}
	reportStr := string(reportBytes)
	if !strings.Contains(reportStr, "create_invoice") {
		t.Errorf("expected report to contain create_invoice, got: %s", reportStr)
	}
}
