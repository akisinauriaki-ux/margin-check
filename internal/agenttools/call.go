package agenttools

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CallCreateInvoice starts the pinned PayPal MCP server (only create_invoice exposed, sandbox only, minimal child
// environment), checks that create_invoice is the one and only tool it offers, calls it once with args, and
// returns the new draft's invoice ID. It never calls any other tool and never sends an invoice.
//
// args follow the create_invoice input schema saved in reports/step2-toolkit.txt (@paypal/mcp@1.8.1).
// secrets are redacted from every error message.
func CallCreateInvoice(ctx context.Context, factory CommandFactory, accessToken string, args map[string]any, secrets ...string) (string, error) {
	if factory == nil {
		factory = NewPayPalMCPCommand
	}
	if accessToken == "" {
		return "", errors.New("agenttools: an access token is required")
	}
	secrets = append(secrets, accessToken)

	cmd, err := factory(ctx, accessToken)
	if err != nil {
		return "", err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "margincheck-act", Version: "v1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return "", fmt.Errorf("agenttools: mcp connect failed: %s", redactSecrets(err.Error(), secrets...))
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("agenttools: mcp list_tools failed: %s", redactSecrets(err.Error(), secrets...))
	}
	if len(tools.Tools) != 1 || tools.Tools[0] == nil || tools.Tools[0].Name != CreateInvoiceToolName {
		names := []string{}
		for _, t := range tools.Tools {
			if t != nil {
				names = append(names, t.Name)
			}
		}
		return "", fmt.Errorf("agenttools: expected only [%s] exposed by the MCP server, got %v", CreateInvoiceToolName, names)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: CreateInvoiceToolName, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("agenttools: create_invoice failed: %s", redactSecrets(err.Error(), secrets...))
	}
	if res.IsError {
		return "", fmt.Errorf("agenttools: create_invoice returned an error: %s", redactSecrets(toolText(res), secrets...))
	}
	id := extractInvoiceIDFromCallResult(res)
	if id == "" {
		return "", errors.New("agenttools: create_invoice did not return an INV2-... invoice ID")
	}
	return id, nil
}

func toolText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && tc != nil {
			if len(tc.Text) > 300 {
				return tc.Text[:300]
			}
			return tc.Text
		}
	}
	return "(no text)"
}
