// Package agenttools connects to the local PayPal MCP server in Sandbox mode using the
// official Model Context Protocol Go SDK (github.com/modelcontextprotocol/go-sdk/mcp v1.8.0),
// exposes only the create_invoice draft tool (--tools=invoices.create), pins @paypal/mcp to an exact
// version (@paypal/mcp@1.8.1), isolates the child process with a minimal environment, and verifies
// draft invoices via MarginCheck's PayPal Invoicing v2 REST client read-back.
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
	// PinnedPayPalMCPPackage is the pinned exact version of the PayPal MCP server package.
	// Official release: https://github.com/paypal/agent-toolkit/releases/tag/v1.8.0
	// Official npm registry: https://registry.npmjs.org/@paypal/mcp (tag latest: 1.8.1)
	PinnedPayPalMCPPackage = "@paypal/mcp@1.8.1"

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

var (
	invoiceIDRe    = regexp.MustCompile(`(INV2-[A-Za-z0-9-]+)`)
	exactVersionRe = regexp.MustCompile(`^@paypal/mcp@\d+\.\d+\.\d+$`)
)

var allowedWindowsEnvKeys = map[string]bool{
	"PATH":         true,
	"PATHEXT":      true,
	"SYSTEMROOT":   true,
	"COMSPEC":      true,
	"TEMP":         true,
	"TMP":          true,
	"APPDATA":      true,
	"LOCALAPPDATA": true,
	"USERPROFILE":  true,
}

var allowedUnixEnvKeys = map[string]bool{
	"PATH": true,
	"HOME": true,
	"TEMP": true,
	"TMP":  true,
	"USER": true,
}

// HasExactPackageVersion reports whether pkg has an exact pinned version of the form @paypal/mcp@<x.y.z>.
func HasExactPackageVersion(pkg string) bool {
	return exactVersionRe.MatchString(strings.TrimSpace(pkg))
}

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

// VerifiedReadBack records field-by-field verification from PayPal's Invoicing v2 REST read-back.
type VerifiedReadBack struct {
	StatusVerified        bool   `json:"status_verified"`
	CurrencyVerified      bool   `json:"currency_verified"`
	InvoiceNumberVerified bool   `json:"invoice_number_verified"`
	QuantityVerified      bool   `json:"quantity_verified"`
	UnitPriceVerified     bool   `json:"unit_price_verified"`

	VerifiedStatus    string `json:"verified_status"`
	VerifiedCurrency  string `json:"verified_currency"`
	VerifiedInvNum    string `json:"verified_invoice_number"`
	VerifiedQuantity  int64  `json:"verified_quantity"`
	VerifiedUnitCents int64  `json:"verified_unit_cents"`
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
	ReadBack          *VerifiedReadBack
}

// ToolSchemaFinding records the schema analysis of create_invoice's input parameters.
type ToolSchemaFinding struct {
	ExposedTools         []string       `json:"exposed_tools"`
	CreateInvoiceSchema  map[string]any `json:"create_invoice_schema"`
	HasItemDiscount      bool           `json:"has_item_discount"`
	HasPaymentTerms      bool           `json:"has_payment_terms"`
	MissingRequiredField string         `json:"missing_required_field,omitempty"`
	Summary              string         `json:"summary"`
}

// NewPayPalMCPCommand builds the local npx @paypal/mcp@<x.y.z> command in SANDBOX mode limited to --tools=invoices.create.
// It verifies that the package has an exact version pin and provides a minimal environment to the child process.
// Official doc: https://developer.paypal.com/ai-tools/mcp-server/
func NewPayPalMCPCommand(ctx context.Context, accessToken string) (*exec.Cmd, error) {
	tok := strings.TrimSpace(accessToken)
	if tok == "" {
		return nil, errors.New("agenttools: sandbox access token is required for PayPal MCP server")
	}

	if !HasExactPackageVersion(PinnedPayPalMCPPackage) {
		return nil, fmt.Errorf("agenttools: package %q must be pinned to an exact version", PinnedPayPalMCPPackage)
	}

	npxBin := "npx"
	if runtime.GOOS == "windows" {
		if resolved, err := exec.LookPath("npx.cmd"); err == nil {
			npxBin = resolved
		}
	} else if resolved, err := exec.LookPath("npx"); err == nil {
		npxBin = resolved
	}

	cmd := exec.CommandContext(ctx, npxBin, "-y", PinnedPayPalMCPPackage, AllowedToolsArg)
	cmd.Env = BuildMinimalSandboxEnv(os.Environ(), tok)
	return cmd, nil
}

// BuildMinimalSandboxEnv filters parent environment variables down to an explicit minimal allowlist
// required by npx on Windows (PATH, PATHEXT, SystemRoot, ComSpec, TEMP, TMP, APPDATA, LOCALAPPDATA, USERPROFILE)
// or Unix (PATH, HOME, TEMP, TMP, USER), plus PAYPAL_ACCESS_TOKEN and PAYPAL_ENVIRONMENT=SANDBOX.
// Parent credentials such as PAYPAL_CLIENT_SECRET, PAYPAL_CLIENT_ID, and GEMINI_API_KEY are strictly excluded.
func BuildMinimalSandboxEnv(base []string, accessToken string) []string {
	allowlist := allowedUnixEnvKeys
	if runtime.GOOS == "windows" {
		allowlist = allowedWindowsEnvKeys
	}

	out := make([]string, 0, len(allowlist)+2)
	for _, kv := range base {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		keyUpper := strings.ToUpper(parts[0])
		if allowlist[keyUpper] {
			out = append(out, kv)
		}
	}
	out = append(out,
		"PAYPAL_ACCESS_TOKEN="+accessToken,
		"PAYPAL_ENVIRONMENT="+SandboxEnvironment,
	)
	return out
}

// VerifyDraftReadBack compares PayPal's REST read-back invoice against expected input values field by field.
// It verifies: status DRAFT, currency, quantity, unit price in cents, and the MC- invoice number.
// Any mismatch returns an error naming each mismatched field.
func VerifyDraftReadBack(inv *paypal.Invoice, expected DraftProofInput, expectedInvNum string) (*VerifiedReadBack, error) {
	if inv == nil {
		return nil, errors.New("agenttools: read-back invoice is nil")
	}

	var mismatches []string
	res := &VerifiedReadBack{}

	// 1. Status DRAFT
	if strings.EqualFold(inv.Status, "DRAFT") {
		res.StatusVerified = true
		res.VerifiedStatus = strings.ToUpper(inv.Status)
	} else {
		mismatches = append(mismatches, fmt.Sprintf("status: got %q, want %q", inv.Status, "DRAFT"))
	}

	// 2. Currency
	if strings.EqualFold(inv.Detail.CurrencyCode, expected.CurrencyCode) {
		res.CurrencyVerified = true
		res.VerifiedCurrency = strings.ToUpper(inv.Detail.CurrencyCode)
	} else {
		mismatches = append(mismatches, fmt.Sprintf("currency: got %q, want %q", inv.Detail.CurrencyCode, expected.CurrencyCode))
	}

	// 3. Invoice Number (MC-...)
	if inv.Detail.InvoiceNumber == expectedInvNum {
		res.InvoiceNumberVerified = true
		res.VerifiedInvNum = inv.Detail.InvoiceNumber
	} else {
		mismatches = append(mismatches, fmt.Sprintf("invoice_number: got %q, want %q", inv.Detail.InvoiceNumber, expectedInvNum))
	}

	// 4. Quantity & Unit Price in whole cents
	wantUnitVal, err := paypal.FormatCents(expected.UnitPriceCents)
	if err != nil {
		return nil, err
	}
	wantQtyStr := strconv.FormatInt(expected.Quantity, 10)

	for _, it := range inv.Items {
		qtyTrimmed := strings.TrimSpace(it.Quantity)
		if qtyTrimmed == wantQtyStr {
			parsedQty, err := strconv.ParseInt(qtyTrimmed, 10, 64)
			if err == nil && parsedQty == expected.Quantity {
				res.QuantityVerified = true
				res.VerifiedQuantity = parsedQty
			}
		}
		if strings.TrimSpace(it.UnitAmount.Value) == wantUnitVal && strings.EqualFold(it.UnitAmount.CurrencyCode, expected.CurrencyCode) {
			res.UnitPriceVerified = true
			res.VerifiedUnitCents = expected.UnitPriceCents
		}
		if res.QuantityVerified && res.UnitPriceVerified {
			break
		}
	}

	if !res.QuantityVerified {
		mismatches = append(mismatches, fmt.Sprintf("quantity: expected item qty %d, not found in read-back", expected.Quantity))
	}
	if !res.UnitPriceVerified {
		mismatches = append(mismatches, fmt.Sprintf("unit_price_cents: expected unit amount %s %s, not found in read-back", wantUnitVal, expected.CurrencyCode))
	}

	if len(mismatches) > 0 {
		return res, fmt.Errorf("agenttools: read-back verification failed: %s", strings.Join(mismatches, "; "))
	}
	return res, nil
}

// RunDraftProof connects from Go to the local PayPal MCP server over stdio, verifies that only
// create_invoice is exposed, calls create_invoice once, reads the draft back through PayPal's
// Invoicing v2 REST client, and verifies the read-back field by field (status DRAFT, currency,
// quantity, unit price in cents, and MC- invoice number).
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

	// Read draft back via MarginCheck's PayPal Invoicing v2 REST client (never send).
	verifiedInv, err := ppClient.GetInvoice(ctx, invoiceID)
	if err != nil {
		return nil, err
	}

	readBack, err := VerifyDraftReadBack(verifiedInv, in, invNum)
	if err != nil {
		return nil, err
	}

	return &DraftProofResult{
		ExposedTools:      exposed,
		CreatedInvoiceID:  verifiedInv.ID,
		InvoiceNumber:     readBack.VerifiedInvNum,
		VerifiedStatus:    readBack.VerifiedStatus,
		VerifiedCurrency:  readBack.VerifiedCurrency,
		VerifiedQuantity:  readBack.VerifiedQuantity,
		VerifiedUnitCents: readBack.VerifiedUnitCents,
		ReadBack:          readBack,
	}, nil
}

// InspectCreateInvoiceTool connects to the pinned local MCP server, lists tools, extracts create_invoice's
// input schema, and analyzes whether payment terms and item discount can be configured.
func InspectCreateInvoiceTool(ctx context.Context, factory CommandFactory, accessToken string) (*ToolSchemaFinding, error) {
	if factory == nil {
		factory = NewPayPalMCPCommand
	}
	cmd, err := factory(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "margincheck-agenttools-inspect",
		Version: "v1.0.0",
	}, nil)
	session, err := mcpClient.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("agenttools: mcp connect failed: %w", err)
	}
	defer session.Close()

	toolsRes, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("agenttools: mcp list_tools failed: %w", err)
	}

	finding := &ToolSchemaFinding{
		ExposedTools: make([]string, 0, len(toolsRes.Tools)),
	}

	var createInvoiceTool *mcp.Tool
	for _, t := range toolsRes.Tools {
		if t != nil {
			finding.ExposedTools = append(finding.ExposedTools, t.Name)
			if t.Name == CreateInvoiceToolName {
				createInvoiceTool = t
			}
		}
	}
	if createInvoiceTool == nil {
		return nil, fmt.Errorf("agenttools: tool %s was not found in exposed tools %v", CreateInvoiceToolName, finding.ExposedTools)
	}

	schemaMap, ok := createInvoiceTool.InputSchema.(map[string]any)
	if !ok {
		raw, err := json.Marshal(createInvoiceTool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("agenttools: invalid tool input schema format: %w", err)
		}
		if err := json.Unmarshal(raw, &schemaMap); err != nil {
			return nil, fmt.Errorf("agenttools: failed to parse tool input schema: %w", err)
		}
	}
	finding.CreateInvoiceSchema = schemaMap

	props, _ := schemaMap["properties"].(map[string]any)

	// 1. Check item discount in items[].properties.discount
	if itemsProp, ok := props["items"].(map[string]any); ok {
		if itemObj, ok := itemsProp["items"].(map[string]any); ok {
			if itemProps, ok := itemObj["properties"].(map[string]any); ok {
				if _, ok := itemProps["discount"]; ok {
					finding.HasItemDiscount = true
				}
			}
		}
	}

	// 2. Check payment terms in properties
	if props != nil {
		for _, termKey := range []string{"payment_term", "payment_terms", "due_date", "term_type"} {
			if _, ok := props[termKey]; ok {
				finding.HasPaymentTerms = true
				break
			}
		}
	}

	if !finding.HasPaymentTerms {
		finding.MissingRequiredField = "payment_term (due_date / term_type)"
		finding.Summary = "create_invoice supports item discount (items[].discount) but lacks payment terms (payment_term / due_date). Per brief Part C.4, execution must stop with no workaround."
	} else if !finding.HasItemDiscount {
		finding.MissingRequiredField = "items[].discount"
		finding.Summary = "create_invoice supports payment terms but lacks item discount. Per brief Part C.4, execution must stop with no workaround."
	} else {
		finding.Summary = "create_invoice supports both item discount and payment terms."
	}

	return finding, nil
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
