package act

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"margincheck/internal/paypal"
)

// TestHelperToolkitProcess is a pretend @paypal/mcp server, started as a child process by the tests below. Its
// create_invoice always fails with a message that does not contain the word "DUPLICATE".
func TestHelperToolkitProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ACT_TOOLKIT_HELPER") != "1" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-paypal-mcp", Version: "1.8.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "create_invoice", Description: "Create a draft invoice",
		InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "Invoice number has already been used for another invoice."}}}, nil, nil
		})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func helperToolkit(ctx context.Context, _ string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperToolkitProcess$")
	cmd.Env = append(os.Environ(), "GO_WANT_ACT_TOOLKIT_HELPER=1")
	return cmd, nil
}

// When create fails because an earlier run (or a second click) already made this draft, the draft is found by
// its exact invoice number, whatever words the toolkit's error uses.
func TestACreateFailureFindsTheDraftAnEarlierRunMade(t *testing.T) {
	const existingID = "INV2-EARL-IERR-UNNN-0001"
	var (
		mu       sync.Mutex
		searches int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"FAKE_TOKEN_ACT","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/invoicing/search-invoices":
			searches++
			var q struct {
				InvoiceNumber string `json:"invoice_number"`
			}
			_ = json.NewDecoder(r.Body).Decode(&q)
			if searches == 1 { // the look-before-create: the other run hasn't finished yet
				_, _ = w.Write([]byte(`{"total_items":0,"total_pages":0,"items":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"total_items":1,"total_pages":1,"items":[{"id":"` + existingID +
				`","status":"DRAFT","detail":{"currency_code":"USD","invoice_number":"` + q.InvoiceNumber + `"}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/invoicing/invoices":
			_, _ = w.Write([]byte(`{"total_items":0,"total_pages":0,"items":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := paypal.NewClient(srv.URL, "FAKE_CLIENT_ID_ACT", "FAKE_SECRET_ACT", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	tools := PayPalTools{Client: client, Factory: helperToolkit}
	q, _ := sampleOptions(t)
	args := *ExpectedPlan("deal-1", q, parties, invoiceDay).Calls[0].Create

	id, existed, err := tools.CreateInvoice(context.Background(), args)
	if err != nil || id != existingID || !existed {
		t.Fatalf("CreateInvoice = %q, existed %v, err %v; want the existing draft %s", id, existed, err, existingID)
	}
}
