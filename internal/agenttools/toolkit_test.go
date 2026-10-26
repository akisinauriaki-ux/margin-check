package agenttools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	if os.Getenv("PAYPAL_ENVIRONMENT") != SandboxEnvironment {
		os.Exit(2)
	}
	if strings.TrimSpace(os.Getenv("PAYPAL_ACCESS_TOKEN")) == "" {
		os.Exit(3)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "fake-paypal-mcp",
		Version: "1.8.1",
	}, nil)

	// Simulate the real @paypal/mcp create_invoice tool schema
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
		Name:        CreateInvoiceToolName,
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

func TestNewPayPalMCPCommandPinsExactVersion(t *testing.T) {
	if !HasExactPackageVersion(PinnedPayPalMCPPackage) {
		t.Fatalf("PinnedPayPalMCPPackage %q must match exact version pattern", PinnedPayPalMCPPackage)
	}
	if HasExactPackageVersion("@paypal/mcp") {
		t.Fatalf("unpinned @paypal/mcp must fail HasExactPackageVersion")
	}
	if HasExactPackageVersion("@paypal/mcp@latest") {
		t.Fatalf("latest tag must fail HasExactPackageVersion")
	}

	cmd, err := NewPayPalMCPCommand(context.Background(), "tok_test_123")
	if err != nil {
		t.Fatalf("NewPayPalMCPCommand failed: %v", err)
	}

	wantArgs := []string{"-y", PinnedPayPalMCPPackage, AllowedToolsArg}
	if len(cmd.Args) < 4 {
		t.Fatalf("unexpected cmd.Args: %v", cmd.Args)
	}
	gotTail := cmd.Args[len(cmd.Args)-3:]
	for i := range wantArgs {
		if gotTail[i] != wantArgs[i] {
			t.Fatalf("cmd.Args tail = %v, want %v", gotTail, wantArgs)
		}
	}
}

func TestBuildMinimalSandboxEnvExcludesSecrets(t *testing.T) {
	dirtyParentEnv := []string{
		"PAYPAL_CLIENT_SECRET=super_secret_shhh",
		"PAYPAL_CLIENT_ID=client_id_secret_abc",
		"GEMINI_API_KEY=AIzaSySecretGeminiKey123",
		"SOME_OTHER_KEY=dont_leak_me",
		"PATH=C:\\Windows\\system32;C:\\Windows",
		"SystemRoot=C:\\Windows",
		"TEMP=C:\\Temp",
		"USERPROFILE=C:\\Users\\test",
	}

	cleanEnv := BuildMinimalSandboxEnv(dirtyParentEnv, "sandbox_token_xyz")

	for _, kv := range cleanEnv {
		upper := strings.ToUpper(kv)
		if strings.Contains(upper, "PAYPAL_CLIENT_SECRET") {
			t.Fatalf("PAYPAL_CLIENT_SECRET must never reach child env: %s", kv)
		}
		if strings.Contains(upper, "PAYPAL_CLIENT_ID") {
			t.Fatalf("PAYPAL_CLIENT_ID must never reach child env: %s", kv)
		}
		if strings.Contains(upper, "GEMINI_API_KEY") {
			t.Fatalf("GEMINI_API_KEY must never reach child env: %s", kv)
		}
		if strings.Contains(upper, "SOME_OTHER_KEY") {
			t.Fatalf("unallowed variable leaked into child env: %s", kv)
		}
	}

	hasToken := false
	hasSandbox := false
	for _, kv := range cleanEnv {
		if kv == "PAYPAL_ACCESS_TOKEN=sandbox_token_xyz" {
			hasToken = true
		}
		if kv == "PAYPAL_ENVIRONMENT=SANDBOX" {
			hasSandbox = true
		}
	}
	if !hasToken || !hasSandbox {
		t.Fatalf("missing PAYPAL_ACCESS_TOKEN or PAYPAL_ENVIRONMENT=SANDBOX in cleaned env: %v", cleanEnv)
	}
}

func TestVerifyDraftReadBackFieldByField(t *testing.T) {
	const wantInvNum = "MC-1234567890ABCDEF123456"
	expectedInput := DraftProofInput{
		IdempotencyKey: "test-key-1",
		CurrencyCode:   "USD",
		Quantity:       40,
		UnitPriceCents: 1500,
	}

	validInv := &paypal.Invoice{
		ID:     "INV2-TEST-1234",
		Status: "DRAFT",
		Detail: paypal.InvoiceDetail{
			CurrencyCode:  "USD",
			InvoiceNumber: wantInvNum,
		},
		Items: []paypal.InvoiceLineItem{
			{
				Name:     "Wholesale Boxes",
				Quantity: "40",
				UnitAmount: paypal.Money{
					CurrencyCode: "USD",
					Value:        "15.00",
				},
			},
		},
	}

	// 1. Success case: all fields match
	rb, err := VerifyDraftReadBack(validInv, expectedInput, wantInvNum)
	if err != nil {
		t.Fatalf("expected clean pass, got: %v", err)
	}
	if !rb.StatusVerified || !rb.CurrencyVerified || !rb.InvoiceNumberVerified || !rb.QuantityVerified || !rb.UnitPriceVerified {
		t.Fatalf("expected all 5 verified flags to be true, got %+v", rb)
	}
	if rb.VerifiedQuantity != 40 || rb.VerifiedUnitCents != 1500 || rb.VerifiedStatus != "DRAFT" || rb.VerifiedCurrency != "USD" {
		t.Fatalf("unexpected verified values: %+v", rb)
	}

	// 2. Status mismatch
	badStatus := *validInv
	badStatus.Status = "SENT"
	_, err = VerifyDraftReadBack(&badStatus, expectedInput, wantInvNum)
	if err == nil || !strings.Contains(err.Error(), "status:") {
		t.Fatalf("expected status mismatch error, got: %v", err)
	}

	// 3. Currency mismatch
	badCurr := *validInv
	badCurr.Detail.CurrencyCode = "EUR"
	_, err = VerifyDraftReadBack(&badCurr, expectedInput, wantInvNum)
	if err == nil || !strings.Contains(err.Error(), "currency:") {
		t.Fatalf("expected currency mismatch error, got: %v", err)
	}

	// 4. Invoice number mismatch
	badInvNum := *validInv
	badInvNum.Detail.InvoiceNumber = "MC-WRONG"
	_, err = VerifyDraftReadBack(&badInvNum, expectedInput, wantInvNum)
	if err == nil || !strings.Contains(err.Error(), "invoice_number:") {
		t.Fatalf("expected invoice_number mismatch error, got: %v", err)
	}

	// 5. Quantity mismatch
	badQty := *validInv
	badQty.Items = []paypal.InvoiceLineItem{
		{
			Quantity:   "35",
			UnitAmount: paypal.Money{CurrencyCode: "USD", Value: "15.00"},
		},
	}
	_, err = VerifyDraftReadBack(&badQty, expectedInput, wantInvNum)
	if err == nil || !strings.Contains(err.Error(), "quantity:") {
		t.Fatalf("expected quantity mismatch error, got: %v", err)
	}

	// 6. Unit price mismatch
	badPrice := *validInv
	badPrice.Items = []paypal.InvoiceLineItem{
		{
			Quantity:   "40",
			UnitAmount: paypal.Money{CurrencyCode: "USD", Value: "14.50"},
		},
	}
	_, err = VerifyDraftReadBack(&badPrice, expectedInput, wantInvNum)
	if err == nil || !strings.Contains(err.Error(), "unit_price_cents:") {
		t.Fatalf("expected unit_price_cents mismatch error, got: %v", err)
	}
}

func TestInspectCreateInvoiceToolIdentifiesMissingPaymentTerms(t *testing.T) {
	factory := func(ctx context.Context, accessToken string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperMCPServerProcess")
		cmd.Env = append(BuildMinimalSandboxEnv(os.Environ(), accessToken), "GO_WANT_HELPER_MCP_SERVER=1")
		return cmd, nil
	}

	finding, err := InspectCreateInvoiceTool(context.Background(), factory, "dummy_token")
	if err != nil {
		t.Fatalf("InspectCreateInvoiceTool failed: %v", err)
	}

	if !finding.HasItemDiscount {
		t.Fatalf("expected HasItemDiscount=true, got false")
	}
	if finding.HasPaymentTerms {
		t.Fatalf("expected HasPaymentTerms=false, got true")
	}
	if !strings.Contains(finding.MissingRequiredField, "payment_term") {
		t.Fatalf("expected MissingRequiredField to specify payment_term, got %q", finding.MissingRequiredField)
	}
}

func TestRunDraftProofConnectsViaGoSDKAndVerifiesDraftViaRESTClient(t *testing.T) {
	const (
		fakeClientID = "CID_MCP_TEST"
		fakeSecret   = "CSEC_MCP_TEST"
		fakeToken    = "TOK_MCP_TEST_SECRET"
		wantInvID    = "INV2-MCP1-MCP2-MCP3-6543"
	)

	key := "mcp-proof-key-1"
	wantInvNum, err := paypal.DeriveInvoiceNumber(key)
	if err != nil {
		t.Fatalf("DeriveInvoiceNumber: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"` + fakeToken + `","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices/"+wantInvID:
			_, _ = w.Write([]byte(`{
				"id":"` + wantInvID + `",
				"status":"DRAFT",
				"detail":{"currency_code":"USD","invoice_number":"` + wantInvNum + `","invoice_date":"2026-10-25"},
				"items":[{"name":"Wholesale Boxes","quantity":"40","unit_amount":{"currency_code":"USD","value":"15.00"}}]
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ppClient, err := paypal.NewClient(srv.URL, fakeClientID, fakeSecret, srv.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	factory := func(ctx context.Context, accessToken string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperMCPServerProcess")
		cmd.Env = append(BuildMinimalSandboxEnv(os.Environ(), accessToken), "GO_WANT_HELPER_MCP_SERVER=1")
		return cmd, nil
	}

	res, err := RunDraftProof(context.Background(), ppClient, DraftProofInput{
		IdempotencyKey: key,
		CurrencyCode:   "USD",
		SellerEmail:    "seller@example.com",
		BuyerEmail:     "buyer@example.com",
		ItemName:       "Wholesale Boxes",
		Quantity:       40,
		UnitPriceCents: 1500,
	}, factory)
	if err != nil {
		t.Fatalf("RunDraftProof failed: %v", err)
	}
	if len(res.ExposedTools) != 1 || res.ExposedTools[0] != CreateInvoiceToolName {
		t.Fatalf("unexpected ExposedTools: %v", res.ExposedTools)
	}
	if res.CreatedInvoiceID != wantInvID || res.VerifiedStatus != "DRAFT" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.ReadBack == nil || !res.ReadBack.StatusVerified || !res.ReadBack.CurrencyVerified || !res.ReadBack.InvoiceNumberVerified || !res.ReadBack.QuantityVerified || !res.ReadBack.UnitPriceVerified {
		t.Fatalf("expected complete verified read-back, got %+v", res.ReadBack)
	}
}
