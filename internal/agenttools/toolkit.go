// Package agenttools connects to the local PayPal MCP server in Sandbox mode using the
// official Model Context Protocol Go SDK (github.com/modelcontextprotocol/go-sdk/mcp v1.8.0),
// exposes only the create_invoice draft tool (--tools=invoices.create), and verifies the
// resulting draft invoice via MarginCheck's PayPal Invoicing v2 REST client.
package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"margincheck/internal/paypal"
)

const (
	// AllowedToolsArg limits @paypal/mcp to the single create_invoice tool.
	// Official docs:
	//   - https://developer.paypal.com/ai-tools/mcp-server/
	//   - https://github.com/paypal/agent-toolkit/blob/main/typescript/src/modelcontextprotocol/toolkit.ts
	AllowedToolsArg = "--tools=invoices.create"

	// CreateInvoiceToolName is the MCP tool method name exposed when invoices.create is enabled.
	// Official doc: https://github.com/paypal/agent-toolkit
	CreateInvoiceToolName = "create_invoice"

	// SandboxEnvironment forces the PayPal MCP server to run against PayPal Sandbox only.
	// Official doc: https://developer.paypal.com/ai-tools/mcp-server/
	SandboxEnvironment = "SANDBOX"
)

var invoiceIDRe = regexp.MustCompile(`(INV2-[A-Za-z0-9-]+)`)

// CommandFactory constructs the local MCP server *exec.Cmd given a sandbox OAuth access token.
type CommandFactory func(ctx context.Context, accessToken string) (*exec.Cmd, error)

// DraftProofInput holds the parameters for creating one draft invoice via the PayPal MCP server.
// Money is represented strictly in whole integer cents.
type DraftProofInput struct {
	IdempotencyKey string
	CurrencyCode   string
	SellerEmail    string
	BuyerEmail     string
	ItemName       string
	Quantity       int64
	UnitPriceCents int64
}

// DraftProofResult records the verified outcome of the MCP toolkit connection proof.
type DraftProofResult struct {
	ExposedTools      []string
	CreatedInvoiceID  string
	InvoiceNumber     string
	VerifiedStatus    string
	VerifiedCurrency  string
	VerifiedQuantity  int64
	VerifiedUnitCents int64
}

// NewPayPalMCPCommand builds the local npx @paypal/mcp command in SANDBOX mode limited to --tools=invoices.create.
// Official doc: https://developer.paypal.com/ai-tools/mcp-server/
// Command: npx -y @paypal/mcp --tools=invoices.create
// Environment: PAYPAL_ACCESS_TOKEN=<token>, PAYPAL_ENVIRONMENT=SANDBOX.
func NewPayPalMCPCommand(ctx context.Context, accessToken string) (*exec.Cmd, error) {
	tok := strings.TrimSpace(accessToken)
	if tok == "" {
		return nil, errors.New("agenttools: sandbox access token is required for PayPal MCP server")
	}

	npxBin := "npx"
	if runtime.GOOS == "windows" {
		if resolved, err := exec.LookPath("npx.cmd"); err == nil {
			npxBin = resolved
		}
	} else if resolved, err := exec.LookPath("npx"); err == nil {
		npxBin = resolved
	}

	cmd := exec.CommandContext(ctx, npxBin, "-y", "@paypal/mcp", AllowedToolsArg)
	cmd.Env = buildSandboxEnv(os.Environ(), tok)
	return cmd, nil
}

func buildSandboxEnv(base []string, accessToken string) []string {
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		upper := strings.ToUpper(kv)
		if strings.HasPrefix(upper, "PAYPAL_ACCESS_TOKEN=") || strings.HasPrefix(upper, "PAYPAL_ENVIRONMENT=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"PAYPAL_ACCESS_TOKEN="+accessToken,
		"PAYPAL_ENVIRONMENT="+SandboxEnvironment,
	)
	return out
}

// RunDraftProof connects from Go to the local PayPal MCP server over stdio, verifies that only
// create_invoice is exposed, calls create_invoice once (40 boxes, USD, in whole cents), and
// confirms the draft exists in DRAFT status via our PayPal REST client (never sending).
// Official docs:
//   - https://developer.paypal.com/ai-tools/mcp-server/
//   - https://github.com/paypal/agent-toolkit
//   - https://github.com/paypal/agent-toolkit/blob/main/typescript/src/shared/parameters.ts
//   - https://github.com/paypal/agent-toolkit/blob/main/typescript/src/modelcontextprotocol/toolkit.ts
//   - https://github.com/modelcontextprotocol/go-sdk
func RunDraftProof(ctx context.Context, ppClient *paypal.Client, in DraftProofInput, factory CommandFactory) (*DraftProofResult, error) {
	if ppClient == nil {
		return nil, errors.New("agenttools: paypal REST client is required")
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" {
		return nil, errors.New("agenttools: idempotency key is required")
	}
	invNum, err := paypal.DeriveInvoiceNumber(key)
	if err != nil {
		return nil, err
	}
	currency := strings.ToUpper(strings.TrimSpace(in.CurrencyCode))
	if len(currency) != 3 {
		return nil, errors.New("agenttools: 3-letter currency_code is required")
	}
	seller := strings.TrimSpace(in.SellerEmail)
	buyer := strings.TrimSpace(in.BuyerEmail)
	if seller == "" || buyer == "" {
		return nil, errors.New("agenttools: seller and buyer email addresses are required")
	}
	itemName := strings.TrimSpace(in.ItemName)
	if itemName == "" {
		return nil, errors.New("agenttools: item name is required")
	}
	if in.Quantity <= 0 {
		return nil, errors.New("agenttools: quantity must be positive")
	}
	unitValue, err := paypal.FormatCents(in.UnitPriceCents)
	if err != nil {
		return nil, err
	}

	token, err := ppClient.AccessToken(ctx, "mcp-tok-"+key)
	if err != nil {
		return nil, err
	}

	if factory == nil {
		factory = NewPayPalMCPCommand
	}
	cmd, err := factory(ctx, token)
	if err != nil {
		return nil, err
	}

	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "margincheck-agenttools",
		Version: "v1.0.0",
	}, nil)
	session, err := mcpClient.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("agenttools: mcp connect failed: %s", redactSecrets(err.Error(), token, seller, buyer))
	}
	defer session.Close()

	toolsRes, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("agenttools: mcp list_tools failed: %s", redactSecrets(err.Error(), token, seller, buyer))
	}

	exposed := make([]string, 0, len(toolsRes.Tools))
	for _, t := range toolsRes.Tools {
		if t != nil {
			exposed = append(exposed, t.Name)
		}
	}
	if len(exposed) != 1 || exposed[0] != CreateInvoiceToolName {
		return nil, fmt.Errorf("agenttools: expected only [%s] exposed by MCP server, got %v", CreateInvoiceToolName, exposed)
	}

	// Build create_invoice arguments per createInvoiceParameters in typescript/src/shared/parameters.ts:
	// currency_code, invoice_number, invoicer_email_address, primary_recipients[].billing_info.email_address,
	// items[].name, items[].quantity, items[].unit_amount.{currency_code,value}, items[].unit_of_measure.
	args := map[string]any{
		"currency_code":          currency,
		"invoice_number":         invNum,
		"invoicer_email_address": seller,
		"primary_recipients": []map[string]any{
			{
				"billing_info": map[string]any{
					"email_address": buyer,
				},
			},
		},
		"items": []map[string]any{
			{
				"name":     itemName,
				"quantity": strconv.FormatInt(in.Quantity, 10),
				"unit_amount": map[string]any{
					"currency_code": currency,
					"value":        unitValue,
				},
				"unit_of_measure": "QUANTITY",
			},
		},
	}

	callRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      CreateInvoiceToolName,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("agenttools: mcp call_tool(%s) failed: %s", CreateInvoiceToolName, redactSecrets(err.Error(), token, seller, buyer))
	}
	if callRes.IsError {
		return nil, fmt.Errorf("agenttools: mcp tool %s returned error result", CreateInvoiceToolName)
	}

	invoiceID := extractInvoiceIDFromCallResult(callRes)
	if invoiceID == "" {
		return nil, errors.New("agenttools: create_invoice tool response did not contain an INV2-... invoice ID")
	}

	// Confirm the draft exists via our own PayPal Invoicing v2 REST client from Step 3 (never send).
	verifiedInv, err := ppClient.GetInvoice(ctx, invoiceID)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(verifiedInv.Status, "DRAFT") {
		return nil, fmt.Errorf("agenttools: expected DRAFT status for %s, got %s", paypal.MaskID(verifiedInv.ID), verifiedInv.Status)
	}

	return &DraftProofResult{
		ExposedTools:      exposed,
		CreatedInvoiceID:  verifiedInv.ID,
		InvoiceNumber:     invNum,
		VerifiedStatus:    strings.ToUpper(verifiedInv.Status),
		VerifiedCurrency:  strings.ToUpper(verifiedInv.Detail.CurrencyCode),
		VerifiedQuantity:  in.Quantity,
		VerifiedUnitCents: in.UnitPriceCents,
	}, nil
}

func extractInvoiceIDFromCallResult(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		var text string
		if tc, ok := c.(*mcp.TextContent); ok && tc != nil {
			text = tc.Text
		} else if raw, err := json.Marshal(c); err == nil {
			text = string(raw)
		}
		if m := invoiceIDRe.FindStringSubmatch(text); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func redactSecrets(s string, secrets ...string) string {
	out := s
	for _, sec := range secrets {
		if strings.TrimSpace(sec) != "" {
			out = strings.ReplaceAll(out, sec, "[REDACTED]")
		}
	}
	return out
}
