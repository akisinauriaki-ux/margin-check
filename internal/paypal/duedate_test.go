package paypal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const draftJSON = `{"id":"INV2-AAAA-BBBB-CCCC-DDDD","status":"DRAFT",
 "detail":{"currency_code":"USD","invoice_number":"MC-X","invoice_date":"2026-10-26","payment_term":{"term_type":"DUE_ON_RECEIPT"},
  "metadata":{"create_time":"2026-10-26T10:00:00Z"}},
 "invoicer":{"email_address":"seller@example.com"},
 "primary_recipients":[{"billing_info":{"email_address":"buyer@example.com"}}],
 "items":[{"name":"Mugs","quantity":"20","unit_amount":{"currency_code":"USD","value":"40.00"},"discount":{"percent":"1.5"}}],
 "amount":{"breakdown":{"shipping":{"amount":{"currency_code":"USD","value":"36.00"}}}}}`

func dueDateServer(t *testing.T, status string, got *map[string]any, query *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token":
			_, _ = w.Write([]byte(`{"access_token":"tok-secret-123","token_type":"Bearer","expires_in":3600}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/invoicing/invoices/"):
			_, _ = w.Write([]byte(strings.Replace(draftJSON, `"status":"DRAFT"`, `"status":"`+status+`"`, 1)))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v2/invoicing/invoices/"):
			*query = r.URL.RawQuery
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, got)
			_, _ = w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestSetDueDateNeverNotifiesAndKeepsTheRestOfTheDraft(t *testing.T) {
	var body map[string]any
	var query string
	srv := dueDateServer(t, "DRAFT", &body, &query)
	defer srv.Close()
	c, err := NewClient(srv.URL, "id", "secret", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetDueDate(context.Background(), "req-1", "INV2-AAAA-BBBB-CCCC-DDDD", "2026-11-02"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "send_to_recipient=false") || !strings.Contains(query, "send_to_invoicer=false") {
		t.Errorf("query %q: both notifications must be switched off (PayPal's default is true)", query)
	}
	detail := body["detail"].(map[string]any)
	term := detail["payment_term"].(map[string]any)
	if term["term_type"] != "DUE_ON_DATE_SPECIFIED" || term["due_date"] != "2026-11-02" {
		t.Errorf("payment_term %v", term)
	}
	var orig map[string]any
	_ = json.Unmarshal([]byte(draftJSON), &orig)
	for _, k := range []string{"invoicer", "primary_recipients", "items", "amount"} {
		a, _ := json.Marshal(orig[k])
		b, _ := json.Marshal(body[k])
		if string(a) != string(b) {
			t.Errorf("%s changed in the update:\n was %s\n now %s", k, a, b)
		}
	}
	if detail["invoice_number"] != "MC-X" || detail["currency_code"] != "USD" {
		t.Errorf("detail lost fields: %v", detail)
	}
}

func TestSetDueDateRefusesAnythingButADraft(t *testing.T) {
	var body map[string]any
	var query string
	srv := dueDateServer(t, "SENT", &body, &query)
	defer srv.Close()
	c, _ := NewClient(srv.URL, "id", "secret", srv.Client())
	err := c.SetDueDate(context.Background(), "req-1", "INV2-AAAA-BBBB-CCCC-DDDD", "2026-11-02")
	if err == nil || query != "" {
		t.Fatalf("err %v, PUT query %q: an invoice that isn't a draft must never be updated", err, query)
	}
}

func TestSetDueDateRejectsABadDate(t *testing.T) {
	c, _ := NewClient("https://api-m.sandbox.paypal.com", "id", "secret", nil)
	for _, d := range []string{"", "02/11/2026", "2026-13-01", "tomorrow"} {
		if err := c.SetDueDate(context.Background(), "req-1", "INV2-X", d); err == nil {
			t.Errorf("due date %q accepted", d)
		}
	}
}
