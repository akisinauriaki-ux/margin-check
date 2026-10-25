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

	mcp.AddTool(server, &mcp.Tool{
		Name:        CreateInvoiceToolName,
		Description: "Create a draft invoice in PayPal Sandbox",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in helperCreateInvoiceInput) (*mcp.CallToolResult, any, error) {
		if in.CurrencyCode != "USD" || in.InvoiceNumber == "" || len(in.Items) != 1 {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid arguments"}},
			}, nil, nil
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

func TestNewPayPalMCPCommandRestrictsToCreateInvoiceInSandbox(t *testing.T) {
	cmd, err := NewPayPalMCPCommand(context.Background(), "tok_test_123")
	if err != nil {
		t.Fatalf("NewPayPalMCPCommand failed: %v", err)
	}
	wantArgs := []string{"-y", "@paypal/mcp", AllowedToolsArg}
	if len(cmd.Args) < 4 {
		t.Fatalf("unexpected cmd.Args: %v", cmd.Args)
	}
	gotTail := cmd.Args[len(cmd.Args)-3:]
	for i := range wantArgs {
		if gotTail[i] != wantArgs[i] {
			t.Fatalf("cmd.Args tail = %v, want %v", gotTail, wantArgs)
		}
	}

	hasToken := false
	hasSandbox := false
	for _, kv := range cmd.Env {
		if kv == "PAYPAL_ACCESS_TOKEN=tok_test_123" {
			hasToken = true
		}
		if kv == "PAYPAL_ENVIRONMENT=SANDBOX" {
			hasSandbox = true
		}
	}
	if !hasToken || !hasSandbox {
		t.Fatalf("expected PAYPAL_ACCESS_TOKEN and PAYPAL_ENVIRONMENT=SANDBOX in cmd.Env")
	}
}

func TestRunDraftProofConnectsViaGoSDKAndVerifiesDraftViaRESTClient(t *testing.T) {
	const (
		fakeClientID = "CID_MCP_TEST"
		fakeSecret   = "CSEC_MCP_TEST"
		fakeToken    = "TOK_MCP_TEST_SECRET"
		wantInvID    = "INV2-MCP1-MCP2-MCP3-6543"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"` + fakeToken + `","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices/"+wantInvID:
			_, _ = w.Write([]byte(`{"id":"` + wantInvID + `","status":"DRAFT","detail":{"currency_code":"USD","invoice_date":"2026-10-25"}}`))
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
		cmd.Env = append(buildSandboxEnv(os.Environ(), accessToken), "GO_WANT_HELPER_MCP_SERVER=1")
		return cmd, nil
	}

	res, err := RunDraftProof(context.Background(), ppClient, DraftProofInput{
		IdempotencyKey: "mcp-proof-key-1",
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
}
