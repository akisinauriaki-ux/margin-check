package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SetDueDate sets a DRAFT invoice's payment term to a specific due date, and nothing else.
//
// PayPal has no partial update, so this reads the complete invoice, changes only detail.payment_term, and sends
// the complete invoice back. Both update notifications are switched off, because PayPal's default is to notify.
// It refuses any invoice that is not a DRAFT. Repeating it with the same date changes nothing.
//
// Official doc: https://developer.paypal.com/api/invoicing/v2/invoices-update
//   - "Fully updates an invoice, by ID." "In the JSON request body, include a complete `invoice` object."
//     "This call does not support partial updates."
//   - query send_to_recipient (default true) and send_to_invoicer (default true): both sent as false here.
//   - detail.payment_term.term_type "DUE_ON_DATE_SPECIFIED"; detail.payment_term.due_date (date_no_time).
//
// Endpoint: PUT /v2/invoicing/invoices/{invoice_id}?send_to_recipient=false&send_to_invoicer=false
func (c *Client) SetDueDate(ctx context.Context, requestID, invoiceID, dueDate string) error {
	invID := strings.TrimSpace(invoiceID)
	reqID := strings.TrimSpace(requestID)
	if invID == "" || reqID == "" {
		return errors.New("paypal: invoice_id and PayPal-Request-Id are required")
	}
	if _, err := time.Parse("2006-01-02", dueDate); err != nil {
		return fmt.Errorf("paypal: due date %q is not YYYY-MM-DD", dueDate)
	}
	token, err := c.bearerToken(ctx, reqID)
	if err != nil {
		return err
	}
	endpoint := c.baseURL + "/v2/invoicing/invoices/" + url.PathEscape(invID)

	get, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New(c.redact(err.Error()))
	}
	get.Header.Set("Authorization", "Bearer "+token)
	get.Header.Set("Accept", "application/json")
	resp, body, err := c.do(get)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return c.parseAPIError(resp.StatusCode, body)
	}

	var inv map[string]any
	if err := json.Unmarshal(body, &inv); err != nil {
		return errors.New("paypal: invalid invoice response JSON")
	}
	if status, _ := inv["status"].(string); !strings.EqualFold(status, "DRAFT") {
		return fmt.Errorf("paypal: invoice %s is %q, not DRAFT; it will not be changed", MaskID(invID), status)
	}
	detail, ok := inv["detail"].(map[string]any)
	if !ok {
		return errors.New("paypal: invoice has no detail")
	}
	detail["payment_term"] = map[string]any{"term_type": "DUE_ON_DATE_SPECIFIED", "due_date": dueDate}

	raw, err := json.Marshal(inv)
	if err != nil {
		return errors.New("paypal: failed to encode invoice JSON")
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"?send_to_recipient=false&send_to_invoicer=false", bytes.NewReader(raw))
	if err != nil {
		return errors.New(c.redact(err.Error()))
	}
	put.Header.Set("Authorization", "Bearer "+token)
	put.Header.Set("Content-Type", "application/json")
	put.Header.Set("Accept", "application/json")
	put.Header.Set("PayPal-Request-Id", reqID)
	resp, body, err = c.do(put)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return c.parseAPIError(resp.StatusCode, body)
	}
	return nil
}
